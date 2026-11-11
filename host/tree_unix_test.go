//go:build unix

package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// otherGroup is a group of this user's other than its primary one: what a
// test owns its files to without root.
func otherGroup(t *testing.T) int {
	gs, _ := os.Getgroups()
	for _, g := range gs {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("this user is in one group only: nothing to give its files to")
	return 0
}

func gidOf(t *testing.T, p string) int {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Gid)
}

// The tree is given to the owner, links themselves but never what they
// point at: a link to a file outside (an app's trap: /etc/shadow) and a
// link to a directory outside are not followed.
func TestOwnTree(t *testing.T) {
	g := otherGroup(t)
	base := t.TempDir()
	tree, outside := filepath.Join(base, "cache"), filepath.Join(base, "etc")
	for _, d := range []string{tree + "/apps/a/venv", outside + "/sub"} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(tree+"/apps/a/venv/python", []byte("x"), 0o755)
	os.WriteFile(outside+"/shadow", []byte("secret"), 0o600)
	os.WriteFile(outside+"/sub/f", []byte("x"), 0o600)
	os.Symlink(outside+"/shadow", tree+"/apps/a/shadow")
	os.Symlink(outside, tree+"/apps/a/etc")
	os.Symlink("../../../etc/sub", tree+"/apps/a/rel")

	ot := &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}
	r := converge(t, ot, Changed)
	t.Log(r.Msg)
	for _, p := range []string{tree, tree + "/apps/a", tree + "/apps/a/venv/python", tree + "/apps/a/shadow", tree + "/apps/a/etc"} {
		if gidOf(t, p) != g {
			t.Errorf("%s: gid %d, want %d", p, gidOf(t, p), g)
		}
	}
	for _, p := range []string{outside, outside + "/shadow", outside + "/sub", outside + "/sub/f"} {
		if gidOf(t, p) == g {
			t.Errorf("%s, outside the tree, was changed", p)
		}
	}
}

// A file with another hard link is left unless the kernel protects hard
// links: it may be a link to a file outside the tree.
func TestOwnTreeHardlinks(t *testing.T) {
	g := otherGroup(t)
	base := t.TempDir()
	tree := filepath.Join(base, "cache")
	os.MkdirAll(tree, 0o755)
	os.WriteFile(base+"/outside", []byte("x"), 0o600)
	if err := os.Link(base+"/outside", tree+"/linked"); err != nil {
		t.Skip(err)
	}
	ot := &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}
	apply(t, ot)
	if gidOf(t, base+"/outside") == g {
		t.Fatal("a hard-linked file was changed without protected_hardlinks")
	}
	ot.Hardlinks = true
	apply(t, ot)
	if gidOf(t, tree+"/linked") != g {
		t.Fatal("protected_hardlinks: the linked file was left")
	}
}

// A tree that is a link is refused.
func TestOwnTreeRefusesALink(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(base+"/real", 0o755)
	os.Symlink(base+"/real", base+"/cache")
	ot := &OwnTree{Path: base + "/cache", Owner: Owner{UID: os.Getuid(), GID: os.Getgid()}}
	if _, err := ot.Check(t.Context()); err == nil {
		t.Fatal("a link was taken as the tree")
	}
}

// A walk cut short is done again: Check still finds what is left, and the
// next Apply skips what the first changed.
func TestOwnTreeResumes(t *testing.T) {
	g := otherGroup(t)
	tree := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(tree+"/a/b", 0o755)
	os.WriteFile(tree+"/a/b/f", []byte("x"), 0o644)
	os.WriteFile(tree+"/top", []byte("x"), 0o644)
	// The first walk got through a/ before it was cut short.
	for _, p := range []string{tree + "/a/b/f", tree + "/a/b", tree + "/a"} {
		if err := os.Lchown(p, os.Getuid(), g); err != nil {
			t.Fatal(err)
		}
	}
	ot := &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}
	st, err := ot.Check(t.Context())
	if err != nil || st.InPlace {
		t.Fatalf("a tree half done is in place: %+v %v", st, err)
	}
	r := converge(t, ot, Changed)
	if !strings.HasPrefix(r.Msg, "2 of 5 paths") {
		t.Errorf("msg %q, want the 2 left (top and the tree) of 5", r.Msg)
	}
	if gidOf(t, tree+"/top") != g || gidOf(t, tree) != g {
		t.Error("the rest was not changed")
	}
}

