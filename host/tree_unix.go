//go:build unix

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// OwnTree is a directory and everything in it owned by Owner: what an
// engine of another user leaves (its cache) handed to the engine's user.
//
// It walks the tree by directory descriptor: each directory is opened
// from its parent's with O_NOFOLLOW|O_DIRECTORY, and each name is read
// and changed relative to that descriptor (fstatat, fchownat with
// AT_SYMLINK_NOFOLLOW). So no link is ever followed, and no name or
// directory swapped in while it walks reaches outside it; links are
// changed themselves (lchown). A file with more than one hard link may be
// a link to a file outside the tree, and is left as it is unless
// Hardlinks (the kernel's protected_hardlinks, which stops a user linking
// to a file they do not own).
//
// Something else may have changed owners part-way (a chown -R cut short
// changes a directory before what is in it), so the tree being the
// owner's at its top proves nothing about the rest. When Apply has been
// through all of it, it records that at Marker (in setup's state
// directory, root's): the path, the owner, and the tree directory's
// device and inode, so another or a recreated tree does not match. Check
// is in place only when the marker matches; otherwise Apply walks the
// whole tree again, which costs an fstatat for what is the owner's
// already. Without a Marker, Check reads the whole tree, up to the first
// path that is not the owner's.
type OwnTree struct {
	Path      string
	Owner     Owner
	Hardlinks bool
	// Marker is where Apply records the tree done; "" records nothing.
	Marker string
}

func (t *OwnTree) ID() string { return "tree " + t.Path + " owned by " + t.Owner.String() }

// progressEvery is how often Apply says how far it is.
var progressEvery = time.Second

