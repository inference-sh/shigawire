package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func self() *Owner { return &Owner{UID: os.Getuid(), GID: os.Getgid()} }

func TestFile_CreatesThenIsInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.yml")
	f := &File{Path: path, Content: []byte("api_key: x\n"), Mode: 0o600, Owner: self()}
	if r := converge(t, f, Changed); r.Msg != "create" {
		t.Fatalf("msg %q", r.Msg)
	}
	if b, _ := os.ReadFile(path); string(b) != "api_key: x\n" {
		t.Fatalf("content %q", b)
	}
	if m := perm(t, path); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
}

// A file with the right content and the wrong mode is written again, as a
// new file: one opened while its mode let others write keeps letting them
// through that descriptor after a chmod. The message never holds the
// content.
func TestFile_ModeOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("secret"), 0o644)
	before, _ := os.Stat(path)
	r := converge(t, &File{Path: path, Content: []byte("secret"), Mode: 0o600}, Changed)
	if r.Msg != "mode 0644, want 0600" {
		t.Fatalf("msg %q", r.Msg)
	}
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Fatal("the file was chmod'ed in place, want a new file")
	}
	if b, _ := os.ReadFile(path); string(b) != "secret" {
		t.Fatalf("content %q", b)
	}
	if m := perm(t, path); m != 0o600 {
		t.Fatalf("mode %o", m)
	}
}

func TestFile_NewContentReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unit")
	os.WriteFile(path, []byte("old"), 0o644)
	r := converge(t, &File{Path: path, Content: []byte("new")}, Changed)
	if strings.Contains(r.Msg, "new") || r.Msg != "content" {
		t.Fatalf("msg %q", r.Msg)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("left %v", ents)
	}
}

// A link at the path is replaced, not followed.
func TestFile_ReplacesALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	os.WriteFile(target, []byte("keep"), 0o644)
	path := filepath.Join(dir, "f")
	os.Symlink(target, path)
	r := converge(t, &File{Path: path, Content: []byte("x")}, Changed)
	if r.Msg != "a link, want a file" {
		t.Fatalf("msg %q", r.Msg)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("wrote through the link")
	}
}

func TestFile_From(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "visor"), filepath.Join(dir, "boxd-visor")
	os.WriteFile(src, []byte("v1"), 0o700)
	f := &File{Path: dst, From: src, Mode: 0o755}
	converge(t, f, Changed)
	os.WriteFile(src, []byte("v2"), 0o700)
	if r := converge(t, f, Changed); r.Msg != "content" {
		t.Fatalf("a new visor: %q", r.Msg)
	}
	if b, _ := os.ReadFile(dst); string(b) != "v2" || perm(t, dst) != 0o755 {
		t.Fatalf("content %q mode %o", b, perm(t, dst))
	}
}

func TestFile_OwnerDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, nil, 0o644)
	other := &Owner{UID: os.Getuid() + 1, GID: os.Getgid()}
	st, err := (&File{Path: path, Owner: other}).Check(context.Background())
	if err != nil || st.InPlace || !strings.HasPrefix(st.Drift, "owner ") {
		t.Fatalf("state %+v, %v", st, err)
	}
	if st, _ := (&File{Path: path, Owner: self()}).Check(context.Background()); !st.InPlace {
		t.Fatalf("own file: %+v", st)
	}
}

func TestDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".config", "inference")
	d := &Dir{Path: path, Mode: 0o700, Owner: self()}
	converge(t, d, Changed)
	if m := perm(t, path); m != 0o700 {
		t.Fatalf("mode %o", m)
	}
	os.Chmod(path, 0o755)
	if r := converge(t, d, Changed); r.Msg != "mode 0755, want 0700" {
		t.Fatalf("msg %q", r.Msg)
	}
	// Mode 0 leaves an existing directory's.
	if st, _ := (&Dir{Path: path}).Check(context.Background()); !st.InPlace {
		t.Fatalf("mode 0: %+v", st)
	}
	// The missing parents are 0755, whatever the directory's mode.
	if m := perm(t, filepath.Dir(path)); m != 0o755 {
		t.Fatalf("parent mode %o", m)
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := (&Dir{Path: file}).Check(context.Background()); err == nil {
		t.Fatal("a file passed for a directory")
	}
}

func TestAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker.list")
	os.WriteFile(path, nil, 0o644)
	if r := converge(t, &Absent{Path: path}, Changed); r.Msg != "removed" {
		t.Fatalf("msg %q", r.Msg)
	}
	if ok, _ := exists(path); ok {
		t.Fatal("still there")
	}
	full := filepath.Join(dir, "full")
	os.MkdirAll(filepath.Join(full, "x"), 0o755)
	if _, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: &Absent{Path: full}}}}); err == nil {
		t.Fatal("removed a directory with something in it")
	}
	if r := converge(t, &Absent{Path: full, Recursive: true}, Changed); r.Msg != "removed" {
		t.Fatalf("recursive: %q", r.Msg)
	}
	if ok, _ := exists(full); ok {
		t.Fatal("recursive: still there")
	}
}

func TestSymlink(t *testing.T) {
	dir := t.TempDir()
	cache, other, link := filepath.Join(dir, "cache"), filepath.Join(dir, "other"), filepath.Join(dir, ".cache")
	os.Mkdir(cache, 0o755)

	// A real ~/.cache goes aside, its content kept.
	os.Mkdir(link, 0o755)
	os.WriteFile(filepath.Join(link, "x"), []byte("keep"), 0o644)
	aside := filepath.Join(dir, ".cache.bak")
	s := &Symlink{Path: link, Target: cache, Aside: aside, Owner: self()}
	if r := converge(t, s, Changed); r.Msg != "a directory, moved to "+aside {
		t.Fatalf("msg %q", r.Msg)
	}
	if got, _ := os.Readlink(link); got != cache {
		t.Fatalf("link to %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(aside, "x")); string(b) != "keep" {
		t.Fatal("the old directory's content is gone")
	}

	// A link elsewhere is replaced.
	s2 := &Symlink{Path: link, Target: other}
	if r := converge(t, s2, Changed); r.Msg != "points at "+cache {
		t.Fatalf("msg %q", r.Msg)
	}

	// A real directory with nowhere to go is an error.
	real := filepath.Join(dir, "real")
	os.Mkdir(real, 0o755)
	if _, err := (&Symlink{Path: real, Target: cache}).Check(context.Background()); err == nil {
		t.Fatal("a directory with no Aside passed")
	}

	// So is one whose Aside is taken: what is there is never replaced.
	os.WriteFile(filepath.Join(real, "y"), []byte("mine"), 0o644)
	taken := &Symlink{Path: real, Target: cache, Aside: aside}
	if _, err := taken.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("an Aside taken: %v", err)
	}
	if _, err := taken.Apply(context.Background(), State{Data: true}); err == nil {
		t.Fatal("applied over an Aside taken")
	}
	if b, _ := os.ReadFile(filepath.Join(aside, "x")); string(b) != "keep" {
		t.Fatal("the earlier aside is gone")
	}
}

// A seeded file is written when missing, then left: its content is the
// program's (an engine that updated itself), only mode and owner put right.
func TestFileSeed(t *testing.T) {
	dir := t.TempDir()
	src, path := filepath.Join(dir, "src"), filepath.Join(dir, "bin")
	os.WriteFile(src, []byte("v1"), 0o644)
	f := &File{Path: path, From: src, Mode: 0o755, Seed: true}
	converge(t, f, Changed)
	os.WriteFile(path, []byte("v2, self-updated"), 0o755)
	if r := apply(t, f); r.Status != OK {
		t.Fatalf("a newer file: %s %s", r.Status, r.Msg)
	}
	os.Chmod(path, 0o777)
	if r := apply(t, f); r.Status != Changed {
		t.Fatalf("mode: %s", r.Status)
	}
	if b, _ := os.ReadFile(path); string(b) != "v2, self-updated" {
		t.Fatalf("content %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// sameStream compares across buffer boundaries and stops at a length
// difference.
func TestSameStream(t *testing.T) {
	big := strings.Repeat("x", 32<<10)
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{big, big, true},
		{big, big + "y", false},
		{big + "y", big + "z", false},
		{big + "y", big + "y", true},
	} {
		got, err := sameStream(strings.NewReader(c.a), strings.NewReader(c.b))
		if err != nil || got != c.want {
			t.Errorf("%d/%d bytes: %v %v, want %v", len(c.a), len(c.b), got, err, c.want)
		}
	}
}
