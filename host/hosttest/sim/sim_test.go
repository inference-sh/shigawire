package sim_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
)

// Each resource converges on the simulation: changed, then ok, then no
// drift in Verify.
func converge(t *testing.T, r host.Resource) {
	t.Helper()
	hosttest.Converge(t, r, host.Changed)
	if got, err := hosttest.Run(hosttest.Verify, host.Play{Tasks: []host.Task{{Resource: r}}}); err != nil {
		t.Fatalf("%s: verify: %v %s", r.ID(), err, hosttest.Statuses(got))
	}
}

func TestResourcesConverge(t *testing.T) {
	h := sim.New(t)
	apt := &host.Apt{Exec: h.Exec, Root: h.Root}
	converge(t, apt.Install("ufw", "nginx"))
	if !h.Packages["ufw"] || !h.Exists("/etc/ufw/user.rules") || !h.Exists("/etc/nginx/sites-enabled/default") {
		t.Fatal("packages not installed")
	}
	converge(t, &host.Group{Exec: h.Exec, Name: "app"})
	converge(t, &host.User{Exec: h.Exec, Name: "deploy", Shell: "/bin/bash", Groups: []string{"sudo", "app"}, CreateHome: true})
	if u := h.Users["deploy"]; u == nil || u.Shell != "/bin/bash" || !h.Exists("/home/deploy") {
		t.Fatal(h.Users)
	}
	converge(t, &host.Service{Exec: h.Exec, Name: "app", Enabled: true, Running: true, Unit: []byte("[Service]\nExecStart=/bin/true\n"), Root: h.Root})
	if u := h.Units["app"]; !u.Enabled || !u.Active || h.DaemonReloads != 1 {
		t.Fatal(*u, h.DaemonReloads)
	}
	converge(t, &host.Sysctl{Exec: h.Exec, Path: "/etc/sysctl.d/99-test.conf", Key: "vm.swappiness", Value: "10", Root: h.Root})
	if h.Sysctl["vm.swappiness"] != "10" {
		t.Fatal(h.Sysctl)
	}
	converge(t, &host.UFW{Exec: h.Exec, Root: h.Root, Defaults: map[string]string{"incoming": "deny", "outgoing": "allow"}, Enabled: true,
		Rules: []host.UFWRule{
			{Action: "allow", Port: "22", Proto: "tcp"},
			{Action: "allow", From: "172.16.0.0/12", Comment: "Docker internal"},
			{Action: "allow", From: "100.64.0.0/10", Port: "5432"},
			{Action: "deny", Port: "5432", Proto: "tcp"},
		}})
	if !strings.Contains(h.Read("/etc/ufw/user.rules"), "### tuple ### allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in") {
		t.Fatal(h.Read("/etc/ufw/user.rules"))
	}
	converge(t, &host.Swarm{Exec: h.Exec, AdvertiseAddr: "100.64.0.1"})
	if h.Swarm != "active" {
		t.Fatal(h.Swarm)
	}
}

func TestAnswers(t *testing.T) {
	h := sim.New(t)
	run := func(name string, args ...string) host.Output { return h.Run(host.Cmd{Name: name, Args: args}) }
	if o := run("usermod", "--append", "--groups", "docker", "root"); o.Code != 6 {
		t.Fatalf("a missing group: %+v", o)
	}
	if o := run("systemctl", "reload", "nginx"); o.Code == 0 {
		t.Fatal("reload of an inactive unit")
	}
	if o := run("dpkg-query", "-W", "-f=${Status}", "nginx"); o.Code != 1 {
		t.Fatal(o)
	}
	run("apt-get", "-y", "install", "nginx=1.26")
	if o := run("dpkg-query", "-W", "-f=${Status}", "nginx"); o.Out != "install ok installed" {
		t.Fatal(o)
	}
	if o := run("docker", "network", "inspect", "net"); o.Code != 1 {
		t.Fatal(o)
	}
	run("docker", "network", "create", "--driver", "overlay", "net")
	if o := run("docker", "network", "inspect", "net"); o.Code != 0 {
		t.Fatal(o)
	}
	// The On rules come first; unknown commands go to Fallback.
	h.Exec.On("systemctl is-active nginx", hosttest.Exit(3))
	if o := h.Exec.Run(t.Context(), host.Cmd{Name: "systemctl", Args: []string{"is-active", "nginx"}}); o.Code != 3 {
		t.Fatal(o)
	}
	h.Fallback = func(c host.Cmd) host.Output { return hosttest.Out("fallback " + c.Name) }
	if o := run("tailscale", "ip"); o.Out != "fallback tailscale" {
		t.Fatal(o)
	}
}

