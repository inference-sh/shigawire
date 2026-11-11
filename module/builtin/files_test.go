package builtin_test

import (
	"fmt"
	"os"
	"testing"
	"testing/fstest"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
)

func TestFile(t *testing.T) {
	h, env := newHost(t)
	r := build(t, env, "file", map[string]any{"path": "/etc/motd", "content": "hi\n", "mode": "0600", "parents": true})
	hosttest.Converge(t, r, host.Changed)
	if h.Read("/etc/motd") != "hi\n" {
		t.Fatal(h.Read("/etc/motd"))
	}
	if fi, _ := os.Stat(h.Path("/etc/motd")); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	// An empty content is an empty file.
	hosttest.Converge(t, build(t, env, "file", map[string]any{"path": "/etc/motd", "content": ""}), host.Changed)
	if h.Read("/etc/motd") != "" {
		t.Fatal("not emptied")
	}
}

func TestFileOwner(t *testing.T) {
	h, env := newHost(t)
	uid, gid := os.Getuid(), os.Getgid()
	h.Exec.On("getent passwd me", hosttest.Out(fmt.Sprintf("me:x:%d:%d::/home/me:/bin/sh\n", uid, gid)))
	h.Exec.On("getent group mine", hosttest.Out(fmt.Sprintf("mine:x:%d:\n", gid)))
	h.Write("/etc/app.conf", "a\n")
	hosttest.Converge(t, build(t, env, "file", map[string]any{"path": "/etc/app.conf", "content": "b\n", "owner": "me", "group": "mine"}), host.Changed)
	if h.Read("/etc/app.conf") != "b\n" {
		t.Fatal(h.Read("/etc/app.conf"))
	}
	r := build(t, env, "file", map[string]any{"path": "/etc/app.conf", "content": "b\n", "owner": "nobody"})
	h.Exec.On("getent passwd nobody", hosttest.Exit(2))
	if got, err := play(hosttest.Apply, host.Task{Resource: r}); err == nil || got[0].Status != host.Failed {
		t.Fatalf("%v %v", hosttest.Statuses(got), err)
	}
}

func TestCopy(t *testing.T) {
	h, env := newHost(t)
	// An absolute src is the host's, under Root, read when checked.
	r := build(t, env, "copy", map[string]any{"src": "/opt/build/tool", "dest": "/usr/local/bin/tool", "mode": "0755", "parents": true})
	h.Write("/opt/build/tool", "\x7fELF")
	hosttest.Converge(t, r, host.Changed)
	if h.Read("/usr/local/bin/tool") != "\x7fELF" {
		t.Fatal("not copied")
	}
}

func TestCopyFromFiles(t *testing.T) {
	h, env := newHost(t)
	env.Files = fstest.MapFS{"nginx/site.conf": {Data: []byte("server {}\n")}}
	hosttest.Converge(t, build(t, env, "copy", map[string]any{"src": "nginx/site.conf", "dest": "/etc/nginx/conf.d/site.conf", "parents": true}), host.Changed)
	if h.Read("/etc/nginx/conf.d/site.conf") != "server {}\n" {
		t.Fatal("not copied")
	}
	refuse(t, env, "copy", map[string]any{"src": "missing", "dest": "/etc/x"}, "src:")
	refuse(t, env, "copy", map[string]any{"src": "../etc/passwd", "dest": "/etc/x"}, "not a path inside files/")
	env.Files = nil
	refuse(t, env, "copy", map[string]any{"src": "site.conf", "dest": "/etc/x"}, "no files/ directory")
}

func TestFileRefused(t *testing.T) {
	h, env := newHost(t)
	refuse(t, env, "file", map[string]any{"path": "etc/motd", "content": "x"}, "want an absolute path")
	refuse(t, env, "file", map[string]any{"content": "x"}, "path is required")
	refuse(t, env, "file", map[string]any{"path": "/x", "content": "x", "src": "/y"}, "give one")
	refuse(t, env, "file", map[string]any{"path": "/x"}, "content or src is required")
	// seed alone: the content there is kept, only the mode is set.
	h.Write("/etc/key", "KEY")
	hosttest.Converge(t, build(t, env, "file", map[string]any{"path": "/etc/key", "seed": true, "mode": "0600"}), host.Changed)
	if fi, _ := os.Stat(h.Path("/etc/key")); h.Read("/etc/key") != "KEY" || fi.Mode().Perm() != 0o600 {
		t.Fatal(h.Read("/etc/key"), fi.Mode())
	}
	refuse(t, env, "file", map[string]any{"path": "/x", "content": "x", "group": "adm"}, "group needs owner")
	refuse(t, env, "file", map[string]any{"path": "/x", "content": "x", "mode": "rw"}, "not an octal mode")
	refuse(t, env, "copy", map[string]any{"path": "/x", "content": "x"}, `unknown key "path"`)
}

