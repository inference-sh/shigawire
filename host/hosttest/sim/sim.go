// Package sim is a simulated Debian host for tests of plays and
// playbooks: hosttest's temporary root, and an Exec that answers the
// commands package host's resources run by changing what the host holds, so a
// second run sees what the first one did.
//
// It answers dpkg-query and apt-get (packages), systemctl (units),
// getent, id, useradd, usermod, userdel, groupadd, groupmod and groupdel
// (accounts), sysctl (kernel parameters), ufw (writing ufw's own files, as
// host.UFW reads them) and docker's swarm and network commands. Any other
// command goes to Fallback, else succeeds with no output, as hosttest.Exec
// answers it. hosttest.Exec's On rules come before the simulation, so a test
// can still give one command another answer.
//
// It is a model, not Debian: it keeps no package catalog (any name
// installs), a sysctl key never set reads "0", and every account gets the
// test process's uid and gid unless one is given, so files can be given
// to accounts in a temporary root without privilege.
package sim

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

// Unit is a systemd unit's state.
type Unit struct {
	Enabled, Active bool
	// Restarts and Reloads count systemctl restart and reload.
	Restarts, Reloads int
}

// User is an account in the user database.
type User struct {
	UID, GID             int
	Comment, Home, Shell string
	// Password is the shadow password field: useradd sets "!" unless
	// given --password.
	Password string
}

// Group is a group in the group database.
type Group struct {
	GID     int
	Members []string
}

// Host is a simulated Debian host. Its maps are what the host holds; a
// test may set them before a run and read them after.
type Host struct {
	*hosttest.Host

	// Packages are the installed packages.
	Packages map[string]bool
	Units    map[string]*Unit
	Users    map[string]*User
	Groups   map[string]*Group
	Sysctl   map[string]string
	// Swarm is the node's swarm state: inactive (the default), active,
	// pending, error or locked.
	Swarm string
	// Networks are the docker networks.
	Networks map[string]bool
	// DaemonReloads counts systemctl daemon-reload.
	DaemonReloads int
	// OnInstall runs when apt-get installs the package: what the package
	// puts on the host. New sets it for ufw and nginx.
	OnInstall map[string]func(h *Host)
	// Fallback answers a command the simulation does not know; nil:
	// success, no output.
	Fallback func(c host.Cmd) host.Output

	mu sync.Mutex
}

// stockDirs are the directories a Debian install has that resources write
// into without making them.
var stockDirs = []string{"/etc/apt/apt.conf.d", "/etc/apt/keyrings", "/etc/apt/sources.list.d", "/etc/cron.d", "/etc/default",
	"/etc/ssh/sshd_config.d", "/etc/sudoers.d", "/etc/sysctl.d", "/etc/systemd/system", "/home", "/root", "/tmp",
	"/usr/local/bin", "/usr/local/sbin", "/usr/share/keyrings", "/var/lib/apt/lists"}

