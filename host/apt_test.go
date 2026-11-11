package host

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeDpkg is a host's dpkg database behind a fakeExec: dpkg-query reads
// it, apt-get install and remove change it.
type fakeDpkg struct {
	pkgs map[string]string // name -> "<status abbrev>\t<version>"
	ex   *fakeExec
}

func newFakeDpkg(pkgs map[string]string) *fakeDpkg {
	d := &fakeDpkg{pkgs: pkgs}
	d.ex = &fakeExec{respond: d.respond}
	return d
}

func (d *fakeDpkg) respond(line string) Output {
	f := strings.Fields(line)
	switch {
	case f[0] == "dpkg-query":
		var b strings.Builder
		for _, n := range slices.Sorted(maps.Keys(d.pkgs)) {
			fmt.Fprintf(&b, "%s\t%s\n", n, d.pkgs[n])
		}
		return Output{Out: b.String()}
	case f[0] == "apt-get":
		args := slices.DeleteFunc(f[1:], func(s string) bool { return strings.HasPrefix(s, "-") || strings.Contains(s, "::") })
		switch args[0] {
		case "install":
			for _, n := range args[1:] {
				if name, v, ok := strings.Cut(n, "="); ok {
					d.pkgs[name] = "ii \t" + v
				} else {
					d.pkgs[n] = "ii \t1.0"
				}
			}
		case "remove":
			for _, n := range args[1:] {
				if strings.Contains(line, "--purge") {
					delete(d.pkgs, n)
				} else {
					d.pkgs[n] = "rc \t1.0"
				}
			}
		}
	}
	return Output{}
}

func TestApt_InstallOnlyTheMissing(t *testing.T) {
	d := newFakeDpkg(map[string]string{
		"dkms":          "ii \t3.0",
		"linux-headers": "rc \t6.8", // config files only: not installed
		"half":          "iF \t1.0", // half-configured: apt finishes it
	})
	a := &Apt{Exec: d.ex}
	r := converge(t, a.Install("dkms", "linux-headers", "half"), Changed)
	if r.Msg != "installed linux-headers half" {
		t.Fatalf("msg %q", r.Msg)
	}
	inst := d.ex.ran("apt-get")
	if len(inst) != 1 || !strings.HasSuffix(inst[0], " install linux-headers half") {
		t.Fatalf("ran %v", inst)
	}
	for _, want := range []string{"-y", "DPkg::Lock::Timeout=600", "--force-confold"} {
		if !strings.Contains(inst[0], want) {
			t.Errorf("apt-get without %s: %s", want, inst[0])
		}
	}
	// What is installed is read once, and again after apt changed it.
	if n := len(d.ex.ran("dpkg-query")); n != 2 {
		t.Fatalf("read the packages %d times, want 2", n)
	}
	got, _ := a.read(context.Background())
	if !got["half"].installed() || got["dkms"].version != "3.0" {
		t.Fatalf("packages %v", got)
	}
}

// A name=version is installed at that version only.
func TestApt_InstallPinned(t *testing.T) {
	d := newFakeDpkg(map[string]string{"nvidia-open": "ii \t580.1-1"})
	a := &Apt{Exec: d.ex}
	r := converge(t, a.Install("nvidia-open=615.71-1"), Changed)
	if r.Msg != "installed nvidia-open=615.71-1" {
		t.Fatalf("msg %q", r.Msg)
	}
}

func TestApt_RemoveMatching(t *testing.T) {
	d := newFakeDpkg(map[string]string{
		"nvidia-driver-570-server":        "ii \t570",
		"libnvidia-compute-570-server":    "ii \t570",
		"nvidia-container-toolkit":        "ii \t1.17",
		"nvidia-utils-550-server":         "rc \t550", // config files left
		"linux-modules-nvidia-570-server": "ii \t6.8",
	})
	sel := []string{"nvidia-driver-570-server", "libnvidia-compute-570-server", "nvidia-utils-550-server", "linux-modules-nvidia-570-server", "nvidia-absent-server"}
	a := &Apt{Exec: d.ex}
	if r := converge(t, a.Remove(sel, true), Changed); r.Msg != "removed libnvidia-compute-570-server linux-modules-nvidia-570-server nvidia-driver-570-server nvidia-utils-550-server" {
		t.Fatalf("msg %q", r.Msg)
	}
	if !strings.Contains(d.ex.ran("apt-get")[0], " remove --purge ") {
		t.Fatalf("ran %v", d.ex.ran("apt-get"))
	}
	if _, ok := d.pkgs["nvidia-container-toolkit"]; !ok {
		t.Fatal("removed what the selection leaves out")
	}

	// Remove without purge leaves config-file packages alone.

	d2 := newFakeDpkg(map[string]string{"docker-ce": "ii \t27", "containerd.io": "rc \t1"})
	converge(t, (&Apt{Exec: d2.ex}).Remove([]string{"docker-ce", "containerd.io"}, false), Changed)
	if got := d2.ex.ran("apt-get"); len(got) != 1 || !strings.HasSuffix(got[0], " remove docker-ce") {
		t.Fatalf("ran %v", got)
	}
}