func TestDirAbsentSymlink(t *testing.T) {
	h, env := newHost(t)
	hosttest.Converge(t, build(t, env, "dir", map[string]any{"path": "/srv/data/a", "mode": "0750"}), host.Changed)
	if fi, err := os.Stat(h.Path("/srv/data/a")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatal(fi, err)
	}
	h.Write("/srv/data/a/f", "x")
	hosttest.Converge(t, build(t, env, "absent", map[string]any{"path": "/srv/data", "recursive": true}), host.Changed)
	if h.Exists("/srv/data") {
		t.Fatal("still there")
	}
	h.Write("/etc/resolv.conf", "nameserver 1.1.1.1\n")
	hosttest.Converge(t, build(t, env, "symlink", map[string]any{"path": "/etc/resolv.conf", "target": "/run/resolv.conf", "force": true}), host.Changed)
	if got, _ := os.Readlink(h.Path("/etc/resolv.conf")); got != "/run/resolv.conf" {
		t.Fatal(got)
	}
	refuse(t, env, "dir", map[string]any{"path": "srv"}, "absolute")
	refuse(t, env, "absent", map[string]any{"path": "/", "recursive": true}, "refusing")
	refuse(t, env, "symlink", map[string]any{"path": "/a"}, "target is required")
	refuse(t, env, "symlink", map[string]any{"path": "/a", "target": "/b", "aside": "b"}, "aside")
}

func TestLine(t *testing.T) {
	h, env := newHost(t)
	h.Write("/etc/ssh/sshd_config", "Port 22\nPasswordAuthentication yes\n")
	hosttest.Converge(t, build(t, env, "lineinfile", map[string]any{"path": "/etc/ssh/sshd_config",
		"line": "PasswordAuthentication no", "regexp": "^#?PasswordAuthentication"}), host.Changed)
	if got := h.Read("/etc/ssh/sshd_config"); got != "Port 22\nPasswordAuthentication no\n" {
		t.Fatalf("%q", got)
	}
	hosttest.Converge(t, build(t, env, "line", map[string]any{"path": "/etc/ssh/sshd_config", "regexp": "^Port ", "state": "absent"}), host.Changed)
	if got := h.Read("/etc/ssh/sshd_config"); got != "PasswordAuthentication no\n" {
		t.Fatalf("%q", got)
	}
	// Absent from a file that is not there: nothing to do.
	if r := hosttest.ApplyOne(t, build(t, env, "line", map[string]any{"path": "/etc/none", "line": "x", "state": "absent"})); r.Status != host.OK {
		t.Fatal(r.Status)
	}
	refuse(t, env, "line", map[string]any{"path": "/etc/x"}, "line is required")
	refuse(t, env, "line", map[string]any{"path": "/etc/x", "state": "absent"}, "line or regexp")
	refuse(t, env, "line", map[string]any{"path": "/etc/x", "line": "a", "regexp": "("}, "regexp")
	refuse(t, env, "line", map[string]any{"path": "/etc/x", "line": "a", "state": "gone"}, "want present, absent")
	// Several lines are a block's; a block scalar's one newline is not part
	// of the line.
	refuse(t, env, "line", map[string]any{"path": "/etc/x", "line": "foo\nbar"}, "use block")
	h.Write("/etc/x.conf", "a\n")
	hosttest.Converge(t, build(t, env, "line", map[string]any{"path": "/etc/x.conf", "line": "foo\n"}), host.Changed)
	if got := h.Read("/etc/x.conf"); got != "a\nfoo\n" {
		t.Fatalf("%q", got)
	}
}

func TestBlock(t *testing.T) {
	h, env := newHost(t)
	h.Write("/etc/hosts", "127.0.0.1 localhost\n")
	hosttest.Converge(t, build(t, env, "blockinfile", map[string]any{"path": "/etc/hosts", "block": "10.0.0.2 db\n10.0.0.3 cache\n"}), host.Changed)
	want := "127.0.0.1 localhost\n# BEGIN MANAGED BLOCK\n10.0.0.2 db\n10.0.0.3 cache\n# END MANAGED BLOCK\n"
	if got := h.Read("/etc/hosts"); got != want {
		t.Fatalf("%q", got)
	}
	hosttest.Converge(t, build(t, env, "block", map[string]any{"path": "/etc/hosts", "state": "absent"}), host.Changed)
	if got := h.Read("/etc/hosts"); got != "127.0.0.1 localhost\n" {
		t.Fatalf("%q", got)
	}
	refuse(t, env, "block", map[string]any{"path": "/etc/hosts", "marker": "# managed"}, "{mark}")
}

// A module of another package's that writes a file embeds FileArgs and
// takes file's keys for it.
func TestFileArgsEmbedded(t *testing.T) {
	var a struct {
		Name             string `yaml:"name"`
		builtin.FileArgs `yaml:",inline"`
	}
	if err := module.Decode(map[string]any{"name": "x", "content": "hi\n", "mode": "0600", "parents": true}, &a); err != nil {
		t.Fatal(err)
	}
	if a.Name != "x" || a.Content == nil || *a.Content != "hi\n" || a.Mode != 0o600 || !a.Parents {
		t.Fatalf("%+v", a)
	}
}
