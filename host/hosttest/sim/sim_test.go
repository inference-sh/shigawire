package sim_test

import (
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
