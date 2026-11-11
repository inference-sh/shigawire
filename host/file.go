package host

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UnderRoot is the host path p under root, the directory the host's
// files are in as this process sees them; "" is /, the host itself.
func UnderRoot(root, p string) string { return filepath.Join(cmp.Or(root, "/"), p) }

// ReadKV reads a shell-style KEY=value file (/etc/os-release,
// /etc/default/ufw): parseKV of its content.
func ReadKV(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseKV(b), nil
}

// parseKV reads KEY=value lines, each value's surrounding quotes dropped;
// a blank line or a # comment is skipped.
func parseKV(b []byte) map[string]string {
	kv := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return kv
}

// File is a regular file with its content, mode and owner. A file that
// differs is replaced atomically (written beside it, renamed over it), so
// a reader sees the old content or all of the new. One that differs only
// in mode or owner is replaced too, never changed in place: whoever could
// write to it before (its old owner) and opened it then could still write
// through that descriptor after a chown or chmod.
type File struct {
	Path string
	// Content is what the file holds; From, when set, is a local file to
	// copy instead (a binary, compared by hash).
	Content []byte
	From    string
	// Seed: an existing regular file's content is kept, only its mode and
	// owner put right. What setup puts there once and the program then
	// owns (a binary that updates itself, a config it rewrites).
	Seed bool
	// Mode is the permission bits; 0 leaves an existing file's and makes a
	// new one 0644. Owner, when set, owns the file; nil leaves an existing
	// file's owner and gives a new one the writer's.
	Mode  fs.FileMode
	Owner *Owner
	// Parents: missing parent directories are made (0755, the writer's).
	// Without it a missing parent is an error.
	Parents bool
}

func (f *File) ID() string { return "file " + f.Path }

func (f *File) attrs() attrs { return attrs{f.Mode, f.Owner} }

// fileDrift is what File.Check found: whether the content is to be
// written, and the file there (nil: none).
type fileDrift struct {
	content bool
	fi      fs.FileInfo
}

func (f *File) Check(context.Context) (State, error) {
	fi, err := os.Lstat(f.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return driftData(fileDrift{content: true}, "create")
	case err != nil:
		return State{}, err
	case !fi.Mode().IsRegular():
		return driftData(fileDrift{content: true, fi: fi}, "%s, want a file", kind(fi))
	}
	var d []string
	same, err := f.sameContent(fi)
	if err != nil {
		return State{}, err
	}
	if !same {
		d = append(d, "content")
	}
	d = append(d, f.attrs().drift(fi)...)
	if len(d) == 0 {
		return inPlace()
	}
	return driftData(fileDrift{content: !same, fi: fi}, "%s", strings.Join(d, "; "))
}

