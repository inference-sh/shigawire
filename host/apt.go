package host

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Apt is a Debian-family host's packages: the resources it makes share
// one read of what is installed, read again after any of them changed it.
// One Apt serves any number of runs.
type Apt struct {
	Exec Exec
	// Root is where the host's filesystem is, as this process sees it; ""
	// is /, the host it runs on. Apt finds the files it names itself under
	// it (sources, keyrings, the lists); the commands it runs go through
	// Exec, which must run them on that same host (in a chroot of Root).
	Root string

	pkgs map[string]pkgState // nil until read, and after apt changed it
	// pkgsAt is dpkg's status file as it was when pkgs was read: one that
	// changed since (something outside a ran dpkg) makes pkgs stale.
	pkgsAt dpkgStamp
	// sourcesNew: a resource of a's added or changed a source or a key
	// since the last update, which is due now whatever the lists' age.
	sourcesNew bool
	// updatedIn is the run (runOf) in which an update of a's last
	// succeeded, nil before one did: in that run, an Update with maxAge 0
	// is not due again (unless sourcesNew).
	updatedIn *runMark
	// sleep waits between tries; nil waits for real (tests make it instant).
	sleep func(ctx context.Context, d time.Duration) error
}

// pkgState is one package as dpkg has it.
type pkgState struct {
	version string
	status  byte // dpkg's current-status letter: i installed, c config-files, ...
}

// installed: configured, or only waiting for triggers.
func (p pkgState) installed() bool { return p.status == 'i' || p.status == 'W' || p.status == 't' }

// present: some of the package is on the host: installed, or unpacked or
// configured only partway (a failed DKMS build leaves its package
// half-configured); with config, also its config files alone.
func (p pkgState) present(config bool) bool {
	return p.status != 'n' && (p.status != 'c' || config)
}

// rank orders the states of one package's architectures: the most
// present wins.
func (p pkgState) rank() int {
	switch {
	case p.installed():
		return 3
	case p.present(false):
		return 2
	case p.present(true):
		return 1
	}
	return 0
}

// noninteractiveEnv keeps apt, dpkg, debconf and needrestart from asking
// anything, and their messages in English: what installers that run apt
// need too.
func noninteractiveEnv() []string {
	return []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a", "LC_ALL=C.UTF-8", "LANG=C.UTF-8"}
}

func (a *Apt) path(p string) string { return UnderRoot(a.Root, p) }

// lockTimeout is how long apt waits for another apt to finish (a test
// shortens it).
var lockTimeout = 10 * time.Minute