func TestApt_Update(t *testing.T) {
	root := t.TempDir()
	stamp := filepath.Join(root, "var/lib/apt/periodic/update-success-stamp")
	lists := filepath.Join(root, "var/lib/apt/lists")
	ex := &fakeExec{respond: func(line string) Output {
		if strings.HasPrefix(line, "apt-get") {
			os.MkdirAll(filepath.Dir(stamp), 0o755)
			os.WriteFile(stamp, nil, 0o644)
			os.WriteFile(filepath.Join(lists, "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages"), nil, 0o644)
		}
		return Output{}
	}}
	// A fresh image: the lists removed, their directory new.
	os.MkdirAll(lists, 0o755)
	a := &Apt{Exec: ex, Root: root}
	u := a.Update(time.Hour)
	if r := converge(t, u, Changed); r.Msg != "updated: no lists" {
		t.Fatalf("msg %q", r.Msg)
	}
	if got := ex.ran("apt-get"); !strings.Contains(got[0], "-o APT::Update::Error-Mode=any update") {
		t.Fatalf("ran %v", got)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stamp, old, old)
	if r := converge(t, u, Changed); !strings.HasPrefix(r.Msg, "updated: lists 2h") {
		t.Fatalf("msg %q", r.Msg)
	}
	if r := apply(t, a.Update(0)); r.Status != Changed {
		t.Fatal("maxAge 0 is not every run")
	}
}

// A source or key that changed after the last update makes fresh lists
// due, in any later run: the host's files hold the fact.
func TestApt_UpdateAfterASourceChanged(t *testing.T) {
	root := t.TempDir()
	lists := filepath.Join(root, "var/lib/apt/lists")
	os.MkdirAll(lists, 0o755)
	os.WriteFile(filepath.Join(lists, "x_Packages"), nil, 0o644)
	fresh := time.Now().Add(-time.Minute)
	os.Chtimes(lists, fresh, fresh)
	for _, d := range []string{"etc/apt/sources.list.d", "etc/apt/keyrings"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
		os.Chtimes(filepath.Join(root, d), fresh.Add(-time.Hour), fresh.Add(-time.Hour))
	}
	failing := true
	ex := &fakeExec{respond: func(line string) Output {
		if failing && strings.HasPrefix(line, "apt-get") {
			return Output{Code: 100, Out: "E: Failed to fetch https://nvidia.github.io/...  Could not resolve host"}
		}
		return Output{}
	}}
	a := &Apt{Exec: ex, Root: root, sleep: func(context.Context, time.Duration) error { return nil }}
	if st, _ := a.Update(time.Hour).Check(context.Background()); !st.InPlace {
		t.Fatalf("fresh lists, no source changed: %+v", st)
	}

	// A run adds a repo, and its update fails, three times: the lists'
	// times stay as they were.
	key := testKey
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, key) }))
	defer srv.Close()
	play := func(a *Apt) Play {
		return Play{Tasks: []Task{
			{Resource: a.Repo(Repo{Name: "nvidia", URIs: []string{"https://nvidia.github.io/x"}, Suites: []string{"/"}, KeyURL: srv.URL})},
			{Resource: a.Update(time.Hour)},
		}}
	}
	if _, err := runPlay(context.Background(), play(a)); err == nil || !strings.Contains(err.Error(), "3 tries") {
		t.Fatalf("err %v", err)
	}
	if n := len(ex.ran("apt-get")); n != updateTries {
		t.Fatalf("tried %d times", n)
	}
	if fi, _ := os.Stat(lists); !fi.ModTime().Equal(fresh) {
		t.Fatal("a failed update dated the lists")
	}

	// The next run, a new Apt: the repo is in place, and the update is
	// still due.
	failing = false
	got, err := runPlay(context.Background(), play(&Apt{Exec: ex, Root: root}))
	if err != nil || got[0].Status != OK || got[1].Status != Changed || got[1].Msg != "updated: a source changed" {
		t.Fatalf("second run %+v %v", got, err)
	}
	if st, _ := (&Apt{Exec: ex, Root: root}).Update(time.Hour).Check(context.Background()); !st.InPlace {
		t.Fatalf("after the update: %+v", st)
	}
}

