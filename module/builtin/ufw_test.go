package builtin_test

import (
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

const ufwDefaults = `# /etc/default/ufw
IPV6=yes
DEFAULT_INPUT_POLICY="ACCEPT"
DEFAULT_OUTPUT_POLICY="ACCEPT"
DEFAULT_FORWARD_POLICY="DROP"
`

// fakeUFW is ufw writing its files: the rules it is given are added as
// the tuple lines in tuples (v4, then v6, "" for none).
func fakeUFW(h *hosttest.Host, tuples map[string][2]string) {
	h.Write("/etc/default/ufw", ufwDefaults)
	h.Write("/etc/ufw/ufw.conf", "ENABLED=no\nLOGLEVEL=low\n")
	h.Write("/etc/ufw/user.rules", "*filter\n:ufw-user-input - [0:0]\n### RULES ###\n\n### END RULES ###\nCOMMIT\n")
	h.Write("/etc/ufw/user6.rules", "*filter\n:ufw6-user-input - [0:0]\n### RULES ###\n\n### END RULES ###\nCOMMIT\n")
	add := func(file, tuple string) {
		if tuple != "" {
			c := h.Read(file)
			h.Write(file, strings.Replace(c, "### END RULES ###", "### tuple ### "+tuple+"\n-A ufw-user-input -j ACCEPT\n\n### END RULES ###", 1))
		}
	}
	h.Exec.Respond = func(line string) host.Output {
		switch {
		case line == "ufw default deny incoming":
			h.Write("/etc/default/ufw", strings.Replace(h.Read("/etc/default/ufw"), `INPUT_POLICY="ACCEPT"`, `INPUT_POLICY="DROP"`, 1))
		case line == "ufw --force enable":
			h.Write("/etc/ufw/ufw.conf", "ENABLED=yes\nLOGLEVEL=low\n")
		case strings.HasPrefix(line, "ufw delete "):
			for _, f := range []string{"/etc/ufw/user.rules", "/etc/ufw/user6.rules"} {
				var keep []string
				for _, l := range strings.Split(h.Read(f), "\n") {
					t := tuples[strings.TrimPrefix(line, "ufw delete ")]
					if l != "### tuple ### "+t[0] && l != "### tuple ### "+t[1] {
						keep = append(keep, l)
					}
				}
				h.Write(f, strings.Join(keep, "\n"))
			}
		case strings.HasPrefix(line, "ufw "):
			t, ok := tuples[strings.TrimPrefix(line, "ufw ")]
			if !ok {
				return host.Output{Code: 1, Out: "ERROR: test has no tuple for " + line}
			}
			add("/etc/ufw/user.rules", t[0])
			add("/etc/ufw/user6.rules", t[1])
		}
		return host.Output{}
	}
}

func TestUFW(t *testing.T) {
	h, env := newHost(t)
	// The tuples as ufw 0.36 writes them for these commands.
	fakeUFW(h, map[string][2]string{
		"allow in proto tcp from any to any port 22": {"allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in", "allow tcp 22 ::/0 any ::/0 in"},
		"allow in from 172.16.0.0/12 to any comment Docker internal": {
			"allow any any 0.0.0.0/0 any 172.16.0.0/12 in comment=446f636b657220696e7465726e616c", ""},
		"deny in proto tcp from any to any port 5432":                {"deny tcp 5432 0.0.0.0/0 any 0.0.0.0/0 in", "deny tcp 5432 ::/0 any ::/0 in"},
		"allow in on tailscale0 from 100.64.0.0/10 to any port 5432": {"allow any 5432 0.0.0.0/0 any 100.64.0.0/10 in_tailscale0", ""},
	})
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
	h, env := newHost(t)
	// ufw takes the rule and stores it in a form the module does not
	// expect: an error, not a change every run.
	fakeUFW(h, map[string][2]string{"allow in proto tcp from any to any port 80": {"allow tcp 80 0.0.0.0/0 any 0.0.0.0/0 in", ""}})
	got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "ufw", map[string]any{"rules": []any{map[string]any{"rule": "allow", "port": "80", "proto": "tcp"}}})})
	if err == nil || !strings.Contains(got[0].Msg, "still differ") {
		t.Fatal(hosttest.Statuses(got), err)
	}
}

func TestUFWNotInstalled(t *testing.T) {
	_, env := newHost(t)
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