// ufw keeps one rule for "limit 22/tcp" and "allow 22/tcp" and updates
// it in place, so two plays that list the same port with other actions
// each see the other's rule as drift that names both actions. The rules
// files are those ufw 0.36.2 wrote on Ubuntu 24.04 for examples/web.
func TestUFWActionIsPartOfTheRule(t *testing.T) {
	h := sim.New(t)
	h.Run(host.Cmd{Name: "apt-get", Args: []string{"-y", "install", "ufw"}})
	for _, f := range []string{"user.rules", "user6.rules"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		h.Write("/etc/ufw/"+f, string(b))
	}
	ufw := func(r host.UFWRule) *host.UFW {
		return &host.UFW{Exec: h.Exec, Root: h.Root, Rules: []host.UFWRule{r}}
	}
	allow := ufw(host.UFWRule{Action: "allow", Proto: "tcp", Port: "22"})
	limit := ufw(host.UFWRule{Action: "limit", Proto: "tcp", Port: "22", Comment: "ssh"})
	tuples := func(f string) []string {
		var out []string
		for _, l := range strings.Split(h.Read(f), "\n") {
			if strings.HasPrefix(l, "### tuple ###") {
				out = append(out, l)
			}
		}
		return out
	}
	before := tuples("/etc/ufw/user.rules")

	// The web play's limit is in place; an allow of the same port is not.
	if st, err := limit.Check(t.Context()); err != nil || !st.InPlace {
		t.Fatalf("limit: %+v %v", st, err)
	}
	for _, m := range []hosttest.Mode{hosttest.Plan, hosttest.Verify} {
		got, _ := hosttest.Run(m, host.Play{Tasks: []host.Task{{Resource: allow}}})
		if want := `allow in proto tcp from any to any port 22 (limit -> allow, comment "ssh" -> none)`; got[0].Status != host.Changed || got[0].Msg != want {
			t.Fatalf("%s %q, want changed %q", got[0].Status, got[0].Msg, want)
		}
	}
	if r := hosttest.Converge(t, allow, host.Changed); !strings.Contains(r.Msg, "limit -> allow") {
		t.Fatal(r.Msg)
	}
	// Updated in place: the same rules, in the same order, 22 allowed, its
	// limit lines gone.
	after := tuples("/etc/ufw/user.rules")
	if len(after) != len(before) || after[1] != "### tuple ### allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in" || after[0] != before[0] || after[2] != before[2] {
		t.Fatalf("%q, was %q", after, before)
	}
	if strings.Contains(h.Read("/etc/ufw/user.rules"), "--dport 22 -m conntrack") || len(tuples("/etc/ufw/user6.rules")) != 3 {
		t.Fatal(h.Read("/etc/ufw/user.rules"), h.Read("/etc/ufw/user6.rules"))
	}
	// Deleting a limit of 22 leaves the allow: ufw delete matches the
	// action.
	if st, err := ufw(host.UFWRule{Action: "limit", Proto: "tcp", Port: "22", Absent: true}).Check(t.Context()); err != nil || !st.InPlace {
		t.Fatalf("absent limit: %+v %v", st, err)
	}
	// And back.
	if r := hosttest.Converge(t, limit, host.Changed); r.Msg != `limit in proto tcp from any to any port 22 comment ssh (allow -> limit, comment none -> "ssh")` {
		t.Fatal(r.Msg)
	}
	if got := tuples("/etc/ufw/user.rules"); !slices.Equal(got, before) {
		t.Fatalf("%q, want %q", got, before)
	}
	// Deleting it with no comment deletes it, as ufw delete does.
	hosttest.Converge(t, ufw(host.UFWRule{Action: "limit", Proto: "tcp", Port: "22", Absent: true}), host.Changed)
	if got := tuples("/etc/ufw/user6.rules"); len(got) != 2 || strings.Contains(h.Read("/etc/ufw/user6.rules"), "--dport 22") {
		t.Fatal(h.Read("/etc/ufw/user6.rules"))
	}
}

// An installer that runs apt-get itself changes dpkg's status file, so an
// Apt that read the packages before reads them again.
func TestDpkgStatus(t *testing.T) {
	h := sim.New(t)
	apt := &host.Apt{Exec: h.Exec, Root: h.Root}
	if ok, _ := apt.Installed(t.Context(), "jq"); ok {
		t.Fatal("jq installed")
	}
	hosttest.ApplyOne(t, &host.Command{Exec: h.Exec, Cmd: host.Cmd{Name: "apt-get", Args: []string{"install", "-y", "jq"}}})
	if ok, _ := apt.Installed(t.Context(), "jq"); !ok || !strings.Contains(h.Read("/var/lib/dpkg/status"), "Package: jq\n") {
		t.Fatal("the read of the packages is stale")
	}
}