// apt-get fails at once on the lists' and the archives' locks: it is run
// again while another apt holds them, until lockTimeout.
func TestApt_WaitsForTheLocks(t *testing.T) {
	locked := 2
	ex := &fakeExec{respond: func(line string) Output {
		if strings.HasPrefix(line, "apt-get") && locked > 0 {
			locked--
			return Output{Code: 100, Out: "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 812 (apt-get)\nE: Unable to lock directory /var/lib/apt/lists/"}
		}
		return Output{}
	}}
	var slept []time.Duration
	a := &Apt{Exec: ex, sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	if _, err := a.get(context.Background(), time.Minute, "update"); err != nil {
		t.Fatal(err)
	}
	if len(ex.ran("apt-get")) != 3 || len(slept) != 2 || slept[1] != 2*time.Second {
		t.Fatalf("ran %v, slept %v", ex.ran("apt-get"), slept)
	}

	// Past lockTimeout it gives up; another failure is not retried.
	locked = 1000
	defer func(old time.Duration) { lockTimeout = old }(lockTimeout)
	lockTimeout = 3 * time.Second
	a = &Apt{Exec: ex, sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := a.get(context.Background(), time.Minute, "update"); err == nil || !strings.Contains(err.Error(), "Could not get lock") {
		t.Fatalf("err %v", err)
	}
	ex.calls = nil
	locked = 0
	ex.respond = func(string) Output { return Output{Code: 100, Out: "E: Unable to locate package nope"} }
	if _, err := a.get(context.Background(), time.Minute, "install", "nope"); err == nil || len(ex.calls) != 1 {
		t.Fatalf("err %v, ran %v", err, ex.calls)
	}
}

// An apt-get killed partway leaves dpkg interrupted: it is finished
// (dpkg --configure -a) once, and apt-get run again.
func TestApt_FinishesAnInterruptedDpkg(t *testing.T) {
	interrupted := true
	ex := &fakeExec{respond: func(line string) Output {
		switch {
		case strings.HasPrefix(line, "dpkg --configure -a"):
			interrupted = false
		case strings.HasPrefix(line, "apt-get") && interrupted:
			return Output{Code: 100, Out: "E: dpkg was interrupted, you must manually run 'dpkg --configure -a' to correct the problem."}
		}
		return Output{}
	}}
	if _, err := (&Apt{Exec: ex}).get(context.Background(), time.Minute, "install", "dkms"); err != nil {
		t.Fatal(err)
	}
	if len(ex.ran("dpkg --configure -a")) != 1 || len(ex.ran("apt-get")) != 2 {
		t.Fatalf("ran %v", ex.calls)
	}
}

// An update that fetches nothing new touches neither the lists' directory
// nor (without Ubuntu's hook) the stamp; the lists are fresh after it all
// the same.
func TestApt_UpdateNothingNew(t *testing.T) {
	root := t.TempDir()
	lists := filepath.Join(root, "var/lib/apt/lists")
	os.MkdirAll(lists, 0o755)
	os.WriteFile(filepath.Join(lists, "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages"), nil, 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(lists, old, old)
	ex := &fakeExec{}
	converge(t, (&Apt{Exec: ex, Root: root}).Update(time.Hour), Changed)
	if got := ex.ran("apt-get"); len(got) != 1 {
		t.Fatalf("ran %v", got)
	}
}

const testKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nmQINBF\n-----END PGP PUBLIC KEY BLOCK-----\n"

func TestApt_Repo(t *testing.T) {
	key := testKey
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; fmt.Fprint(w, key) }))
	defer srv.Close()
	root := t.TempDir()
	a := &Apt{Exec: &fakeExec{}, Root: root}
	repo := a.Repo(Repo{
		Name:   "nvidia-container-toolkit",
		URIs:   []string{"https://nvidia.github.io/libnvidia-container/stable/deb/amd64"},
		Suites: []string{"/"},
		KeyURL: srv.URL,
	})
	if apply(t, repo); hits != 1 {
		t.Fatalf("fetched the key %d times for one run, want 1", hits)
	}
	if !a.sourcesNew {
		t.Fatal("a new repo does not make the update due")
	}
	src, _ := os.ReadFile(filepath.Join(root, "etc/apt/sources.list.d/nvidia-container-toolkit.sources"))
	want := "Types: deb\nURIs: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\nSuites: /\nSigned-By: /etc/apt/keyrings/nvidia-container-toolkit.asc\n"
	if string(src) != want {
		t.Fatalf("sources %q", src)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/apt/keyrings/nvidia-container-toolkit.asc")); string(b) != key {
		t.Fatalf("key %q", b)
	}

	// A rotated key is picked up.
	key = strings.Replace(testKey, "mQINBF", "mQINBG", 1)
	if r := converge(t, repo, Changed); r.Msg != "wrote nvidia-container-toolkit.asc" {
		t.Fatalf("rotated: %q", r.Msg)
	}

	// Something that is not a key is refused.
	for _, k := range []string{"<html>rate limited</html>", "-----BEGIN PGP SIGNATURE-----\niQIz\n-----END PGP SIGNATURE-----\n", "\x88\x01"} {
		key = k
		if _, err := repo.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not an OpenPGP public key") {
			t.Fatalf("%q: err %v", k, err)
		}
	}
}