// New is a fresh host for t: root, the groups a Debian install has, its
// stock directories, an fstab, /etc/os-release of Debian 13, no packages.
func New(t testing.TB) *Host {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	h := &Host{
		Host:     hosttest.New(t),
		Packages: map[string]bool{},
		Units:    map[string]*Unit{},
		Users:    map[string]*User{"root": {UID: uid, GID: gid, Comment: "root", Home: "/root", Shell: "/bin/bash", Password: "*"}},
		Groups:   map[string]*Group{"root": {GID: gid}},
		Sysctl:   map[string]string{},
		Swarm:    "inactive",
		Networks: map[string]bool{},
		OnInstall: map[string]func(*Host){
			"ufw":   installUFW,
			"nginx": installNginx,
		},
	}
	for _, g := range []string{"adm", "sudo", "users", "systemd-journal", "www-data"} {
		h.Groups[g] = &Group{GID: gid}
	}
	for _, d := range stockDirs {
		if err := os.MkdirAll(h.Path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.Write("/etc/fstab", "UUID=0 / ext4 errors=remount-ro 0 1\n")
	h.Write("/etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\nVERSION_CODENAME=trixie\n")
	h.Exec.RespondCmd = h.Run
	return h
}

// Unit is the named unit, made (disabled, inactive) when missing.
func (h *Host) Unit(name string) *Unit {
	name = strings.TrimSuffix(name, ".service")
	u := h.Units[name]
	if u == nil {
		u = &Unit{}
		h.Units[name] = u
	}
	return u
}

func ok(out string) host.Output { return host.Output{Out: out} }

func fail(code int, out string) host.Output { return host.Output{Code: code, Out: out} }

// Run answers c as the host would, changing what it holds.
func (h *Host) Run(c host.Cmd) host.Output {
	h.mu.Lock()
	o, known := h.run(c)
	fallback := h.Fallback
	h.mu.Unlock()
	if !known && fallback != nil {
		return fallback(c)
	}
	return o
}

func (h *Host) run(c host.Cmd) (host.Output, bool) {
	a := c.Args
	switch c.Name {
	case "dpkg-query":
		return h.dpkgQuery(a), true
	case "apt-get":
		return h.aptGet(a), true
	case "systemctl":
		return h.systemctl(a), true
	case "getent":
		return h.getent(a), true
	case "id":
		return h.id(a), true
	case "useradd", "usermod", "userdel", "groupadd", "groupmod", "groupdel":
		return h.account(c.Name, a), true
	case "sysctl":
		return h.sysctl(a), true
	case "ufw":
		return h.ufw(a), true
	case "docker":
		return h.docker(a)
	}
	return host.Output{}, false
}

func arg(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}

// dpkg-query -W -f FORMAT [NAME]
func (h *Host) dpkgQuery(a []string) host.Output {
	format, names := "", []string(nil)
	for i := 0; i < len(a); i++ {
		switch {
		case a[i] == "-W":
		case a[i] == "-f":
			i++
			format = arg(a, i)
		case strings.HasPrefix(a[i], "-f="):
			format = strings.TrimPrefix(a[i], "-f=")
		default:
			names = append(names, a[i])
		}
	}
	if len(names) == 0 {
		var b strings.Builder
		for _, n := range slices.Sorted(maps.Keys(h.Packages)) {
			if h.Packages[n] {
				fmt.Fprintf(&b, "%s\tii \t1.0\n", n)
			}
		}
		return ok(b.String())
	}
	var b strings.Builder
	for _, n := range names {
		re, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(n), `\*`, ".*") + "$")
		if err != nil {
			return fail(2, err.Error())
		}
		found := false
		for _, p := range slices.Sorted(maps.Keys(h.Packages)) {
			if h.Packages[p] && re.MatchString(p) {
				found = true
				out := strings.NewReplacer(`${Package}`, p, `${Status}`, "install ok installed", `${db:Status-Abbrev}`, "ii ",
					`${Version}`, "1.0", `\n`, "\n", `\t`, "\t").Replace(format)
				b.WriteString(out)
			}
		}
		if !found && !strings.Contains(n, "*") {
			return fail(1, "dpkg-query: no packages found matching "+n)
		}
	}
	return ok(b.String())
}

// apt-get [options] update|install|remove [--purge] PKG...
func (h *Host) aptGet(a []string) host.Output {
	var words []string
	for i := 0; i < len(a); i++ {
		switch {
		case a[i] == "-o":
			i++
		case strings.HasPrefix(a[i], "-"):
		default:
			words = append(words, a[i])
		}
	}
	if len(words) == 0 {
		return fail(100, "E: no operation")
	}
	switch words[0] {
	case "update":
		return ok("Reading package lists...\n")
	case "install":
		for _, p := range words[1:] {
			name, _, _ := strings.Cut(p, "=")
			name, _, _ = strings.Cut(name, ":")
			if !h.Packages[name] {
				h.Packages[name] = true
				if f := h.OnInstall[name]; f != nil {
					f(h)
				}
			}
		}
		return ok("")
	case "remove", "purge":
		for _, p := range words[1:] {
			delete(h.Packages, p)
		}
		return ok("")
	}
	return fail(100, "E: Invalid operation "+words[0])
}