// Apply says how far it is while it walks: files so far and the rate,
// never a share of a filesystem the tree only lives on.
func TestOwnTreeProgress(t *testing.T) {
	g := otherGroup(t)
	tree := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(tree, 0o755)
	for i := range 2000 {
		os.WriteFile(filepath.Join(tree, "f"+strconv.Itoa(i)), nil, 0o644)
	}
	defer func(d time.Duration) { progressEvery = d }(progressEvery)
	progressEvery = time.Millisecond
	var mu sync.Mutex
	var lines []string
	r := Runner{OnProgress: func(s string) { mu.Lock(); lines = append(lines, s); mu.Unlock() }}
	if err := r.Run(t.Context(), Play{Tasks: []Task{{Resource: &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}}}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		t.Skip("the walk ended before the first report")
	}
	for _, l := range lines {
		if !strings.Contains(l, " files") || strings.Contains(l, "%") {
			t.Errorf("progress %q: files, and no share of a shared filesystem", l)
		}
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1240000: "1,240,000"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

// A directory that is not a filesystem's root has no total: a share of a
// filesystem others use would be a guess.
func TestMountInodesNotAMount(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	if n := mountInodes(fd, &st, dir); n != 0 {
		t.Errorf("a temp dir's total %d, want 0", n)
	}
}

// Deep in the tree, through the workers: links not followed, hard links
// left, and a directory that cannot be read fails the walk without
// leaving it hanging.
func TestOwnTreeWorkers(t *testing.T) {
	g := otherGroup(t)
	base := t.TempDir()
	tree := filepath.Join(base, "cache")
	for i := range 20 {
		os.MkdirAll(filepath.Join(tree, "apps", "a"+strconv.Itoa(i), "venv", "lib"), 0o755)
		os.WriteFile(filepath.Join(tree, "apps", "a"+strconv.Itoa(i), "venv", "lib", "f"), []byte("x"), 0o644)
	}
	os.WriteFile(base+"/outside", []byte("x"), 0o600)
	os.Symlink(base+"/outside", tree+"/apps/a3/venv/lib/out")
	if err := os.Link(base+"/outside", tree+"/apps/a5/venv/linked"); err != nil {
		t.Skip(err)
	}
	ot := &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}
	converge(t, ot, Changed)
	if gidOf(t, tree+"/apps/a19/venv/lib/f") != g || gidOf(t, tree+"/apps/a3/venv/lib/out") != g {
		t.Error("the tree was not changed through the workers")
	}
	if gidOf(t, base+"/outside") == g {
		t.Error("a link or hard link reached outside the tree")
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	g2 := os.Getgid()
	locked := tree + "/apps/a7/venv"
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o755)
	done := make(chan error, 1)
	go func() {
		_, err := (&OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g2}}).Apply(t.Context(), State{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "a7/venv") {
			t.Errorf("an unreadable directory: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk hung on a failure")
	}
}

// A chown -R cut short (it changes a directory before what is in it)
// leaves the top the owner's and the rest not: found, with a marker or
// without, and finished; then the marker makes the next Check cheap, and
// a recreated tree does not match it.
func TestOwnTreePreorderCut(t *testing.T) {
	g := otherGroup(t)
	base := t.TempDir()
	tree := filepath.Join(base, "cache")
	os.MkdirAll(tree+"/inferencesh/apps/a/venv", 0o755)
	os.WriteFile(tree+"/inferencesh/apps/a/venv/python", []byte("x"), 0o755)
	cut := func() {
		for _, p := range []string{tree, tree + "/inferencesh", tree + "/inferencesh/apps"} {
			if err := os.Lchown(p, os.Getuid(), g); err != nil {
				t.Fatal(err)
			}
		}
	}
	cut()
	marker := filepath.Join(base, "state", "cache-handed-over.json")
	os.MkdirAll(filepath.Dir(marker), 0o700)
	for _, ot := range []*OwnTree{
		{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}},
		{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}, Marker: marker},
	} {
		st, err := ot.Check(t.Context())
		if err != nil || st.InPlace {
			t.Fatalf("marker %q: a cut chown is in place: %+v %v", ot.Marker, st, err)
		}
	}
	ot := &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}, Marker: marker}
	converge(t, ot, Changed)
	if gidOf(t, tree+"/inferencesh/apps/a/venv/python") != g || gidOf(t, tree+"/inferencesh/apps/a") != g {
		t.Fatal("the subtree under the owned top was not finished")
	}

	// The tree made again: the marker is for another directory. The old
	// one is kept aside, so the new one cannot reuse its inode number.
	if err := os.Rename(tree, tree+".old"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(tree+"/inferencesh/apps/b", 0o755)
	os.WriteFile(tree+"/inferencesh/apps/b/f", []byte("x"), 0o644)
	cut()
	if st, err := ot.Check(t.Context()); err != nil || st.InPlace {
		t.Fatalf("a recreated tree matched the marker: %+v %v", st, err)
	}
	converge(t, ot, Changed)
	if gidOf(t, tree+"/inferencesh/apps/b/f") != g {
		t.Fatal("the recreated tree was not finished")
	}
}

// A directory larger than one read of its entries, and subdirectories
// found by d_type: every path is counted once and changed.
func TestOwnTreeBigDir(t *testing.T) {
	g := otherGroup(t)
	tree := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(tree, 0o755)
	for i := range 3000 {
		os.WriteFile(filepath.Join(tree, "file-"+strconv.Itoa(i)), nil, 0o644)
	}
	for i := range 50 {
		os.MkdirAll(filepath.Join(tree, "dir-"+strconv.Itoa(i), "sub"), 0o755)
	}
	r := converge(t, &OwnTree{Path: tree, Owner: Owner{UID: os.Getuid(), GID: g}}, Changed)
	if want := "3101 of 3101 paths"; !strings.HasPrefix(r.Msg, want) {
		t.Errorf("msg %q, want %q (3000 files, 100 directories, the tree)", r.Msg, want)
	}
	if gidOf(t, tree+"/file-2999") != g || gidOf(t, tree+"/dir-49/sub") != g {
		t.Error("not everything was changed")
	}
}
