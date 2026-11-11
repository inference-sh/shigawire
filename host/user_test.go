package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The user is made once, with its own group, a home 0750 that is its own
// and no login; the second run changes nothing.
func TestSystemUser(t *testing.T) {
	root := t.TempDir()
	entry := ""
	ex := &fakeExec{respond: func(line string) Output {
		switch {
		case line == "getent passwd inferencesh":
			if entry == "" {
				return exit(2)
			}
			return Output{Out: entry + "\n"}
		case strings.HasPrefix(line, "useradd "):
			entry = fmt.Sprintf("inferencesh:x:%d:%d:inference.sh engine:/home/inferencesh:/usr/sbin/nologin", os.Getuid(), os.Getgid())
			// useradd --create-home, with the distro's umask.
			os.MkdirAll(filepath.Join(root, "home/inferencesh"), 0o755)
		}
		return Output{}
	}}
	u := &SystemUser{Exec: ex, Name: "inferencesh", Home: "/home/inferencesh", Comment: "inference.sh engine", Shell: "/usr/sbin/nologin", HomeMode: 0o750, Root: root}
	converge(t, u, Changed)
	want := "useradd --system --user-group --create-home --home-dir /home/inferencesh --shell /usr/sbin/nologin --comment inference.sh engine inferencesh"
	if got := ex.ran("useradd"); len(got) != 1 || got[0] != want {
		t.Fatalf("useradd: %v", got)
	}
	fi, err := os.Stat(filepath.Join(root, "home/inferencesh"))
	if err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("home: %v %v", fi.Mode(), err)
	}
}

// A user of that name setup did not make is refused, never taken over.
func TestSystemUserRefusesAnother(t *testing.T) {
	ex := &fakeExec{respond: func(line string) Output {
		if line == "getent passwd inferencesh" {
			return Output{Out: "inferencesh:x:1001:1001:Model server,,,:/home/inferencesh:/bin/bash\n"}
		}
		return Output{}
	}}
	u := &SystemUser{Exec: ex, Name: "inferencesh", Home: "/home/inferencesh", Comment: "inference.sh engine", Shell: "/usr/sbin/nologin", HomeMode: 0o750, Root: t.TempDir()}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "did not make") {
		t.Fatalf("check: %v", err)
	}
	if len(ex.ran("useradd")) != 0 || len(ex.ran("usermod")) != 0 {
		t.Fatalf("ran %v", ex.calls)
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