// systemctl VERB [--now|--quiet] [-p PROP]... UNIT
func (h *Host) systemctl(a []string) host.Output {
	var verb, name string
	now := false
	for i := 0; i < len(a); i++ {
		switch {
		case a[i] == "-p":
			i++
		case a[i] == "--now":
			now = true
		case strings.HasPrefix(a[i], "-"):
		case verb == "":
			verb = a[i]
		default:
			name = a[i]
		}
	}
	if verb == "daemon-reload" {
		h.DaemonReloads++
		return ok("")
	}
	if name == "" {
		return fail(1, "Too few arguments.")
	}
	u := h.Unit(name)
	switch verb {
	case "show":
		return ok(fmt.Sprintf("UnitFileState=%s\nActiveState=%s\n", map[bool]string{true: "enabled", false: "disabled"}[u.Enabled],
			map[bool]string{true: "active", false: "inactive"}[u.Active]))
	case "enable":
		u.Enabled, u.Active = true, u.Active || now
	case "disable":
		u.Enabled, u.Active = false, u.Active && !now
	case "start":
		u.Active = true
	case "stop":
		u.Active = false
	case "restart":
		u.Active = true
		u.Restarts++
	case "reload":
		if !u.Active {
			return fail(1, "Unit "+name+" is not active.")
		}
		u.Reloads++
	case "is-active":
		if u.Active {
			return ok("active\n")
		}
		return fail(3, "inactive\n")
	case "is-enabled":
		if u.Enabled {
			return ok("enabled\n")
		}
		return fail(1, "disabled\n")
	default:
		return fail(1, "Unknown command verb "+verb)
	}
	return ok("")
}

// lookupUser is the user named name or with that uid.
func (h *Host) lookupUser(name string) (string, *User) {
	if u := h.Users[name]; u != nil {
		return name, u
	}
	if id, err := strconv.Atoi(name); err == nil {
		for _, n := range slices.Sorted(maps.Keys(h.Users)) {
			if h.Users[n].UID == id {
				return n, h.Users[n]
			}
		}
	}
	return "", nil
}

// lookupGroup is the group named name or with that gid.
func (h *Host) lookupGroup(name string) (string, *Group) {
	if g := h.Groups[name]; g != nil {
		return name, g
	}
	if id, err := strconv.Atoi(name); err == nil {
		for _, n := range slices.Sorted(maps.Keys(h.Groups)) {
			if h.Groups[n].GID == id {
				return n, h.Groups[n]
			}
		}
	}
	return "", nil
}

func (h *Host) getent(a []string) host.Output {
	switch arg(a, 0) {
	case "passwd":
		if n, u := h.lookupUser(arg(a, 1)); u != nil {
			return ok(fmt.Sprintf("%s:x:%d:%d:%s:%s:%s\n", n, u.UID, u.GID, u.Comment, u.Home, u.Shell))
		}
	case "shadow":
		if u := h.Users[arg(a, 1)]; u != nil {
			return ok(fmt.Sprintf("%s:%s:20000:0:99999:7:::\n", arg(a, 1), u.Password))
		}
	case "group":
		if n, g := h.lookupGroup(arg(a, 1)); g != nil {
			return ok(fmt.Sprintf("%s:x:%d:%s\n", n, g.GID, strings.Join(g.Members, ",")))
		}
	}
	return fail(2, "")
}

// id NAME, id -nG NAME
func (h *Host) id(a []string) host.Output {
	name := a[len(a)-1]
	n, u := h.lookupUser(name)
	if u == nil {
		return fail(1, "id: '"+name+"': no such user")
	}
	if arg(a, 0) != "-nG" {
		return ok(fmt.Sprintf("uid=%d(%s) gid=%d\n", u.UID, n, u.GID))
	}
	var groups []string
	if gn, _ := h.lookupGroup(strconv.Itoa(u.GID)); gn != "" && h.Groups[n] == nil {
		groups = append(groups, gn)
	}
	if h.Groups[n] != nil {
		groups = append(groups, n)
	}
	for _, gn := range slices.Sorted(maps.Keys(h.Groups)) {
		if slices.Contains(h.Groups[gn].Members, n) && !slices.Contains(groups, gn) {
			groups = append(groups, gn)
		}
	}
	return ok(strings.Join(groups, " ") + "\n")
}

