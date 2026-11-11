//go:build unix

package host

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritableBy(t *testing.T) {
	for _, c := range []struct {
		o       Owner
		m       fs.FileMode
		link    bool
		trusted []Owner
		want    string
	}{
		{Owner{0, 0}, 0o755, false, nil, ""},
		{Owner{0, 0}, 0o775, false, nil, ""}, // root's group
		{Owner{0, 50}, 0o775, false, nil, "writable by group 50"},
		{Owner{0, 50}, 0o2775, false, nil, "writable by group 50"},
		{Owner{0, 0}, 0o757, false, nil, "writable by everyone"},
		{Owner{0, 0}, 0o1777, false, nil, "writable by everyone"}, // sticky does not help: anyone can add a file
		{Owner{1000, 1000}, 0o755, false, nil, "owned by uid 1000"},
		{Owner{1000, 1000}, 0o755, false, []Owner{{1000, 1000}}, ""},
		{Owner{1000, 1000}, 0o775, false, []Owner{{1000, 1000}}, ""}, // the user's own group
		{Owner{1000, 50}, 0o775, false, []Owner{{1000, 1000}}, "writable by group 50"},
		{Owner{1000, 1000}, 0o777, true, []Owner{{1000, 1000}}, ""}, // a link's mode means nothing
		{Owner{1000, 1000}, 0o777, true, nil, "owned by uid 1000"},
	} {
		if got := writableBy(c.o, c.m, c.link, c.trusted); got != c.want {
			t.Errorf("%v %04o link=%v trusted=%v: %q, want %q", c.o, c.m, c.link, c.trusted, got, c.want)
		}
	}
}

// A tree under the test's dir: Writers names the first path in it that
// someone other than the trusted can change, the file itself or a
// directory above it, through links too.
func TestWriters(t *testing.T) {
	me := Owner{UID: os.Getuid(), GID: os.Getgid()}
	base := t.TempDir()
	dir := filepath.Join(base, "usr", "local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 0755 whatever the umask.
	for d := dir; d != filepath.Dir(base); d = filepath.Dir(d) {
		os.Chmod(d, 0o755)
	}
	bin := filepath.Join(dir, "engine")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What is above base (the system's temp dir) is not this test's.
	inTree := func(w *Writable) bool { return w != nil && strings.HasPrefix(w.Path, base) }
	check := func(name, path string, trusted []Owner, wantPath, wantWhy string, wantFile bool) {
		t.Helper()
		w, err := Writers(path, trusted...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if wantPath == "" {
			if inTree(w) {
				t.Errorf("%s: %v, want nothing in the tree", name, w)
			}
			return
		}
		if w == nil || w.Path != wantPath || w.Why != wantWhy || w.File != wantFile {
			t.Errorf("%s: %+v, want %s %q file=%v", name, w, wantPath, wantWhy, wantFile)
		}
	}

	check("trusted owner, 0755", bin, []Owner{me}, "", "", false)
	if me.UID != 0 {
		check("another owner", bin, nil, bin, fmt.Sprintf("owned by uid %d", me.UID), true)
	}

	os.Chmod(bin, 0o777)
	check("world-writable file", bin, []Owner{me}, bin, "writable by everyone", true)
	os.Chmod(bin, 0o755)

	os.Chmod(filepath.Dir(dir), 0o777)
	check("a world-writable dir above", bin, []Owner{me}, filepath.Dir(dir), "writable by everyone", false)
	check("a missing file, checked from its dir", filepath.Join(dir, "nope", "visor"), []Owner{me}, filepath.Dir(dir), "writable by everyone", false)
	os.Chmod(filepath.Dir(dir), 0o755)

	// A link in a safe dir to a binary under a writable one.
	other := filepath.Join(base, "opt", "x")
	os.MkdirAll(other, 0o755)
	os.Chmod(filepath.Join(base, "opt"), 0o777)
	real := filepath.Join(other, "engine")
	os.WriteFile(real, []byte("x"), 0o755)
	link := filepath.Join(dir, "engine-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	check("a link to a binary under a writable dir", link, []Owner{me}, filepath.Join(base, "opt"), "writable by everyone", false)
	os.Chmod(filepath.Join(base, "opt"), 0o755)
	check("a link, all safe", link, []Owner{me}, "", "", false)

	if _, err := Writers("relative/path"); err == nil {
		t.Error("a relative path: want an error")
	}
}
