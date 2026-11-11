package host

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestWithLine(t *testing.T) {
	re := regexp.MustCompile(`^(/dev/md0|UUID=old)\s+/data\s`)
	const line = "UUID=new /data ext4 defaults,nofail 0 2"
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", line + "\n"},
		{"appended", "a\nb\n", "a\nb\n" + line + "\n"},
		{"no trailing newline", "a", "a\n" + line + "\n"},
		{"in place", "a\n" + line + "\nb\n", "a\n" + line + "\nb\n"},
		{"in place, no trailing newline", "a\n" + line, "a\n" + line},
		{"replaced where it was", "a\nUUID=old /data ext4 defaults 0 2\nb\n", "a\n" + line + "\nb\n"},
		{"the others dropped", "/dev/md0 /data ext4 defaults 0 2\na\nUUID=old /data ext4 x 0 2\n" + line + "\n", line + "\na\n"},
		{"another mount point kept", "UUID=old /data2 ext4 defaults 0 2\n", "UUID=old /data2 ext4 defaults 0 2\n" + line + "\n"},
	} {
		if got := string(WithLine([]byte(c.in), line, re)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// No pattern: only an equal line counts.
	if got := string(WithLine([]byte("x\n"), "y", nil)); got != "x\ny\n" {
		t.Errorf("nil pattern: %q", got)
	}
}

func TestLine_KeepsTheFileAndItsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fstab")
	os.WriteFile(path, []byte("# fstab\nUUID=old /data ext4 defaults 0 2\n"), 0o640)
	l := &Line{Path: path, Line: "UUID=new /data ext4 defaults,nofail 0 2", Replace: regexp.MustCompile(`^UUID=old\s+/data\s`)}
	if r := converge(t, l, Changed); r.Msg != "set the line" {
		t.Fatalf("msg %q", r.Msg)
	}
	if b, _ := os.ReadFile(path); string(b) != "# fstab\nUUID=new /data ext4 defaults,nofail 0 2\n" {
		t.Fatalf("fstab %q", b)
	}
	if m := perm(t, path); m != 0o640 {
		t.Fatalf("mode %o", m)
	}
}

func TestLine_CreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mdadm.conf")
	l := &Line{Path: path, Line: "ARRAY /dev/md0 metadata=1.2 UUID=x"}
	if r := converge(t, l, Changed); r.Msg != "create the file with the line" || perm(t, path) != 0o644 {
		t.Fatalf("msg %q", r.Msg)
	}
	if b, _ := os.ReadFile(path); string(b) != "ARRAY /dev/md0 metadata=1.2 UUID=x\n" {
		t.Fatalf("content %q", b)
	}
}

// A link is followed: the file it names is written, the link stays.
func TestLine_FollowsALink(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real.conf"), filepath.Join(dir, "conf")
	os.WriteFile(real, []byte("a\n"), 0o644)
	os.Symlink(real, link)
	converge(t, &Line{Path: link, Line: "b"}, Changed)
	if b, _ := os.ReadFile(real); string(b) != "a\nb\n" {
		t.Fatalf("real %q", b)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
}

// A link to nothing is an error: the link is not replaced with a file.
func TestLine_DanglingLink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "fstab")
	os.Symlink("/nonexistent/fstab", link)
	l := &Line{Path: link, Line: "x"}
	if _, err := l.Check(context.Background()); err == nil {
		t.Fatal("a link to nothing passed")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
}

func TestWithoutLine(t *testing.T) {
	for _, c := range []struct{ in, line, re, want string }{
		{"a\nb\na\n", "a", "", "b\n"},
		{"a\nb", "", "^b", "a"},
		{"a\n", "a", "", ""},
		{"", "a", "", ""},
	} {
		var re *regexp.Regexp
		if c.re != "" {
			re = regexp.MustCompile(c.re)
		}
		if got := string(WithoutLine([]byte(c.in), c.line, re)); got != c.want {
			t.Errorf("%q - %q: %q, want %q", c.in, c.line, got, c.want)
		}
	}
}

// A line with a newline in it would never match a line of the file.
func TestLine_RefusesANewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	os.WriteFile(p, []byte("a\n"), 0o644)
	if _, err := (&Line{Path: p, Line: "foo\nbar"}).Check(context.Background()); err == nil {
		t.Fatal("no error")
	}
}