// account runs useradd, usermod, userdel, groupadd, groupmod or groupdel.
func (h *Host) account(cmd string, a []string) host.Output {
	if len(a) == 0 {
		return fail(2, "Usage: "+cmd)
	}
	name := a[len(a)-1]
	opts := map[string]string{}
	for i := 0; i < len(a)-1; i++ {
		switch o := a[i]; o {
		case "--system", "-r", "--create-home", "-m", "--no-create-home", "-M", "--append", "-a":
			opts[strings.TrimLeft(o, "-")] = "true"
		default:
			i++
			opts[strings.TrimLeft(o, "-")] = arg(a, i)
		}
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, found := opts[k]; found {
				return v
			}
		}
		return ""
	}
	groupsOf := func(list string) ([]string, host.Output, bool) {
		if list == "" {
			return nil, host.Output{}, true
		}
		var out []string
		for _, g := range strings.Split(list, ",") {
			gn, grp := h.lookupGroup(g)
			if grp == nil {
				return nil, fail(6, cmd+": group '"+g+"' does not exist"), false
			}
			out = append(out, gn)
		}
		return out, host.Output{}, true
	}
	switch cmd {
	case "useradd":
		if h.Users[name] != nil {
			return fail(9, "useradd: user '"+name+"' already exists")
		}
		u := &User{UID: os.Getuid(), GID: os.Getgid(), Home: cmp.Or(get("home-dir", "d"), "/home/"+name),
			Shell: cmp.Or(get("shell", "s"), "/bin/sh"), Comment: get("comment", "c"), Password: "!"}
		if v, found := opts["password"]; found {
			u.Password = v
		}
		if v := get("uid", "u"); v != "" {
			u.UID, _ = strconv.Atoi(v)
		}
		if g := get("gid", "g"); g != "" {
			_, grp := h.lookupGroup(g)
			if grp == nil {
				return fail(6, "useradd: group '"+g+"' does not exist")
			}
			u.GID = grp.GID
		} else if h.Groups[name] == nil {
			h.Groups[name] = &Group{GID: os.Getgid()}
		}
		extra, o, good := groupsOf(get("groups", "G"))
		if !good {
			return o
		}
		h.Users[name] = u
		for _, g := range extra {
			h.Groups[g].Members = append(h.Groups[g].Members, name)
		}
		if get("create-home", "m") == "true" {
			if err := os.MkdirAll(h.Path(u.Home), 0o755); err != nil {
				return fail(12, err.Error())
			}
		}
	case "usermod":
		u := h.Users[name]
		if u == nil {
			return fail(6, "usermod: user '"+name+"' does not exist")
		}
		if v := get("uid", "u"); v != "" {
			u.UID, _ = strconv.Atoi(v)
		}
		if g := get("gid", "g"); g != "" {
			_, grp := h.lookupGroup(g)
			if grp == nil {
				return fail(6, "usermod: group '"+g+"' does not exist")
			}
			u.GID = grp.GID
		}
		u.Home = cmp.Or(get("home", "d"), u.Home)
		u.Shell = cmp.Or(get("shell", "s"), u.Shell)
		if v, found := opts["comment"]; found {
			u.Comment = v
		}
		if v, found := opts["password"]; found {
			u.Password = v
		}
		extra, o, good := groupsOf(get("groups", "G"))
		if !good {
			return o
		}
		if get("append", "a") != "true" {
			for _, g := range h.Groups {
				g.Members = slices.DeleteFunc(g.Members, func(m string) bool { return m == name })
			}
		}
		for _, g := range extra {
			if !slices.Contains(h.Groups[g].Members, name) {
				h.Groups[g].Members = append(h.Groups[g].Members, name)
			}
		}
	case "userdel":
		if h.Users[name] == nil {
			return fail(6, "userdel: user '"+name+"' does not exist")
		}
		delete(h.Users, name)
		for _, g := range h.Groups {
			g.Members = slices.DeleteFunc(g.Members, func(m string) bool { return m == name })
		}
	case "groupadd":
		if h.Groups[name] != nil {
			return fail(9, "groupadd: group '"+name+"' already exists")
		}
		g := &Group{GID: os.Getgid()}
		if v := get("gid", "g"); v != "" {
			g.GID, _ = strconv.Atoi(v)
		}
		h.Groups[name] = g
	case "groupmod":
		g := h.Groups[name]
		if g == nil {
			return fail(6, "groupmod: group '"+name+"' does not exist")
		}
		if v := get("gid", "g"); v != "" {
			g.GID, _ = strconv.Atoi(v)
		}
	case "groupdel":
		if h.Groups[name] == nil {
			return fail(6, "groupdel: group '"+name+"' does not exist")
		}
		delete(h.Groups, name)
	}
	return ok("")
}