func TestKeyExt(t *testing.T) {
	for key, want := range map[string]string{
		testKey:                       ".asc",
		"\x99\x02\x0d\x04":            ".gpg", // old packet format, tag 6
		"\xc6\x33\x04":                ".gpg", // new format
		"\x89\x02\x33":                "",     // a signature (tag 2)
		"-----BEGIN PGP MESSAGE-----": "",
		"":                            "",
	} {
		if got, _ := keyExt([]byte(key)); got != want {
			t.Errorf("%q: %q, want %q", key, got, want)
		}
	}
}

// What a failed install leaves (a DKMS build that failed: half-configured,
// unpacked, half-installed) is removed: the next rung's install conflicts
// with it.
func TestApt_RemoveWhatAFailedInstallLeft(t *testing.T) {
	d := newFakeDpkg(map[string]string{
		"nvidia-dkms-580-server":   "iF \t580",
		"nvidia-kernel-580-server": "iU \t580",
		"nvidia-utils-580-server":  "iH \t580",
		"nvidia-compute-580":       "rc \t580",
		"nvidia-gone-580":          "pn \t",
	})
	sel := []string{"nvidia-dkms-580-server", "nvidia-kernel-580-server", "nvidia-utils-580-server", "nvidia-compute-580", "nvidia-gone-580"}
	if r := converge(t, (&Apt{Exec: d.ex}).Remove(sel, false), Changed); r.Msg != "removed nvidia-dkms-580-server nvidia-kernel-580-server nvidia-utils-580-server" {
		t.Fatalf("remove: %q", r.Msg)
	}
	if r := converge(t, (&Apt{Exec: d.ex}).Remove(sel, true), Changed); !strings.Contains(r.Msg, "nvidia-compute-580") || strings.Contains(r.Msg, "nvidia-gone") {
		t.Fatalf("purge: %q", r.Msg)
	}
}

// A name:arch is installed when dpkg has the name; a package installed for
// one architecture of several is installed.
func TestApt_InstallArch(t *testing.T) {
	d := newFakeDpkg(map[string]string{"libc6": "ii \t2.36"})
	if st, err := (&Apt{Exec: d.ex}).Install("libc6:amd64").Check(context.Background()); err != nil || !st.InPlace {
		t.Fatalf("%+v %v", st, err)
	}
	pkgs := parseDpkgQuery("libc6\tii \t2.36\nlibc6\tun \t\nlibfoo\tun \t\nlibfoo\tiU \t1\n")
	if !pkgs["libc6"].installed() || pkgs["libfoo"].status != 'U' {
		t.Fatalf("%+v", pkgs)
	}
}