func (a *Apt) wait(ctx context.Context, d time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// get runs apt-get with args: it asks nothing, keeps a config file the
// admin changed (dpkg's confdef, confold), and waits for another apt to
// finish. apt-get waits by itself only for dpkg's frontend lock, and fails
// at once on the lists' and the archives' (apt-daily at a cloud VM's first
// boot): get runs it again until lockTimeout has passed. A dpkg that was
// interrupted (an apt-get killed partway) is finished first, once.
func (a *Apt) get(ctx context.Context, timeout time.Duration, args ...string) (Output, error) {
	lock := lockTimeout
	base := []string{"-y", "-q",
		"-o", "DPkg::Lock::Timeout=" + strconv.Itoa(int(lock.Seconds())),
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}
	cmd := Cmd{Name: "apt-get", Args: append(base, args...), Env: noninteractiveEnv(), Timeout: timeout + lock}
	deadline := time.Now().Add(lock)
	finished := false
	for delay := time.Second; ; delay = min(2*delay, 30*time.Second) {
		o, err := run(ctx, a.Exec, cmd)
		switch {
		case err == nil:
			return o, nil
		case !finished && strings.Contains(o.Out, "dpkg was interrupted"):
			finished = true
			if _, err := run(ctx, a.Exec, Cmd{Name: "dpkg", Args: []string{"--configure", "-a", "--force-confdef", "--force-confold"},
				Env: noninteractiveEnv(), Timeout: timeout}); err != nil {
				return o, fmt.Errorf("dpkg was interrupted, and finishing it failed: %w", err)
			}
			continue
		case !aptLocked(o.Out) || time.Now().Add(delay).After(deadline):
			return o, err
		}
		if err := a.wait(ctx, delay); err != nil {
			return o, err
		}
	}
}

// aptLocked: apt-get failed for a lock another apt holds.
func aptLocked(out string) bool {
	return strings.Contains(out, "Could not get lock") || strings.Contains(out, "Unable to lock directory") ||
		strings.Contains(out, "Unable to acquire the dpkg frontend lock")
}

// dpkgStamp is dpkg's status file's time and size; zero when there is
// none.
type dpkgStamp struct {
	mod  time.Time
	size int64
}

func (a *Apt) dpkgStamp() dpkgStamp {
	fi, err := os.Stat(a.path("var/lib/dpkg/status"))
	if err != nil {
		return dpkgStamp{}
	}
	return dpkgStamp{fi.ModTime(), fi.Size()}
}

// read is what dpkg has installed: read once, and again after a's own
// apt-get ran, Invalidate, or dpkg's status file changed (an installer
// that ran apt or dpkg).
func (a *Apt) read(ctx context.Context) (map[string]pkgState, error) {
	stamp := a.dpkgStamp()
	if a.pkgs != nil && stamp == a.pkgsAt {
		return a.pkgs, nil
	}
	o, err := run(ctx, a.Exec, short("dpkg-query", "-W", "-f", `${Package}\t${db:Status-Abbrev}\t${Version}\n`))
	if err != nil {
		return nil, fmt.Errorf("installed packages: %w", err)
	}
	a.pkgs, a.pkgsAt = parseDpkgQuery(o.Out), stamp
	return a.pkgs, nil
}

// parseDpkgQuery reads dpkg-query's name, status, version lines. A package
// is as present as its most present architecture.
func parseDpkgQuery(out string) map[string]pkgState {
	pkgs := map[string]pkgState{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 3 || len(f[1]) < 2 {
			continue
		}
		p := pkgState{version: f[2], status: f[1][1]}
		if old, ok := pkgs[f[0]]; ok && old.rank() >= p.rank() {
			continue
		}
		pkgs[f[0]] = p
	}
	return pkgs
}

// Invalidate drops what a read of the installed packages: they are read
// again when next asked. For what changed them outside a's resources (a
// Command that runs apt-get: Command.Apt) on a host whose dpkg status
// file's time and size do not tell.
func (a *Apt) Invalidate() { a.pkgs = nil }

// Installed reports whether a package is installed, named as Install
// takes it: a name at any version, name=version at that one, name:arch
// for any architecture. It shares a's read of the installed packages.
func (a *Apt) Installed(ctx context.Context, name string) (bool, error) {
	pkgs, err := a.read(ctx)
	if err != nil {
		return false, err
	}
	return isInstalled(pkgs, name), nil
}

// isInstalled is Installed over what dpkg-query read.
func isInstalled(pkgs map[string]pkgState, n string) bool {
	name, version, pinned := strings.Cut(n, "=")
	name, _, _ = strings.Cut(name, ":")
	p := pkgs[name]
	return p.installed() && (!pinned || p.version == version)
}

// Update is the package lists, at most maxAge old (0: updated once each
// run of a Runner, by the first Update that runs; every time without a
// Runner), and updated whatever their age when a source or a key changed since the
// last update: through a's own Repo, or by its file's time. A fetch that
// fails fails the update (APT::Update::Error-Mode=any; apt-get itself
// only warns and exits 0 over the old lists), which is tried updateTries
// times. It is an action: the lists' age is no state Verify reports.
func (a *Apt) Update(maxAge time.Duration) Resource { return &aptUpdate{a: a, maxAge: maxAge} }

type aptUpdate struct {
	a      *Apt
	maxAge time.Duration
}

const updateTries = 3

// listsStamps are the files whose time is the last update's, first found
// first: Ubuntu's apt stamps a successful update, and the lists' directory
// is the fallback, as Ansible's apt module has it.
func listsStamps() []string {
	return []string{"var/lib/apt/periodic/update-success-stamp", "var/lib/apt/lists"}
}

// sourceDirs hold what apt reads its sources and their keys from. A
// directory's own time is when an entry was added, renamed or removed (a
// .deb's file has the time it was built with).
func sourceDirs() []string {
	return []string{"etc/apt/sources.list.d", "etc/apt/keyrings", "etc/apt/trusted.gpg.d", "usr/share/keyrings"}
}

func (u *aptUpdate) ID() string { return "apt update" }

// Check's Data is why the update is due ("" when every run updates).
func (u *aptUpdate) Check(ctx context.Context) (State, error) {
	if due, why := u.due(ctx); due {
		st, err := driftData(why, "%s", cmp.Or(why, "update"))
		st.Action = true
		return st, err
	}
	return inPlace()
}

// due is whether the lists need an update, and why ("" when every run
// does).
func (u *aptUpdate) due(ctx context.Context) (bool, string) {
	switch {
	case u.a.sourcesNew:
		return true, "a source changed"
	case u.maxAge == 0:
		return u.a.updatedIn != runOf(ctx), ""
	}
	// A fresh image's lists are removed and the directory left, as new as
	// the image: its age says nothing.
	if got, _ := filepath.Glob(u.a.path("var/lib/apt/lists/*_Packages*")); len(got) == 0 {
		return true, "no lists"
	}
	updated, ok := u.a.updated()
	switch {
	case !ok:
		return true, "no lists"
	case u.a.sourcesChanged().After(updated):
		return true, "a source changed"
	case time.Since(updated) > u.maxAge:
		return true, fmt.Sprintf("lists %s old", time.Since(updated).Round(time.Minute))
	}
	return false, ""
}

// updated is when the lists were last updated.
func (a *Apt) updated() (time.Time, bool) {
	for _, p := range listsStamps() {
		if fi, err := os.Stat(a.path(p)); err == nil {
			return fi.ModTime(), true
		}
	}
	return time.Time{}, false
}

// sourcesChanged is when a source or a key last changed: the newest of
// sources.list, the source directories and what is in them.
func (a *Apt) sourcesChanged() time.Time {
	var newest time.Time
	see := func(fi os.FileInfo) {
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if fi, err := os.Stat(a.path("etc/apt/sources.list")); err == nil {
		see(fi)
	}
	for _, d := range sourceDirs() {
		fi, err := os.Stat(a.path(d))
		if err != nil {
			continue
		}
		see(fi)
		entries, _ := os.ReadDir(a.path(d))
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				see(fi)
			}
		}
	}
	return newest
}