func (f *File) sameContent(fi fs.FileInfo) (bool, error) {
	if f.Seed {
		return true, nil
	}
	if f.From != "" {
		return SameContent(f.From, f.Path)
	}
	if fi.Size() != int64(len(f.Content)) {
		return false, nil
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	return sameStream(fh, bytes.NewReader(f.Content))
}

func (f *File) Apply(ctx context.Context, st State) (Result, error) {
	d := st.Data.(fileDrift)
	if f.Parents {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return Result{}, err
		}
	}
	if err := saveBefore(ctx, f.Path); err != nil {
		return Result{}, err
	}
	// Content or From, whether or not the bytes differ: a file that
	// differs only in its mode or owner is written again, as a new file.
	var r io.Reader = bytes.NewReader(f.Content)
	from := f.From
	if f.Seed && d.fi != nil && d.fi.Mode().IsRegular() {
		// Its own content, written again with the mode and owner.
		from = f.Path
	}
	if from != "" {
		flag := os.O_RDONLY
		if from == f.Path {
			flag |= oNoFollow
		}
		src, err := os.OpenFile(from, flag, 0)
		if err != nil {
			return Result{}, err
		}
		defer src.Close()
		r = src
	}
	mode, owner := f.attrs().forNew(d.fi, 0o644)
	if err := writeAtomic(f.Path, r, mode, owner); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// Dir is a directory with its mode and owner. A missing one is made with
// its missing parents (the parents 0755, the writer's).
type Dir struct {
	Path string
	// Mode is the permission bits; 0 leaves an existing directory's and
	// makes a new one 0755.
	Mode  fs.FileMode
	Owner *Owner
}

func (d *Dir) ID() string { return "dir " + d.Path }

func (d *Dir) attrs() attrs { return attrs{d.Mode, d.Owner} }

func (d *Dir) Check(context.Context) (State, error) {
	fi, err := os.Lstat(d.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return driftData(true, "create")
	case err != nil:
		return State{}, err
	case !fi.IsDir():
		return State{}, fmt.Errorf("%s is %s, not a directory", d.Path, kind(fi))
	}
	if dd := d.attrs().drift(fi); len(dd) > 0 {
		return drift("%s", strings.Join(dd, "; "))
	}
	return inPlace()
}

func (d *Dir) Apply(_ context.Context, st State) (Result, error) {
	a := d.attrs()
	if create, _ := st.Data.(bool); create {
		if a.mode == 0 {
			a.mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(d.Path), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.Mkdir(d.Path, a.mode); err != nil {
			return Result{}, err
		}
		// set chmods it again: Mkdir's mode went through the umask.
	}
	if err := a.set(d.Path); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// Absent is a path that does not exist: a file, a link or an empty
// directory, removed. A directory with something in it is an error,
// unless Recursive: then it goes with all in it.
type Absent struct {
	Path      string
	Recursive bool
}

func (a *Absent) ID() string { return "absent " + a.Path }

func (a *Absent) Check(context.Context) (State, error) {
	fi, err := os.Lstat(a.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return inPlace()
	case err != nil:
		return State{}, err
	}
	return drift("remove the %s", strings.TrimPrefix(kind(fi), "a "))
}

func (a *Absent) Apply(context.Context, State) (Result, error) {
	remove := os.Remove
	if a.Recursive {
		remove = os.RemoveAll
	}
	if err := remove(a.Path); err != nil {
		return Result{}, err
	}
	return changed("removed")
}

// Symlink is a link at Path to Target. A link elsewhere is replaced
// atomically; a real file or directory at Path is moved to Aside first,
// and is an error when Aside is "" or something is there already. With
// Force and no Aside, a regular file at Path is replaced by the link (ln
// -sfn); a directory is still an error.
type Symlink struct {
	Path, Target string
	Aside        string
	Force        bool
	// Owner, when set, owns the link itself.
	Owner *Owner
}

func (s *Symlink) ID() string { return "link " + s.Path + " -> " + s.Target }

func (s *Symlink) attrs() attrs { return attrs{owner: s.Owner} }

// Check's Data is whether a real file or directory at Path is to be moved
// aside.
func (s *Symlink) Check(context.Context) (State, error) {
	fi, err := os.Lstat(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return driftData(false, "create")
	case err != nil:
		return State{}, err
	case fi.Mode()&fs.ModeSymlink == 0 && s.Aside == "" && s.Force && fi.Mode().IsRegular():
		return driftData(false, "a file, replaced")
	case fi.Mode()&fs.ModeSymlink == 0:
		if s.Aside == "" {
			return State{}, fmt.Errorf("%s is %s, not a link, and there is nowhere to move it aside", s.Path, kind(fi))
		}
		if _, err := os.Lstat(s.Aside); !errors.Is(err, fs.ErrNotExist) {
			return State{}, fmt.Errorf("%s is %s, not a link, and %s, where it would move aside, exists", s.Path, kind(fi), s.Aside)
		}
		return driftData(true, "%s, moved to %s", kind(fi), s.Aside)
	}
	if got, err := os.Readlink(s.Path); err != nil {
		return State{}, err
	} else if got != s.Target {
		return driftData(false, "points at %s", got)
	}
	if d := s.attrs().drift(fi); len(d) > 0 {
		return driftData(false, "%s", strings.Join(d, "; "))
	}
	return inPlace()
}

func (s *Symlink) Apply(_ context.Context, st State) (Result, error) {
	if aside, _ := st.Data.(bool); aside {
		if err := os.Rename(s.Path, s.Aside); err != nil {
			return Result{}, err
		}
	}
	tmp := filepath.Join(filepath.Dir(s.Path), "."+filepath.Base(s.Path)+".link.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(s.Target, tmp); err != nil {
		return Result{}, err
	}
	if err := s.attrs().set(tmp); err != nil {
		os.Remove(tmp)
		return Result{}, err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// writeAtomic replaces path with what r holds, with mode, owned by owner
// when set (else the writer's): written to a temp file beside it, synced,
// then renamed over it, so a reader sees the old content or all of the
// new, and a link at path is replaced, not followed. The owner and mode
// are set before the rename: the file is never in place with others.
func writeAtomic(path string, r io.Reader, mode fs.FileMode, owner *Owner) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	// Chmod: CreateTemp's mode went through the umask.
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if owner != nil {
		if err := tmp.Chown(owner.UID, owner.GID); err != nil {
			return err
		}
	}
	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	ok = true
	return nil
}

// SameContent reports whether the files at a and b hold the same bytes:
// sizes first, then the bytes.
func SameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil || sa.Size() != sb.Size() {
		return false, err
	}
	return sameStream(fa, fb)
}

// sameStream reports whether a and b read the same bytes, reading both a
// buffer at a time and stopping at the first difference.
func sameStream(a, b io.Reader) (bool, error) {
	ba, bb := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		na, errA := io.ReadFull(a, ba)
		nb, errB := io.ReadFull(b, bb)
		if !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		endA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		endB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		switch {
		case errA != nil && !endA:
			return false, errA
		case errB != nil && !endB:
			return false, errB
		case endA || endB:
			return endA && endB, nil
		}
	}
}

// fileSum is the hex sha256 of the file at path.
func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// kind is what fi is, for messages ("a directory").
func kind(fi fs.FileInfo) string {
	switch m := fi.Mode(); {
	case m.IsRegular():
		return "a file"
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a link"
	}
	return "a special file"
}
