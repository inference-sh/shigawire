//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

// tcase is one resource on a real host. Each run gets fresh resources
// (a fresh Apt), as each run of host setup does.
type tcase struct {
	name string
	// setup puts the host in the state before the play, directly.
	setup func(t *testing.T)
	play  func(a *host.Apt) host.Play
	// present is nil when the play's effect is on the host, else what is
	// missing.
	present func() error
	// dryFirst is a first dry run's statuses, when not all changed.
	dryFirst []host.Status
	// unguarded: a command that runs every time; a dry run always reports
	// it would change, and running it again reports ok.
	unguarded bool
}

// TestInside applies each case four times, in order: a dry run reports
// changed and changes nothing, a run reports changed and puts it in
// place, a second run reports ok and runs no handler, a dry run reports
// ok. It runs only as root in the container TestConformance starts.
func TestInside(t *testing.T) {
	if os.Getenv(insideEnv) != "1" {
		t.Skip("runs only in a container: TestConformance starts it")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil || os.Getuid() != 0 {
		t.Fatal("TestInside needs root in a container")
	}
	h := newHost(t)
	for _, c := range h.cases() {
		t.Run(c.name, func(t *testing.T) {
			if strings.HasPrefix(c.name, "systemd/") && !exists("/run/systemd/system") {
				t.Skip("systemd is not running")
			}
			h.check(t, c)
		})
	}
	t.Run("download/bad-sha256", h.badSHA256)
	t.Run("symlink/aside-taken", h.asideTaken)
	t.Run("line/dangling-link", h.danglingLink)
}

func (h *box) check(t *testing.T, c tcase) {
	if c.setup != nil {
		c.setup(t)
	}
	run := func(phase string, dry bool, want func(i int) host.Status) {
		t.Helper()
		mode := hosttest.Apply
		if dry {
			mode = hosttest.Plan
		}
		got, err := hosttest.Run(mode, c.play(&host.Apt{Exec: h.x}))
		for _, g := range got {
			t.Logf("%s: %-8s %s: %s", phase, g.Status, g.Name, g.Msg)
		}
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		for i, g := range got {
			if w := want(i); g.Status != w {
				t.Errorf("%s: %s is %s, want %s (%s)", phase, g.Name, g.Status, w, g.Msg)
			}
			if g.Handler && !dry && phase == "again" {
				t.Errorf("%s: handler %s ran", phase, g.Name)
			}
		}
	}
	all := func(s host.Status) func(int) host.Status { return func(int) host.Status { return s } }

	if c.present() == nil {
		t.Fatal("in place before the first run: the setup did not undo it")
	}
	// dry is a dry run that must leave the host as it was.
	dry := func(phase string, want func(i int) host.Status) {
		t.Helper()
		before := snapshot(t)
		run(phase, true, want)
		if d := diff(before, snapshot(t)); d != "" {
			t.Fatalf("%s changed the host: %s", phase, d)
		}
	}
	first := all(host.Changed)
	if c.dryFirst != nil {
		first = func(i int) host.Status {
			if i < len(c.dryFirst) {
				return c.dryFirst[i]
			}
			return host.Changed
		}
	}
	dry("check", first)
	if c.present() == nil {
		t.Fatal("check mode changed the host")
	}
	run("apply", false, all(host.Changed))
	if err := c.present(); err != nil {
		t.Fatalf("after apply: %v", err)
	}
	run("again", false, all(host.OK))
	if err := c.present(); err != nil {
		t.Fatalf("after the second apply: %v", err)
	}
	if c.unguarded {
		dry("check again", all(host.Changed))
	} else {
		dry("check again", all(host.OK))
	}
}

// box is the container the cases run in.
type box struct {
	x        host.OSExec
	codename string     // VERSION_CODENAME
	user     host.Owner // the login user, infsh
	srv      *httptest.Server
	payload  []byte
	lists    sync.Once
}

const loginUser = "infsh"

func newHost(t *testing.T) *box {
	h := &box{payload: []byte("#!/bin/sh\necho conformance payload\n")}
	osr, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^VERSION_CODENAME=(\S+)$`).FindSubmatch(osr); m != nil {
		h.codename = string(m[1])
	}
	u, err := user.Lookup(loginUser)
	if err != nil {
		t.Fatal(err)
	}
	if h.user, err = host.UserOwner(u); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/payload", func(w http.ResponseWriter, _ *http.Request) { w.Write(h.payload) })
	mux.HandleFunc("/key.gpg", func(w http.ResponseWriter, _ *http.Request) {
		key, err := h.out("curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor")
		if err != nil {
			http.Error(w, key, http.StatusBadGateway)
			return
		}
		w.Write([]byte(key))
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *box) cases() []tcase {
	const (
		hello    = "hello"           // no dependencies, on every distro
		bashc    = "bash-completion" // no dependencies, has config files
		keyrings = "/etc/apt/keyrings/"
		source   = "/etc/apt/sources.list.d/nvidia-container-toolkit.sources"
		file     = "/etc/shiga-conformance/file"
		fstab    = "/etc/shiga-conformance/fstab"
		unit     = "/etc/systemd/system/shiga-conformance.service"
		conf     = "/etc/shiga-conformance/unit.conf"
		starts   = "/run/shiga-conformance.starts"
	)
	sum := sha256.Sum256(h.payload)
	payloadSum := hex.EncodeToString(sum[:])
	var deb string // InstallDeb's .deb, fetched by its setup
	owner := h.user
	task := func(r host.Resource, notify ...*host.Handler) host.Task {
		return host.Task{Resource: r, Notify: notify}
	}
	tasks := func(rs ...host.Resource) host.Play {
		var p host.Play
		for _, r := range rs {
			p.Tasks = append(p.Tasks, task(r))
		}
		return p
	}
	// repo is NVIDIA's container toolkit repo signed by the key at keyURL,
	// which Repo writes to keyPath. The two repo cases share the source
	// file (apt rejects one repo with two keys), so each removes both.
	repo := func(name, keyURL, keyPath string) tcase {
		return tcase{
			name: name,
			setup: func(t *testing.T) {
				h.sh(t, "rm -f "+keyrings+"nvidia-container-toolkit.* "+source+" && apt-get -q update")
			},
			play: func(a *host.Apt) host.Play {
				return tasks(a.Repo(host.Repo{
					Name:   "nvidia-container-toolkit",
					URIs:   []string{"https://nvidia.github.io/libnvidia-container/stable/deb/" + runtime.GOARCH},
					Suites: []string{"/"},
					KeyURL: keyURL,
				}), a.Update(time.Hour))
			},
			dryFirst: []host.Status{host.Changed, host.OK},
			present: func() error {
				if !exists(keyPath) || !exists(source) {
					return errors.New("no key or source file")
				}
				out, err := h.out("apt-cache policy nvidia-container-toolkit")
				if err != nil || !strings.Contains(out, "nvidia.github.io") {
					return fmt.Errorf("no nvidia-container-toolkit from the repo: %v %s", err, out)
				}
				return nil
			},
		}
	}
	fstabBefore := "# fstab\nUUID=old /data ext4 defaults 0 2\nproc /proc proc defaults 0 0\nUUID=older /data xfs defaults 0 0\n"
	fstabLine := "UUID=new /data ext4 defaults,nofail 0 2"

	return []tcase{
		{
			name: "apt/update+install",
			setup: func(t *testing.T) {
				h.sh(t, "dpkg --purge "+hello+
					" && touch -d '2 days ago' /var/lib/apt/lists; f=/var/lib/apt/periodic/update-success-stamp; [ ! -e $f ] || touch -d '2 days ago' $f")
			},
			play:    func(a *host.Apt) host.Play { return tasks(a.Update(time.Hour), a.Install(hello)) },
			present: func() error { return h.pkgStatus(hello, "ii") },
		},
		{
			// Lists past their age whose sources have not changed since:
			// an update that fetches nothing new still makes them fresh.
			name: "apt/update-unchanged",
			setup: func(t *testing.T) {
				h.aptLists(t)
				h.sh(t, "touch -d '2 days ago' /var/lib/apt/lists; f=/var/lib/apt/periodic/update-success-stamp; [ ! -e $f ] || touch -d '2 days ago' $f")
			},
			play: func(a *host.Apt) host.Play { return tasks(a.Update(time.Hour)) },
			present: func() error {
				for _, p := range []string{"/var/lib/apt/periodic/update-success-stamp", "/var/lib/apt/lists"} {
					if fi, err := os.Stat(p); err == nil {
						if age := time.Since(fi.ModTime()); age > time.Hour {
							return fmt.Errorf("%s is %s old", p, age.Round(time.Minute))
						}
						return nil
					}
				}
				return errors.New("no lists")
			},
		},
		{
			name: "apt/remove",
			setup: func(t *testing.T) {
				h.aptLists(t)
				h.sh(t, "apt-get -y -q install "+bashc)
			},
			play:    func(a *host.Apt) host.Play { return tasks(a.Remove(host.Names(bashc), false)) },
			present: func() error { return h.pkgStatus(bashc, "rc") },
		},
		{
			// What a failed install leaves: a package unpacked, never
			// configured. Remove takes it as present.
			name: "apt/remove-unpacked",
			setup: func(t *testing.T) {
				h.aptLists(t)
				dir := t.TempDir()
				h.sh(t, "dpkg --purge "+hello+" && cd "+dir+" && apt-get -q download "+hello+" && dpkg --unpack "+hello+"_*.deb")
				if err := h.pkgStatus(hello, "iU"); err != nil {
					t.Fatal(err)
				}
			},
			play:    func(a *host.Apt) host.Play { return tasks(a.Remove(host.Names(hello), false)) },
			present: func() error { return h.pkgStatus(hello, "") },
		},
		{
			// A source changed after the last update (a run wrote it, and
			// its update failed): the next run's update is due, though the
			// lists are fresh.
			name: "apt/update-after-a-source-changed",
			setup: func(t *testing.T) {
				h.aptLists(t)
				h.sh(t, "touch -d '10 minutes ago' /var/lib/apt/lists; f=/var/lib/apt/periodic/update-success-stamp; [ ! -e $f ] || touch -d '10 minutes ago' $f; "+
					"touch $(ls -d /etc/apt/sources.list /etc/apt/sources.list.d/* 2>/dev/null)")
			},
			play:    func(a *host.Apt) host.Play { return tasks(a.Update(time.Hour)) },
			present: h.listsAfterSources,
		},
		{
			name: "apt/purge",
			setup: func(t *testing.T) {
				h.aptLists(t)
				h.sh(t, "apt-get -y -q install "+bashc+" && apt-get -y -q remove "+bashc)
			},
			play: func(a *host.Apt) host.Play {
				sel := host.Pkgs{Desc: "bash-completion*", Match: func(n string) bool { return strings.HasPrefix(n, bashc) }}
				return tasks(a.Remove(sel, true))
			},
			present: func() error { return h.pkgStatus(bashc, "") },
		},
		{
			name: "apt/install-deb",
			setup: func(t *testing.T) {
				h.aptLists(t)
				dir := t.TempDir()
				h.sh(t, "dpkg --purge "+hello+" && cd "+dir+" && apt-get -q download "+hello)
				m, _ := filepath.Glob(filepath.Join(dir, hello+"_*.deb"))
				if len(m) != 1 {
					t.Fatalf("apt-get download %s: %v", hello, m)
				}
				deb = m[0]
			},
			play:    func(a *host.Apt) host.Play { return tasks(a.InstallDeb(deb)) },
			present: func() error { return h.pkgStatus(hello, "ii") },
		},
		// The repo gpu_toolkit.yml adds: a flat repo (suite "/"), no
		// components. A changed repo updates the lists however fresh.
		repo("apt/repo", "https://nvidia.github.io/libnvidia-container/gpgkey", keyrings+"nvidia-container-toolkit.asc"),
		// The same key unarmored, served here: a binary keyring.
		repo("apt/repo-binary-key", h.srv.URL+"/key.gpg", keyrings+"nvidia-container-toolkit.gpg"),
		{
			name:  "file/content",
			setup: func(t *testing.T) { h.sh(t, "rm -rf /etc/shiga-conformance && mkdir /etc/shiga-conformance") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.File{Path: file, Content: []byte("a=1\n"), Mode: 0o600, Owner: &owner})
			},
			present: func() error { return h.fileIs(file, "a=1\n", 0o600, owner) },
		},
		{
			name: "file/mode+owner",
			setup: func(t *testing.T) {
				h.sh(t, "mkdir -p /etc/shiga-conformance && printf 'a=1\\n' > "+file+" && chown 0:0 "+file+" && chmod 0644 "+file)
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.File{Path: file, Content: []byte("a=1\n"), Mode: 0o640, Owner: &owner})
			},
			present: func() error { return h.fileIs(file, "a=1\n", 0o640, owner) },
		},
		{
			name:  "file/from",
			setup: func(t *testing.T) { h.sh(t, "rm -f /usr/local/bin/res-true") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.File{Path: "/usr/local/bin/res-true", From: "/bin/true", Mode: 0o755})
			},
			present: func() error {
				if _, err := h.out("/usr/local/bin/res-true"); err != nil {
					return err
				}
				return h.sameFile("/usr/local/bin/res-true", "/bin/true")
			},
		},
		{
			name:  "dir",
			setup: func(t *testing.T) { h.sh(t, "rm -rf /srv/shiga-conformance") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.Dir{Path: "/srv/shiga-conformance/a/b", Mode: 0o750, Owner: &owner})
			},
			present: func() error { return h.modeOwner("/srv/shiga-conformance/a/b", fs.ModeDir|0o750, owner) },
		},
		{
			name:    "absent",
			setup:   func(t *testing.T) { h.sh(t, "mkdir -p /etc/shiga-conformance && touch /etc/shiga-conformance/gone") },
			play:    func(*host.Apt) host.Play { return tasks(&host.Absent{Path: "/etc/shiga-conformance/gone"}) },
			present: func() error { return notExists("/etc/shiga-conformance/gone") },
		},
		{
			name: "symlink",
			setup: func(t *testing.T) {
				h.sh(t, "rm -f /etc/res-link /etc/res-link.orig && echo real > /etc/res-link")
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.Symlink{Path: "/etc/res-link", Target: "/etc/hostname", Aside: "/etc/res-link.orig"})
			},
			present: func() error {
				if got, err := os.Readlink("/etc/res-link"); err != nil || got != "/etc/hostname" {
					return fmt.Errorf("link: %q %v", got, err)
				}
				return h.fileIs("/etc/res-link.orig", "real\n", 0o644, host.Owner{})
			},
		},
		{
			// An fstab with two entries for the mount point: the first
			// becomes the line, the other goes; the mode is kept.
			name: "line/replace",
			setup: func(t *testing.T) {
				h.sh(t, "mkdir -p /etc/shiga-conformance && printf '"+fstabBefore+"' > "+fstab+" && chmod 0600 "+fstab)
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.Line{Path: fstab, Line: fstabLine, Replace: regexp.MustCompile(`^\S+\s+/data\s`)})
			},
			present: func() error {
				want := "# fstab\n" + fstabLine + "\nproc /proc proc defaults 0 0\n"
				return h.fileIs(fstab, want, 0o600, host.Owner{})
			},
		},
		{
			name:  "line/create",
			setup: func(t *testing.T) { h.sh(t, "mkdir -p /etc/shiga-conformance && rm -f /etc/shiga-conformance/new") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.Line{Path: "/etc/shiga-conformance/new", Line: "x=1"})
			},
			present: func() error { return h.fileIs("/etc/shiga-conformance/new", "x=1\n", 0o644, host.Owner{}) },
		},
		{
			name:  "command/creates",
			setup: func(t *testing.T) { h.sh(t, "rm -f /var/lib/res-made") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.Command{Exec: h.x, Creates: "/var/lib/res-made",
					Cmd: host.Cmd{Name: "sh", Args: []string{"-c", "echo made > /var/lib/res-made"}}})
			},
			present: func() error { return h.fileIs("/var/lib/res-made", "made\n", 0o644, host.Owner{}) },
		},
		{
			// A config a tool rewrites with the same settings (nvidia-ctk's
			// daemon.json): changed only the first time.
			name:      "command/watch",
			setup:     func(t *testing.T) { h.sh(t, "rm -f /etc/res-watched") },
			unguarded: true,
			play: func(*host.Apt) host.Play {
				return tasks(&host.Command{Exec: h.x, Watch: []string{"/etc/res-watched"},
					Cmd: host.Cmd{Name: "sh", Args: []string{"-c", "printf x > /etc/res-watched"}}})
			},
			present: func() error { return h.fileIs("/etc/res-watched", "x", 0o644, host.Owner{}) },
		},
		{
			// A user and a group made by command, guarded with Unless, and
			// the login user put in the group (User.Groups).
			name: "user+group",
			setup: func(t *testing.T) {
				h.sh(t, "userdel res-svc 2>/dev/null; groupdel res-group 2>/dev/null; true")
			},
			play: func(*host.Apt) host.Play {
				cmd := func(name string, args ...string) host.Cmd { return host.Cmd{Name: name, Args: args} }
				unless := func(name string, args ...string) *host.Cmd { c := cmd(name, args...); return &c }
				return tasks(
					&host.Command{Exec: h.x, Cmd: cmd("groupadd", "--system", "res-group"), Unless: unless("getent", "group", "res-group")},
					&host.Command{Exec: h.x, Cmd: cmd("useradd", "--system", "--no-create-home", "-g", "res-group", "res-svc"), Unless: unless("id", "res-svc")},
					&host.User{Exec: h.x, Name: loginUser, Groups: []string{"res-group"}},
				)
			},
			present: func() error {
				out, err := h.out("id -nG " + loginUser + " && id -gn res-svc")
				if err != nil || !slices.Contains(strings.Fields(out), "res-group") {
					return fmt.Errorf("not in res-group: %v %s", err, out)
				}
				return nil
			},
		},
		{
			// A system user with its home and no login (User).
			name: "system-user",
			setup: func(t *testing.T) {
				h.sh(t, "userdel -r res-engine 2>/dev/null; groupdel res-engine 2>/dev/null; rm -rf /home/res-engine; true")
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.User{Exec: h.x, Name: "res-engine", Home: "/home/res-engine", Comment: "res conformance engine",
					Shell: "/usr/sbin/nologin", System: true, CreateHome: true})
			},
			present: func() error {
				out, err := h.out("getent passwd res-engine && stat -c '%U' /home/res-engine")
				if err != nil || !strings.Contains(out, ":res conformance engine:/home/res-engine:/usr/sbin/nologin") ||
					!strings.Contains(out, "\nres-engine") {
					return fmt.Errorf("user: %v %s", err, out)
				}
				return nil
			},
		},
		{
			// The login user put in a group by GroupMember.
			name: "group-member",
			setup: func(t *testing.T) {
				h.sh(t, "gpasswd -d "+loginUser+" res-member 2>/dev/null; groupdel res-member 2>/dev/null; groupadd --system res-member")
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.GroupMember{Exec: h.x, User: loginUser, Group: "res-member"})
			},
			present: func() error {
				if in, err := host.InGroup(context.Background(), h.x, loginUser, "res-member"); err != nil || !in {
					return fmt.Errorf("not in res-member: %v", err)
				}
				return nil
			},
		},
		{
			// The engine's own user, as setup makes it: a system user with
			// its group, a home 0750 of its own, no login (SystemUser).
			name: "system-user/engine",
			setup: func(t *testing.T) {
				h.sh(t, "userdel -r res-sysuser 2>/dev/null; groupdel res-sysuser 2>/dev/null; rm -rf /home/res-sysuser; true")
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.SystemUser{Exec: h.x, Name: "res-sysuser", Home: "/home/res-sysuser", Comment: "res conformance engine", Shell: "/usr/sbin/nologin", HomeMode: 0o750})
			},
			present: func() error {
				out, err := h.out("getent passwd res-sysuser && stat -c '%a %U:%G' /home/res-sysuser && id -gn res-sysuser")
				if err != nil || !strings.Contains(out, ":res conformance engine:/home/res-sysuser:/usr/sbin/nologin") ||
					!strings.Contains(out, "750 res-sysuser:res-sysuser") {
					return fmt.Errorf("user: %v %s", err, out)
				}
				return nil
			},
		},
		{
			// A tree root's engine left, given to the login user; a link in
			// it to /etc/shadow is changed itself, never what it points at.
			name: "own-tree",
			setup: func(t *testing.T) {
				h.sh(t, "rm -rf /srv/res-tree && mkdir -p /srv/res-tree/apps/a/venv && echo x > /srv/res-tree/apps/a/venv/python && "+
					"ln -s /etc/shadow /srv/res-tree/apps/a/shadow && ln -s /etc /srv/res-tree/apps/a/etc")
			},
			play: func(*host.Apt) host.Play {
				return tasks(&host.OwnTree{Path: "/srv/res-tree", Owner: owner, Hardlinks: host.ProtectedHardlinks("")})
			},
			present: func() error {
				out, err := h.out("stat -c '%U %n' /srv/res-tree/apps/a/venv/python && stat -c '%U %n' /etc/shadow /etc && stat -L -c '%U' /srv/res-tree/apps/a/shadow")
				if err != nil || !strings.Contains(out, loginUser+" /srv/res-tree/apps/a/venv/python") || !strings.Contains(out, "root /etc/shadow") || !strings.Contains(out, "root /etc\n") {
					return fmt.Errorf("tree: %v %s", err, out)
				}
				return nil
			},
		},
		{
			name:  "download",
			setup: func(t *testing.T) { h.sh(t, "rm -f /usr/local/bin/res-payload") },
			play: func(*host.Apt) host.Play {
				return tasks(&host.Download{URL: h.srv.URL + "/payload", Path: "/usr/local/bin/res-payload",
					SHA256: payloadSum, Mode: 0o755})
			},
			present: func() error {
				if out, err := h.out("/usr/local/bin/res-payload"); err != nil || out != "conformance payload\n" {
					return fmt.Errorf("run it: %v %q", err, out)
				}
				return h.fileIs("/usr/local/bin/res-payload", string(h.payload), 0o755, host.Owner{})
			},
		},
		{
			// A unit written, reloaded and restarted at a flush, then
			// enabled and started; its config changed after, so it is
			// restarted once at the end. It counts its starts in
			// ExecStartPre, which start waits for: counted in ExecStart,
			// a restart right after the start can kill the shell before
			// it writes (systemd 257, debian-13).
			name: "systemd/unit",
			setup: func(t *testing.T) {
				h.sh(t, "systemctl disable --now shiga-conformance 2>/dev/null; rm -f "+unit+" "+conf+" "+starts+
					" && mkdir -p /etc/shiga-conformance && systemctl daemon-reload")
			},
			play: func(*host.Apt) host.Play {
				body := "[Unit]\nDescription=res conformance\n\n[Service]\n" +
					"ExecStartPre=/bin/sh -c 'echo started >> " + starts + "'\nExecStart=/bin/sleep infinity\n\n" +
					"[Install]\nWantedBy=multi-user.target\n"
				reload := &host.Handler{Name: "daemon-reload", Resource: &host.DaemonReload{Exec: h.x}}
				restart := &host.Handler{Name: "restart", Resource: &host.Restart{Exec: h.x, Name: "shiga-conformance"}}
				return host.Play{
					Tasks: []host.Task{
						task(&host.File{Path: unit, Content: []byte(body)}, reload),
						host.Flush(),
						task(&host.Service{Exec: h.x, Name: "shiga-conformance", Enabled: true, Running: true}),
						task(&host.File{Path: conf, Content: []byte("level=1\n")}, restart),
					},
					Handlers: []*host.Handler{reload, restart},
				}
			},
			present: func() error {
				if _, err := h.out("systemctl is-enabled --quiet shiga-conformance && systemctl is-active --quiet shiga-conformance"); err != nil {
					return errors.New("not enabled and running")
				}
				// Started, then restarted for the config: twice, and no more
				// on the second run.
				b, _ := os.ReadFile(starts)
				if n := bytes.Count(b, []byte("started")); n != 2 {
					return fmt.Errorf("started %d times, want 2", n)
				}
				return nil
			},
		},
	}
}

// badSHA256: a download that does not match its hash fails and leaves
// nothing at the path.
func (h *box) badSHA256(t *testing.T) {
	const path = "/usr/local/bin/res-bad"
	_ = os.Remove(path)
	got, err := hosttest.Run(hosttest.Apply, host.Play{Tasks: []host.Task{{Resource: &host.Download{
		URL: h.srv.URL + "/payload", Path: path, SHA256: strings.Repeat("0", 64)}}}})
	if err == nil || len(got) != 1 || got[0].Status != host.Failed {
		t.Fatalf("got %+v, %v; want a failure", got, err)
	}
	if exists(path) {
		t.Fatal("a download that failed its hash is in place")
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*")); len(m) > 0 {
		t.Fatalf("left behind: %v", m)
	}
}

// asideTaken: a real directory where the link goes, and its aside taken:
// the link fails, and both are left as they were.
func (h *box) asideTaken(t *testing.T) {
	const path, aside = "/etc/res-taken", "/etc/res-taken.orig"
	h.sh(t, "rm -rf "+path+" "+aside+" && mkdir -p "+path+" "+aside+" && echo mine > "+path+"/x && echo earlier > "+aside+"/x")
	got, err := hosttest.Run(hosttest.Apply, host.Play{Tasks: []host.Task{{Resource: &host.Symlink{Path: path, Target: "/etc/hostname", Aside: aside}}}})
	if err == nil || len(got) != 1 || got[0].Status != host.Failed {
		t.Fatalf("got %+v, %v; want a failure", got, err)
	}
	if err := h.fileIs(path+"/x", "mine\n", 0o644, host.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := h.fileIs(aside+"/x", "earlier\n", 0o644, host.Owner{}); err != nil {
		t.Fatal(err)
	}
}

// danglingLink: a line through a link to nothing fails, the link kept.
func (h *box) danglingLink(t *testing.T) {
	const link = "/etc/res-dangling"
	h.sh(t, "rm -f "+link+" && ln -s /nonexistent/res-fstab "+link)
	if _, err := hosttest.Run(hosttest.Apply, host.Play{Tasks: []host.Task{{Resource: &host.Line{Path: link, Line: "x"}}}}); err == nil {
		t.Fatal("a line through a link to nothing passed")
	}
	if got, err := os.Readlink(link); err != nil || got != "/nonexistent/res-fstab" {
		t.Fatalf("the link: %q %v", got, err)
	}
}

// listsAfterSources is nil when the lists were updated after every source
// and key last changed.
func (h *box) listsAfterSources() error {
	var updated time.Time
	for _, p := range []string{"/var/lib/apt/periodic/update-success-stamp", "/var/lib/apt/lists"} {
		if fi, err := os.Stat(p); err == nil {
			updated = fi.ModTime()
			break
		}
	}
	var paths []string
	for _, d := range []string{"/etc/apt/sources.list.d", "/etc/apt/keyrings", "/etc/apt/trusted.gpg.d"} {
		m, _ := filepath.Glob(d + "/*")
		paths = append(append(paths, d), m...)
	}
	for _, p := range append(paths, "/etc/apt/sources.list") {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(updated) {
			return fmt.Errorf("%s changed after the last update", p)
		}
	}
	return nil
}

// snapshot is what a run may change on the host: every path under the
// trees the cases write, with its mode, owner, size, time and link
// target, and dpkg's database.
func snapshot(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, root := range []string{"/etc", "/usr/local", "/srv", "/var/lib/apt", "/var/lib/dpkg/status", "/var/lib/res-made"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			o, _ := host.FileOwner(fi)
			v := fmt.Sprintf("%s %s %d %d", fi.Mode(), o, fi.Size(), fi.ModTime().UnixNano())
			if fi.Mode()&fs.ModeSymlink != 0 {
				l, _ := os.Readlink(p)
				v += " -> " + l
			}
			got[p] = v
			return nil
		})
	}
	return got
}

// diff is the paths that differ between two snapshots, "" for none.
func diff(a, b map[string]string) string {
	var out []string
	for p, v := range a {
		if b[p] != v {
			out = append(out, fmt.Sprintf("%s: %q -> %q", p, v, b[p]))
		}
	}
	for p, v := range b {
		if _, ok := a[p]; !ok {
			out = append(out, fmt.Sprintf("%s: new %q", p, v))
		}
	}
	slices.Sort(out)
	if len(out) > 10 {
		out = append(out[:10], fmt.Sprintf("and %d more", len(out)-10))
	}
	return strings.Join(out, "; ")
}

// aptLists updates the package lists once, for setups that install.
func (h *box) aptLists(t *testing.T) {
	h.lists.Do(func() { h.sh(t, "apt-get -q update") })
}

// sh runs script as the setup of a case; a failure ends the case.
func (h *box) sh(t *testing.T, script string) {
	t.Helper()
	if out, err := h.out(script); err != nil {
		t.Fatalf("setup %q: %v\n%s", script, err, out)
	}
}

func (h *box) out(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pkgStatus is nil when dpkg has the package in status ("ii", "rc"; ""
// is not known to dpkg at all, or known as not installed).
func (h *box) pkgStatus(name, status string) error {
	out, err := h.out("dpkg-query -W -f '${db:Status-Abbrev}' " + name)
	got := strings.TrimSpace(out)
	if err != nil || strings.HasPrefix(got, "u") || strings.HasPrefix(got, "p") {
		got = "" // unknown, or unknown and purged
	}
	if got != status {
		return fmt.Errorf("%s is %q, want %q", name, got, status)
	}
	return nil
}

// fileIs is nil when path is a regular file with content and mode, owned
// by owner.
func (h *box) fileIs(path, content string, mode fs.FileMode, owner host.Owner) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(got) != content {
		return fmt.Errorf("%s holds %q, want %q", path, got, content)
	}
	return h.modeOwner(path, mode, owner)
}

func (h *box) modeOwner(path string, mode fs.FileMode, owner host.Owner) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode() != mode {
		return fmt.Errorf("%s is %s, want %s", path, fi.Mode(), mode)
	}
	if got, _ := host.FileOwner(fi); got != owner {
		return fmt.Errorf("%s is owned by %s, want %s", path, got, owner)
	}
	return nil
}

func (h *box) sameFile(a, b string) error {
	x, err := os.ReadFile(a)
	if err != nil {
		return err
	}
	y, err := os.ReadFile(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(x, y) {
		return fmt.Errorf("%s differs from %s", a, b)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func notExists(path string) error {
	if exists(path) {
		return fmt.Errorf("%s exists", path)
	}
	return nil
}