// sysctl -n KEY..., sysctl -p FILE, sysctl -w KEY=VALUE...
func (h *Host) sysctl(a []string) host.Output {
	switch arg(a, 0) {
	case "-n":
		var out []string
		for _, k := range a[1:] {
			out = append(out, cmp.Or(h.Sysctl[k], "0"))
		}
		return ok(strings.Join(out, "\n") + "\n")
	case "-p":
		p := cmp.Or(arg(a, 1), "/etc/sysctl.conf")
		b, err := os.ReadFile(h.Path(p))
		if err != nil {
			return fail(255, "sysctl: cannot open \""+p+"\": No such file or directory")
		}
		var out strings.Builder
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || l[0] == '#' || l[0] == ';' {
				continue
			}
			if k, v, found := strings.Cut(l, "="); found {
				k, v = strings.TrimPrefix(strings.TrimSpace(k), "-"), strings.Join(strings.Fields(v), " ")
				h.Sysctl[k] = v
				fmt.Fprintf(&out, "%s = %s\n", k, v)
			}
		}
		return ok(out.String())
	case "-w":
		for _, kv := range a[1:] {
			if k, v, found := strings.Cut(kv, "="); found {
				h.Sysctl[k] = v
			}
		}
		return ok("")
	}
	return fail(255, "sysctl: unsupported in the simulation: "+strings.Join(a, " "))
}

// installUFW writes what the ufw package installs: its files, IPv6 on,
// incoming denied, disabled, no rules.
func installUFW(h *Host) {
	h.Write("/etc/default/ufw", "IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\nDEFAULT_FORWARD_POLICY=\"DROP\"\n")
	h.Write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	h.Write("/etc/ufw/user.rules", "*filter\n:ufw-user-input - [0:0]\n### RULES ###\n\n### END RULES ###\nCOMMIT\n")
	h.Write("/etc/ufw/user6.rules", "*filter\n:ufw6-user-input - [0:0]\n### RULES ###\n\n### END RULES ###\nCOMMIT\n")
}

// installNginx writes what the nginx package installs: its directories,
// the default site, the binary; the unit enabled and running.
func installNginx(h *Host) {
	for _, d := range []string{"/etc/nginx/conf.d", "/etc/nginx/sites-available", "/etc/nginx/sites-enabled", "/var/log/nginx", "/var/www/html"} {
		if err := os.MkdirAll(h.Path(d), 0o755); err != nil {
			h.T.Fatal(err)
		}
	}
	h.Write("/etc/nginx/nginx.conf", "events {}\nhttp { include /etc/nginx/conf.d/*.conf; include /etc/nginx/sites-enabled/*; }\n")
	h.Write("/etc/nginx/sites-available/default", "server { listen 80 default_server; }\n")
	if err := os.Symlink("/etc/nginx/sites-available/default", h.Path("/etc/nginx/sites-enabled/default")); err != nil {
		h.T.Fatal(err)
	}
	h.Write("/usr/sbin/nginx", "")
	u := h.Unit("nginx")
	u.Enabled, u.Active = true, true
}

// ufwFiles are ufw's rules files, v4 then v6.
var ufwFiles = [2]string{"/etc/ufw/user.rules", "/etc/ufw/user6.rules"}