func (u *aptUpdate) Apply(ctx context.Context, st State) (Result, error) {
	why := st.Data.(string)
	var err error
	for try := 1; try <= updateTries; try++ {
		if _, err = u.a.get(ctx, 10*time.Minute, "-o", "APT::Update::Error-Mode=any", "update"); err == nil {
			break
		}
		if try == updateTries {
			return Result{}, fmt.Errorf("%d tries: %w", updateTries, err)
		}
		if err := u.a.wait(ctx, time.Duration(try)*10*time.Second); err != nil {
			return Result{}, err
		}
	}
	// An update that fetched nothing new leaves the lists' directory as it
	// was, and without Ubuntu's stamp hook a stamp is not renewed: dated
	// here, the lists are as fresh as the update that just succeeded.
	u.a.sourcesNew, u.a.updatedIn = false, runOf(ctx)
	now := time.Now()
	for _, p := range listsStamps() {
		if err := os.Chtimes(u.a.path(p), now, now); err != nil && !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	if why == "" {
		return changed("updated")
	}
	return changed("updated: %s", why)
}

// aptOp is an apt resource: Check lists what is left to do (the packages
// missing), Apply runs apt-get on that list.
type aptOp struct {
	a  *Apt
	id string
	// todo is what is left to do, for the messages; args are apt-get's
	// arguments for it.
	todo func(ctx context.Context) ([]string, error)
	args func(todo []string) []string
	// verb and done say it in the messages ("install", "installed").
	verb, done string
	// update, when set, runs first when it is due and there is
	// something to do.
	update *aptUpdate
}

func (o *aptOp) ID() string { return o.id }

// Check's Data is the list left to do.
func (o *aptOp) Check(ctx context.Context) (State, error) {
	todo, err := o.todo(ctx)
	if err != nil || len(todo) == 0 {
		return State{InPlace: err == nil}, err
	}
	first := ""
	if o.update != nil {
		if due, _ := o.update.due(ctx); due {
			first = "update, "
		}
	}
	return driftData(todo, "%s%s %s", first, o.verb, strings.Join(todo, " "))
}

func (o *aptOp) Apply(ctx context.Context, st State) (Result, error) {
	todo := st.Data.([]string)
	first := ""
	if o.update != nil {
		if due, why := o.update.due(ctx); due {
			if _, err := o.update.Apply(ctx, State{Data: why}); err != nil {
				return Result{}, fmt.Errorf("apt update: %w", err)
			}
			first = "updated, "
		}
	}
	defer o.a.Invalidate()
	if _, err := o.a.get(ctx, 30*time.Minute, o.args(todo)...); err != nil {
		return Result{}, err
	}
	return changed("%s%s %s", first, o.done, strings.Join(todo, " "))
}

// Install is packages installed: a name at any version, a name=version at
// that one (as apt-get takes it); a name:arch is installed for any
// architecture. Only the missing ones go to apt-get, with what they
// recommend, as apt does. A virtual package's name never matches what
// dpkg has installed: name the package that provides it.
func (a *Apt) Install(names ...string) Resource {
	return &aptOp{a: a, id: "apt install " + strings.Join(names, " "), verb: "install", done: "installed",
		args: func(m []string) []string { return append([]string{"install"}, m...) },
		todo: func(ctx context.Context) ([]string, error) {
			pkgs, err := a.read(ctx)
			if err != nil {
				return nil, err
			}
			var missing []string
			for _, n := range names {
				if !isInstalled(pkgs, n) {
					missing = append(missing, n)
				}
			}
			return missing, nil
		}}
}

// InstallUpdating is Install, with the package lists updated first, as
// Update(maxAge) has them, when something is to be installed: a host
// that has the packages needs no new lists.
func (a *Apt) InstallUpdating(maxAge time.Duration, names ...string) Resource {
	o := a.Install(names...).(*aptOp)
	o.update = &aptUpdate{a: a, maxAge: maxAge}
	return o
}

// Remove is the named packages not on the host, nor any part of them:
// installed, or unpacked or configured only partway (what a failed
// install leaves). purge removes their config files too, and purges
// those whose config files are all that is left.
func (a *Apt) Remove(names []string, purge bool) Resource {
	desc := strings.Join(names, " ")
	o := &aptOp{a: a, id: "apt remove " + desc, verb: "remove", done: "removed",
		args: func(p []string) []string { return append([]string{"remove"}, p...) },
		todo: func(ctx context.Context) ([]string, error) {
			pkgs, err := a.read(ctx)
			if err != nil {
				return nil, err
			}
			var out []string
			for n, p := range pkgs {
				if p.present(purge) && slices.Contains(names, n) {
					out = append(out, n)
				}
			}
			slices.Sort(out)
			return out, nil
		}}
	if purge {
		o.id = "apt purge " + desc
		o.args = func(p []string) []string { return append([]string{"remove", "--purge"}, p...) }
	}
	return o
}

// Repo is an apt source in deb822 form, signed by a key fetched from
// KeyURL each run (a rotated key is picked up): the key goes in
// KeyringDir/<Name>.asc (.gpg for a binary key), the source in
// /etc/apt/sources.list.d/<Name>.sources, naming the key as Signed-By.
type Repo struct {
	Name       string
	URIs       []string
	Suites     []string
	Components []string
	KeyURL     string
	// Key is the key itself, in place of KeyURL: one the engine carries.
	Key []byte
	// Client fetches the key; nil is one that gives up after a minute.
	Client *http.Client
	// KeyringDir is the directory the key goes in, as the host knows it;
	// "" is /etc/apt/keyrings. It is made when missing.
	KeyringDir string
}

// Sources is the repo's .sources file, signed by the key at keyPath.
func (r Repo) Sources(keyPath string) string {
	var b strings.Builder
	field := func(k string, v []string) {
		if len(v) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(v, " "))
		}
	}
	field("Types", []string{"deb"})
	field("URIs", r.URIs)
	field("Suites", r.Suites)
	field("Components", r.Components)
	field("Signed-By", []string{keyPath})
	return b.String()
}