// treeMarker is what Apply records once the tree is the owner's.
type treeMarker struct {
	Path string `json:"path"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

func (t *OwnTree) marker(st *unix.Stat_t) treeMarker {
	return treeMarker{Path: t.Path, UID: t.Owner.UID, GID: t.Owner.GID, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}
}

func (t *OwnTree) Check(ctx context.Context) (State, error) {
	fd, st, err := t.open()
	if err != nil {
		return State{}, err
	}
	defer unix.Close(fd)
	if !t.owned(&st) {
		return drift("%s is not %s's", t.Path, t.Owner)
	}
	if t.Marker != "" {
		var m treeMarker
		if b, err := os.ReadFile(t.Marker); err == nil && json.Unmarshal(b, &m) == nil && m == t.marker(&st) {
			return inPlace()
		}
		return drift("not handed over whole yet")
	}
	w := &treeWalk{t: t, ctx: ctx, dry: true}
	switch err := w.run(fd, &st); {
	case errors.Is(err, errNotOwned):
		return drift("paths in it are not %s's", t.Owner)
	case err != nil:
		return State{}, err
	}
	return inPlace()
}

func (t *OwnTree) Apply(ctx context.Context, _ State) (Result, error) {
	fd, st, err := t.open()
	if err != nil {
		return Result{}, err
	}
	defer unix.Close(fd)
	w := &treeWalk{t: t, ctx: ctx, start: time.Now(), total: mountInodes(fd, &st, t.Path)}
	stop := w.report()
	err = w.run(fd, &st)
	stop()
	if err == nil {
		// A chown changes neither the device nor the inode: st is still
		// the tree's directory.
		err = t.record(&st)
	}
	if err != nil {
		return Result{}, err
	}
	msg := fmt.Sprintf("%d of %d paths now %s's", w.changed.Load(), w.seen.Load(), t.Owner)
	if n := w.linked.Load(); n > 0 {
		msg += fmt.Sprintf("; %d hard-linked files left as they are (protected_hardlinks is off)", n)
	}
	return changed("%s", msg)
}

// record writes the marker for the tree's directory (st), all of it now
// the owner's.
func (t *OwnTree) record(st *unix.Stat_t) error {
	if t.Marker == "" {
		return nil
	}
	b, err := json.Marshal(t.marker(st))
	if err != nil {
		return err
	}
	if err := writeAtomic(t.Marker, bytes.NewReader(b), 0o600, nil); err != nil {
		return fmt.Errorf("the handover's record %s: %w", t.Marker, err)
	}
	return nil
}

// errNotOwned ends a dry walk (Check's) at the first path not the
// owner's.
var errNotOwned = errors.New("not the owner's")

// open opens the tree's directory, refusing a link, and stats it.
func (t *OwnTree) open() (int, unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Open(t.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if fi, lerr := os.Lstat(t.Path); lerr == nil && !fi.IsDir() {
			return -1, st, fmt.Errorf("%s is %s, not a directory", t.Path, kind(fi))
		}
		return -1, st, fmt.Errorf("%s: %w", t.Path, err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, st, fmt.Errorf("%s: %w", t.Path, err)
	}
	return fd, st, nil
}

func (t *OwnTree) owned(st *unix.Stat_t) bool {
	return t.Owner.has(Owner{UID: int(st.Uid), GID: int(st.Gid)})
}

// leave: a file with another hard link, without protected_hardlinks.
func (t *OwnTree) leave(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink > 1 && !t.Hardlinks
}

// treeWorkers is how many directories a walk reads and changes at once:
// a cache of millions of files on a RAID volume is bound by the disks'
// latency, not one core.
var treeWorkers = 8

// treeWalk is one walk: a pool of workers taking directories, each opened
// from its parent's descriptor (O_NOFOLLOW|O_DIRECTORY), changed by its
// own descriptor, its other entries by name relative to it (fchownat
// AT_SYMLINK_NOFOLLOW). The order does not matter: only Apply's record
// says the tree is whole.
type treeWalk struct {
	t     *OwnTree
	ctx   context.Context
	start time.Time
	// seen counts the paths read so far, for the progress line; total is
	// the tree's paths when it is a filesystem of its own (0 when not
	// known). Workers add to them a directory at a time.
	seen, changed, linked atomic.Int64
	total                 int64
	// dry: change nothing, and end at the first path not the owner's
	// (errNotOwned).
	dry bool
}

// counts are a worker's own tallies for one directory, added to the
// walk's when it is done.
type counts struct{ seen, changed, linked int64 }

func (w *treeWalk) add(c counts) {
	w.seen.Add(c.seen)
	w.changed.Add(c.changed)
	w.linked.Add(c.linked)
}

// dirRef is an open directory and the jobs that still need it: its
// subdirectories, opened from it, and its own reading. The last to let go
// closes it; owned: the caller's, never closed here.
type dirRef struct {
	fd    int
	path  string
	refs  atomic.Int32
	owned bool
}

func (d *dirRef) release() {
	if d.refs.Add(-1) == 0 && !d.owned {
		unix.Close(d.fd)
	}
}

// dirJob is a directory to open (name, in parent) and change; for the
// tree's own directory, root is set and already open.
type dirJob struct {
	parent *dirRef
	name   string
	root   *dirRef
	rootSt *unix.Stat_t
}

// run changes the tree whose directory is fd (st), with treeWorkers
// workers.
func (w *treeWalk) run(fd int, st *unix.Stat_t) error {
	q := &dirQueue{}
	q.cond = sync.NewCond(&q.mu)
	root := &dirRef{fd: fd, path: w.t.Path, owned: true}
	root.refs.Store(1)
	q.push(dirJob{root: root, rootSt: st})
	var wg sync.WaitGroup
	for range treeWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for {
				j, ok := q.pop()
				if !ok {
					return
				}
				q.done(w.dir(q, j, buf))
			}
		}()
	}
	wg.Wait()
	for _, j := range q.stack { // left by a failure
		j.parent.release()
	}
	if q.err == nil {
		q.err = w.ctx.Err()
	}
	return q.err
}

// dir opens j's directory from its parent's, changes it and what is in
// it, and queues its subdirectories. buf is the worker's for reading
// directory entries.
func (w *treeWalk) dir(q *dirQueue, j dirJob, buf []byte) error {
	if err := w.ctx.Err(); err != nil {
		if j.parent != nil {
			j.parent.release()
		}
		return err
	}
	var c counts
	defer func() { w.add(c) }()
	d, st := j.root, j.rootSt
	if d == nil {
		path := j.parent.path + "/" + j.name
		fd, err := unix.Openat(j.parent.fd, j.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		j.parent.release()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		d = &dirRef{fd: fd, path: path}
		d.refs.Store(1)
		st = new(unix.Stat_t)
		if err := unix.Fstat(fd, st); err != nil {
			d.release()
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	defer d.release() // this reading
	c.seen++
	if err := w.self(d, st, &c); err != nil {
		return err
	}
	sub := func(name string) {
		d.refs.Add(1)
		q.push(dirJob{parent: d, name: name})
	}
	// A subdirectory counts itself, once open: its own Fstat is what
	// counts, d_type only says to open it.
	return eachEntry(d.fd, buf, func(name string, typ uint8) error {
		if typ == unix.DT_DIR {
			sub(name)
			return nil
		}
		var est unix.Stat_t
		if err := unix.Fstatat(d.fd, name, &est, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("%s/%s: %w", d.path, name, err)
		}
		if est.Mode&unix.S_IFMT == unix.S_IFDIR { // d_type unknown
			sub(name)
			return nil
		}
		c.seen++
		return w.entry(d, name, &est, &c)
	})
}

// self changes the directory d itself (st), unless it is the owner's.
func (w *treeWalk) self(d *dirRef, st *unix.Stat_t, c *counts) error {
	if w.t.owned(st) {
		return nil
	}
	if w.dry {
		return errNotOwned
	}
	if err := unix.Fchown(d.fd, w.t.Owner.UID, w.t.Owner.GID); err != nil {
		return fmt.Errorf("%s: %w", d.path, err)
	}
	c.changed++
	return nil
}

// entry changes name in the directory d, which st describes, unless it is
// the owner's already or a hard-linked file to leave.
func (w *treeWalk) entry(d *dirRef, name string, st *unix.Stat_t, c *counts) error {
	switch {
	case w.t.owned(st):
		return nil
	case w.t.leave(st):
		c.linked++
		return nil
	case w.dry:
		return errNotOwned
	}
	if err := unix.Fchownat(d.fd, name, w.t.Owner.UID, w.t.Owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%s/%s: %w", d.path, name, err)
	}
	c.changed++
	return nil
}

// dirQueue is the directories the workers take, last in first out:
// deepest first, so few directories wait open.
type dirQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	stack   []dirJob
	pending int // queued or being worked on
	err     error
}

func (q *dirQueue) push(j dirJob) {
	q.mu.Lock()
	q.stack = append(q.stack, j)
	q.pending++
	q.mu.Unlock()
	q.cond.Signal()
}

// pop takes a job, waiting while others may still queue one; false when
// the walk is done or failed.
func (q *dirQueue) pop() (dirJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.stack) == 0 && q.pending > 0 && q.err == nil {
		q.cond.Wait()
	}
	if q.err != nil || len(q.stack) == 0 {
		return dirJob{}, false
	}
	j := q.stack[len(q.stack)-1]
	q.stack = q.stack[:len(q.stack)-1]
	return j, true
}

// done ends a job, with its error.
func (q *dirQueue) done(err error) {
	q.mu.Lock()
	q.pending--
	if err != nil && q.err == nil {
		q.err = err
	}
	end := q.pending == 0 || q.err != nil
	q.mu.Unlock()
	if end {
		q.cond.Broadcast()
	}
}

// report says how far the walk is every progressEvery, until stop.
func (w *treeWalk) report() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				Progress(w.ctx, "%s", w.line())
			}
		}
	}()
	return func() { close(done); <-finished }
}

// line is the progress line: paths so far, the rate, and a share when
// the total is known ("1,240,000 files, 85,000/s, ≈ 41%").
func (w *treeWalk) line() string {
	n := w.seen.Load()
	s := commas(n) + " files"
	if d := time.Since(w.start).Seconds(); d >= 1 {
		s += ", " + commas(int64(float64(n)/d)) + "/s"
	}
	if w.total > 0 {
		s += fmt.Sprintf(", ≈ %d%%", min(99, n*100/w.total))
	}
	return s
}

// mountInodes is the inodes in use on the filesystem whose root is the
// directory fd (st, at path): the tree's paths, when the tree is a
// filesystem of its own. 0 when it is not, or not known: a share of a
// filesystem others use would be a guess.
func mountInodes(fd int, st *unix.Stat_t, path string) int64 {
	var parent unix.Stat_t
	if unix.Lstat(filepath.Dir(path), &parent) != nil || st.Dev == parent.Dev {
		return 0
	}
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil || fs.Files == 0 || fs.Files < uint64(fs.Ffree) {
		return 0
	}
	return int64(fs.Files - uint64(fs.Ffree))
}

// commas writes n with thousands separators.
func commas(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ProtectedHardlinks reports whether the kernel at root (the host's /)
// protects hard links.
func ProtectedHardlinks(root string) bool {
	b, err := os.ReadFile(UnderRoot(root, "proc/sys/fs/protected_hardlinks"))
	return err == nil && strings.TrimSpace(string(b)) == "1"
}
