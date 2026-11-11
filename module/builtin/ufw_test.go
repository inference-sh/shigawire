package builtin_test

import (
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/module"
)

const ufwDefaults = `# /etc/default/ufw
IPV6=yes
DEFAULT_INPUT_POLICY="ACCEPT"
DEFAULT_OUTPUT_POLICY="ACCEPT"
DEFAULT_FORWARD_POLICY="DROP"
`

// ufwHost is a simulated host with ufw installed, its default incoming
// policy ACCEPT.
func ufwHost(t *testing.T) (*sim.Host, module.Env) {
	h, env := simHost(t)
	hosttest.ApplyOne(t, env.Apt.Install("ufw"))
	h.Write("/etc/default/ufw", ufwDefaults)
	h.Exec.Reset()
	return h, env
}

func TestUFW(t *testing.T) {
	h, env := ufwHost(t)
	args := map[string]any{
		"default": map[string]any{"incoming": "deny", "outgoing": "allow"},
		"enabled": true,
		"rules": []any{
			map[string]any{"rule": "allow", "port": "22", "proto": "tcp"},
			map[string]any{"rule": "allow", "from": "172.16.5.0/12", "comment": "Docker internal"},
			map[string]any{"rule": "allow", "from": "100.64.0.0/10", "port": "5432", "interface": "tailscale0"},
			map[string]any{"rule": "deny", "port": "5432", "proto": "tcp"},
		},
	}
	r := build(t, env, "ufw", args)
	hosttest.Converge(t, r, host.Changed)
	want := []string{
		"ufw default deny incoming",
		"ufw allow in proto tcp from any to any port 22",
		"ufw allow in from 172.16.0.0/12 to any comment Docker internal",
		"ufw allow in on tailscale0 from 100.64.0.0/10 to any port 5432",
		"ufw deny in proto tcp from any to any port 5432",
		"ufw --force enable",
	}
	if got := h.Exec.Ran("ufw"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ran:\n%s", strings.Join(got, "\n"))
	}
	// A rule to delete, which ufw has.
	h.Exec.Reset()
	del := build(t, env, "ufw", map[string]any{"rules": []any{map[string]any{"rule": "deny", "port": "5432", "proto": "tcp", "state": "absent"}}})
	hosttest.Converge(t, del, host.Changed)
	if strings.Contains(h.Read("/etc/ufw/user6.rules"), "deny tcp 5432") {
		t.Fatal("v6 rule left")
	}
	// Plan reads the files and runs nothing.
	h.Exec.Reset()
	if got, _ := play(hosttest.Plan, host.Task{Resource: build(t, env, "ufw", map[string]any{"rules": []any{map[string]any{"rule": "allow", "port": "443", "proto": "tcp"}}})}); got[0].Status != host.Changed {
		t.Fatal(hosttest.Statuses(got))
	}
	if len(h.Exec.Calls()) != 0 {
		t.Fatal(h.Exec.Calls())
	}
}

func TestUFWStoredOtherwise(t *testing.T) {
	h, env := ufwHost(t)
	// ufw takes the rule and stores it in a form the module does not
	// expect (here: not at all): an error, not a change every run.
	h.Exec.On("ufw allow", host.Output{})
	got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "ufw", map[string]any{"rules": []any{map[string]any{"rule": "allow", "port": "80", "proto": "tcp"}}})})
	if err == nil || !strings.Contains(got[0].Msg, "still differ") {
		t.Fatal(hosttest.Statuses(got), err)
	}
}

func TestUFWNotInstalled(t *testing.T) {
	_, env := simHost(t)
	got, err := play(hosttest.Plan, host.Task{Resource: build(t, env, "ufw", map[string]any{"enabled": true})})
	if err != nil || got[0].Status != host.Unknown || !strings.Contains(got[0].Msg, "not installed") {
		t.Fatal(hosttest.Statuses(got), err)
	}
}

func TestUFWRefused(t *testing.T) {
	_, env := newHost(t)
	rule := func(kv ...string) map[string]any {
		r := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			r[kv[i]] = kv[i+1]
		}
		return map[string]any{"rules": []any{r}}
	}
	refuse(t, env, "ufw", map[string]any{}, "default, enabled or rules")
	refuse(t, env, "ufw", map[string]any{"default": map[string]any{"incoming": "drop"}}, "want allow, deny or reject")
	refuse(t, env, "ufw", map[string]any{"default": map[string]any{"inbound": "deny"}}, "want incoming, outgoing or routed")
	refuse(t, env, "ufw", rule("rule", "allow", "port", "ssh"), "want a number")
	refuse(t, env, "ufw", rule("rule", "allow", "port", "6000:6007"), "needs proto")
	refuse(t, env, "ufw", rule("rule", "permit", "port", "22"), "want allow, deny, reject or limit")
	refuse(t, env, "ufw", rule("rule", "allow", "from", "10.0.0.300"), "not an address")
	refuse(t, env, "ufw", rule("rule", "allow", "from", "10.0.0.1", "to", "fd00::1"), "different IP versions")
	refuse(t, env, "ufw", rule("rule", "allow", "port", "22", "comment", "it's"), "quote")
	refuse(t, env, "ufw", rule("rule", "allow", "port", "22", "state", "gone"), "want present, absent")
}