func (h *Host) ufw(a []string) host.Output {
	if !h.Exists("/etc/default/ufw") {
		return fail(127, "ufw: command not found")
	}
	switch {
	case arg(a, 0) == "default" && len(a) == 3:
		key := map[string]string{"incoming": "DEFAULT_INPUT_POLICY", "outgoing": "DEFAULT_OUTPUT_POLICY", "routed": "DEFAULT_FORWARD_POLICY"}[a[2]]
		val := map[string]string{"allow": "ACCEPT", "deny": "DROP", "reject": "REJECT"}[a[1]]
		if key == "" || val == "" {
			return fail(1, "ERROR: Unsupported default policy")
		}
		h.setKV("/etc/default/ufw", key, `"`+val+`"`)
		return ok("Default " + a[2] + " policy changed to '" + a[1] + "'\n")
	case strings.Join(a, " ") == "--force enable":
		h.setKV("/etc/ufw/ufw.conf", "ENABLED", "yes")
		return ok("Firewall is active and enabled on system startup\n")
	case strings.Join(a, " ") == "disable":
		h.setKV("/etc/ufw/ufw.conf", "ENABLED", "no")
		return ok("Firewall stopped and disabled on system startup\n")
	case arg(a, 0) == "status":
		if strings.Contains(h.Read("/etc/ufw/ufw.conf"), "ENABLED=yes") {
			return ok("Status: active\n")
		}
		return ok("Status: inactive\n")
	}
	del := arg(a, 0) == "delete"
	if del {
		a = a[1:]
	}
	r, err := parseUFWRule(a)
	if err != nil {
		return fail(1, "ERROR: "+err.Error())
	}
	v4, v6, err := r.Tuples(strings.Contains(h.Read("/etc/default/ufw"), "IPV6=yes"))
	if err != nil {
		return fail(1, "ERROR: "+err.Error())
	}
	var out []string
	for i, t := range []string{v4, v6} {
		if t == "" {
			continue
		}
		file := h.Read(ufwFiles[i])
		line := "### tuple ### " + t
		has := slices.Contains(strings.Split(file, "\n"), line)
		switch {
		case del && has:
			file = strings.Replace(file, line+"\n", "", 1)
			out = append(out, "Rule deleted")
		case del:
			out = append(out, "Could not delete non-existent rule")
		case has:
			out = append(out, "Skipping adding existing rule")
		default:
			file = strings.Replace(file, "### END RULES ###", line+"\n\n### END RULES ###", 1)
			out = append(out, "Rule added")
		}
		h.Write(ufwFiles[i], file)
	}
	return ok(strings.Join(out, "\n") + "\n")
}

// parseUFWRule reads ufw's rule syntax as host.UFW writes it: ACTION
// [in|out] [on IFACE] [proto P] [from X] [to Y] [port P] [comment C].
func parseUFWRule(a []string) (host.UFWRule, error) {
	if len(a) == 0 {
		return host.UFWRule{}, fmt.Errorf("no rule")
	}
	r := host.UFWRule{Action: a[0]}
	for i := 1; i < len(a); i++ {
		w := a[i]
		if w == "in" || w == "out" {
			r.Direction = w
			continue
		}
		if i+1 >= len(a) {
			return r, fmt.Errorf("%q: no value", w)
		}
		v := a[i+1]
		i++
		switch w {
		case "on":
			r.Interface = v
		case "proto":
			r.Proto = v
		case "from":
			r.From = strings.TrimPrefix(v, "any")
		case "to":
			r.To = strings.TrimPrefix(v, "any")
		case "port":
			r.Port = v
		case "comment":
			r.Comment = v
		default:
			return r, fmt.Errorf("unsupported in the simulation: %q", w)
		}
	}
	return r, nil
}

// setKV sets KEY=value in a shell-style file under the root.
func (h *Host) setKV(p, key, value string) {
	lines := strings.Split(strings.TrimRight(h.Read(p), "\n"), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			lines[i], found = key+"="+value, true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	h.Write(p, strings.Join(lines, "\n")+"\n")
}

func (h *Host) docker(a []string) (host.Output, bool) {
	switch {
	case arg(a, 0) == "info" && slices.Contains(a, "{{.Swarm.LocalNodeState}}"):
		return ok(h.Swarm + "\n"), true
	case arg(a, 0) == "swarm" && (arg(a, 1) == "init" || arg(a, 1) == "join"):
		if h.Swarm != "inactive" {
			return fail(1, "Error response from daemon: This node is already part of a swarm."), true
		}
		h.Swarm = "active"
		return ok("Swarm initialized\n"), true
	case arg(a, 0) == "network" && arg(a, 1) == "inspect":
		if h.Networks[arg(a, 2)] {
			return ok("[{}]\n"), true
		}
		return fail(1, "Error response from daemon: network "+arg(a, 2)+" not found"), true
	case arg(a, 0) == "network" && arg(a, 1) == "create":
		n := a[len(a)-1]
		if h.Networks[n] {
			return fail(1, "Error response from daemon: network with name "+n+" already exists"), true
		}
		h.Networks[n] = true
		return ok("id\n"), true
	}
	return host.Output{}, false
}