// Repo is r on the host. A changed key or source makes the next Update
// due, whatever the lists' age.
func (a *Apt) Repo(r Repo) Resource { return &aptRepo{a: a, r: r} }

type aptRepo struct {
	a *Apt
	r Repo
}

func (r *aptRepo) ID() string { return "apt repo " + r.r.Name }

// files are the key and source files as they should be, the paths the
// host knows them by.
func (r *aptRepo) files(ctx context.Context) ([]File, error) {
	key, err := r.fetchKey(ctx)
	if err != nil {
		return nil, err
	}
	keyPath, err := r.r.KeyPath(key)
	if err != nil {
		return nil, fmt.Errorf("the key for %s: %s: %w", r.r.Name, cmp.Or(r.r.KeyURL, "the engine's"), err)
	}
	return []File{
		{Path: r.a.path(keyPath), Content: key, Mode: 0o644, Parents: true},
		{Path: r.a.path(r.r.SourcesPath()), Content: []byte(r.r.Sources(keyPath)), Mode: 0o644, Parents: true},
	}, nil
}

// SourcesPath is where the host has r's source.
func (r Repo) SourcesPath() string { return "/etc/apt/sources.list.d/" + r.Name + ".sources" }

// KeyPath is where the host has r's key, when the key is key.
func (r Repo) KeyPath(key []byte) (string, error) {
	ext, err := keyExt(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(cmp.Or(r.KeyringDir, "/etc/apt/keyrings"), r.Name+ext), nil
}

func (r *aptRepo) fetchKey(ctx context.Context) ([]byte, error) {
	if r.r.Key != nil {
		return r.r.Key, nil
	}
	client := r.r.Client
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	key, err := fetch(ctx, client, r.r.KeyURL, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("the key for %s: %w", r.r.Name, err)
	}
	return key, nil
}

// keyExt is the keyring file's extension for key, as apt tells them apart:
// .asc for an armored public key, .gpg for a binary one (its first OpenPGP
// packet a public key). Anything else is no key: an armored signature, an
// error page served with 200.
func keyExt(key []byte) (string, error) {
	if bytes.HasPrefix(bytes.TrimSpace(key), []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		return ".asc", nil
	}
	if len(key) > 0 && key[0]&0x80 != 0 {
		tag := key[0] >> 2 & 0x0f // the old packet format
		if key[0]&0x40 != 0 {
			tag = key[0] & 0x3f // the new one
		}
		if tag == 6 {
			return ".gpg", nil
		}
	}
	return "", errors.New("not an OpenPGP public key")
}

// Check's Data is the files to write: the key it fetched, the source.
func (r *aptRepo) Check(ctx context.Context) (State, error) {
	files, err := r.files(ctx)
	if err != nil {
		return State{}, err
	}
	var all []string
	var todo []fileTodo
	for i := range files {
		st, err := files[i].Check(ctx)
		if err != nil {
			return State{}, err
		}
		if !st.InPlace {
			all = append(all, filepath.Base(files[i].Path)+": "+st.Drift)
			todo = append(todo, fileTodo{&files[i], st})
		}
	}
	if len(todo) == 0 {
		return inPlace()
	}
	return driftData(todo, "%s", strings.Join(all, "; "))
}

// fileTodo is a file to write and what its Check found.
type fileTodo struct {
	f  *File
	st State
}

func (r *aptRepo) Apply(ctx context.Context, st State) (Result, error) {
	var did []string
	for _, t := range st.Data.([]fileTodo) {
		if _, err := t.f.Apply(ctx, t.st); err != nil {
			return Result{}, err
		}
		did = append(did, filepath.Base(t.f.Path))
	}
	r.a.sourcesNew = true
	return changed("wrote %s", strings.Join(did, ", "))
}
