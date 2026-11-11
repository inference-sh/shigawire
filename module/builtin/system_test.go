package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/module"
)

// simHost is a simulated host (package sim) and the Env its modules act
// through.
func simHost(t *testing.T) (*sim.Host, module.Env) {
	h := sim.New(t)
	return h, module.NewEnv(h.Exec, h.Root)
}

func TestSystemd(t *testing.T) {
	h, env := simHost(t)
	unitText := "[Service]\nExecStart=/usr/local/bin/app\n"
	r := build(t, env, "systemd", map[string]any{"name": "app", "enabled": true, "state": "started", "unit": unitText})
	if got := hosttest.Converge(t, r, host.Changed); got.Msg != "unit file: create; enable and start" {
		t.Fatal(got.Msg)
	}
	if h.Read("/etc/systemd/system/app.service") != unitText {
		t.Fatal("unit file")
	}
	if u := h.Units["app"]; !u.Enabled || !u.Active || h.DaemonReloads != 1 {
		t.Fatalf("%+v, daemon-reload %d times", *u, h.DaemonReloads)
	}
	hosttest.Converge(t, build(t, env, "service", map[string]any{"name": "app", "enabled": false, "state": "stopped"}), host.Changed)
	if u := h.Units["app"]; u.Enabled || u.Active {
		t.Fatalf("%+v", *u)
	}
	refuse(t, env, "systemd", map[string]any{"name": "app"}, "enabled, state or unit")
	refuse(t, env, "systemd", map[string]any{"name": "app", "state": "restarted"}, "restart and reload modules")
	refuse(t, env, "systemd", map[string]any{"name": "a b", "state": "started"}, "not a unit name")
	refuse(t, env, "systemd", map[string]any{"name": "app", "enabled": "yes"}, "want true or false")
}

func TestUser(t *testing.T) {
	h, env := simHost(t)
	h.Groups["docker"] = &sim.Group{GID: 999}
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "shell": "/bin/bash", "groups": []any{"docker"}}), host.Changed)
	if u := h.Users["deploy"]; u == nil || u.Shell != "/bin/bash" || !slices.Contains(h.Groups["docker"].Members, "deploy") || !h.Exists("/home/deploy") {
		t.Fatal(h.Users, h.Groups["docker"])
	}
	// Another group is added; docker is kept.
	h.Exec.Reset()
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "groups": []any{"docker", "adm"}}), host.Changed)
	if got := h.Exec.Ran("usermod"); len(got) != 1 || got[0] != "usermod --append --groups adm deploy" {
		t.Fatal(got)
	}
	if !slices.Contains(h.Groups["docker"].Members, "deploy") || !slices.Contains(h.Groups["adm"].Members, "deploy") {
		t.Fatal(h.Groups["docker"], h.Groups["adm"])
	}
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "state": "absent"}), host.Changed)
	if h.Users["deploy"] != nil {
		t.Fatal("not removed")
	}
	refuse(t, env, "user", map[string]any{"name": "Bad Name"}, "not a user name")
	refuse(t, env, "user", map[string]any{"name": "u", "home": "home/u"}, "absolute")
	refuse(t, env, "user", map[string]any{"name": "u", "comment": "a:b"}, "no colon")
	refuse(t, env, "user", map[string]any{"name": "u", "uid": 1.5}, "whole number")
}

func TestGroup(t *testing.T) {
	h, env := simHost(t)
	hosttest.Converge(t, build(t, env, "group", map[string]any{"name": "app", "gid": 2000, "system": true}), host.Changed)
	if g := h.Groups["app"]; g == nil || g.GID != 2000 {
		t.Fatal(h.Groups)
	}
	if got := h.Exec.Ran("groupadd"); len(got) != 1 || got[0] != "groupadd --system --gid 2000 app" {
		t.Fatal(got)
	}
	hosttest.Converge(t, build(t, env, "group", map[string]any{"name": "app", "state": "absent"}), host.Changed)
	if h.Groups["app"] != nil {
		t.Fatal("not removed")
	}
	refuse(t, env, "group", map[string]any{}, "name is required")
	refuse(t, env, "group", map[string]any{"name": "app", "gid": -1}, "positive")
}

func TestSysctl(t *testing.T) {
	h, env := simHost(t)
	h.Sysctl["net.ipv4.ip_local_port_range"] = "32768\t60999"
	h.Write("/etc/sysctl.d/99-shigawire.conf", "net.ipv4.ip_forward=0\nvm.overcommit_memory = 1\n")
	hosttest.Converge(t, build(t, env, "sysctl", map[string]any{"name": "net.ipv4.ip_forward", "value": "1"}), host.Changed)
	if got := h.Read("/etc/sysctl.d/99-shigawire.conf"); got != "net.ipv4.ip_forward = 1\nvm.overcommit_memory = 1\n" || h.Sysctl["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("%q %v", got, h.Sysctl)
	}
	// The key alone is set: the drop-in's other keys are their own tasks'.
	if h.Sysctl["vm.overcommit_memory"] != "" || len(h.Exec.Ran("sysctl -p")) != 0 {
		t.Fatalf("loaded the whole drop-in: %v %v", h.Sysctl, h.Exec.Ran("sysctl"))
	}
	// The kernel's tab-separated value is the same value.
	h.Write("/etc/sysctl.d/90-ports.conf", "net.ipv4.ip_local_port_range = 32768 60999\n")
	if r := hosttest.ApplyOne(t, build(t, env, "sysctl", map[string]any{"name": "net.ipv4.ip_local_port_range", "value": "32768 60999", "file": "/etc/sysctl.d/90-ports.conf"})); r.Status != host.OK {
		t.Fatalf("%s %s", r.Status, r.Msg)
	}
	// A kernel that does not take the value: an error, not a change each run.
	h.Exec.On("sysctl -w", host.Output{})
	h.Sysctl["vm.swappiness"] = "60"
	got, err := hosttest.Run(hosttest.Apply, host.Play{Tasks: []host.Task{{Resource: build(t, env, "sysctl", map[string]any{"name": "vm.swappiness", "value": "10"})}}})
	if err == nil || !strings.Contains(got[0].Msg, `the kernel has vm.swappiness = "60"`) {
		t.Fatal(hosttest.Statuses(got), err)
	}
	refuse(t, env, "sysctl", map[string]any{"name": "net.ipv4.ip_forward"}, "value is required")
	refuse(t, env, "sysctl", map[string]any{"name": "rm -rf", "value": "1"}, "not a sysctl key")
	refuse(t, env, "sysctl", map[string]any{"name": "vm.swappiness", "value": 0.5}, "want a string")
}

func TestHandlers(t *testing.T) {
	h, env := newHost(t)
	h.Write("/etc/nginx/nginx.conf", "good\n")
	valid := true
	h.Exec.Respond = func(line string) host.Output {
		if line == "/bin/sh -c nginx -t -q" && !valid {
			return hosttest.Exit(1)
		}
		return host.Output{}
	}
	conf := func(content string) host.Task {
		return host.Task{Resource: build(t, env, "file", map[string]any{"path": "/etc/nginx/nginx.conf", "content": content}),
			Notify: []*host.Handler{{Resource: build(t, env, "validate_reload", map[string]any{
				"validate": "nginx -t -q", "service": "nginx", "restore": []any{"/etc/nginx/nginx.conf"}})}}}
	}
	got, err := play(hosttest.Apply, conf("better\n"))
	if err != nil || len(got) != 2 || got[0].Status != host.Changed || got[1].Status != host.Changed || got[1].Name != "validate and reload nginx" {
		t.Fatal(hosttest.Statuses(got), err)
	}
	if r := h.Exec.Ran("systemctl reload nginx"); len(r) != 1 {
		t.Fatal(h.Exec.Calls())
	}
	// Again: nothing changed, no handler.
	if got, _ := play(hosttest.Apply, conf("better\n")); len(got) != 1 || got[0].Status != host.OK {
		t.Fatal(hosttest.Statuses(got))
	}
	// A config that does not validate is put back, and nginx not reloaded.
	valid = false
	h.Exec.Reset()
	if _, err := play(hosttest.Apply, conf("broken\n")); err == nil {
		t.Fatal("no error")
	}
	if h.Read("/etc/nginx/nginx.conf") != "better\n" || len(h.Exec.Ran("systemctl reload")) != 0 {
		t.Fatal(h.Read("/etc/nginx/nginx.conf"), h.Exec.Calls())
	}

	for name, want := range map[string]string{"restart": "systemctl restart app", "reload": "systemctl reload app", "daemon_reload": "systemctl daemon-reload"} {
		args := map[string]any{"name": "app"}
		if name == "daemon_reload" {
			args = nil
		}
		h.Exec.Reset()
		if r := hosttest.ApplyOne(t, build(t, env, name, args)); r.Status != host.Changed || !r.Action {
			t.Fatalf("%s: %s", name, r.Status)
		}
		if got := h.Exec.Calls(); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s ran %v", name, got)
		}
	}
	refuse(t, env, "restart", map[string]any{}, "name is required")
	refuse(t, env, "validate_reload", map[string]any{"service": "nginx"}, "validate: an empty command")
	// validate alone only validates.
	h.Exec.Reset()
	if r := hosttest.ApplyOne(t, build(t, env, "validate_reload", map[string]any{"validate": []any{"visudo", "-c"}})); r.Status != host.Changed || !r.Action {
		t.Fatal(r.Status)
	}
	if got := h.Exec.Calls(); !slices.Equal(got, []string{"visudo -c"}) {
		t.Fatal(got)
	}
	// The reload is bounded as every systemctl call is: a hung reload
	// does not hang the play.
	if v := build(t, env, "validate_reload", map[string]any{"validate": "nginx -t", "service": "nginx"}).(*host.Validated); v.Then[0].Timeout == 0 {
		t.Fatalf("reload with no timeout: %+v", v.Then[0])
	}
	refuse(t, env, "validate_reload", map[string]any{"validate": "nginx -t", "service": "nginx", "then": []any{"x"}}, "give one")
	refuse(t, env, "validate_reload", map[string]any{"validate": "", "service": "nginx"}, "empty command")
}

// no_password: a new user gets "*", not useradd's locked "!"; an existing
// locked one is unlocked; one locked with a hash is an error.
func TestUserNoPassword(t *testing.T) {
	h, env := simHost(t)
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "ok", "no_password": true}), host.Changed)
	if pw := h.Users["ok"].Password; pw != "*" {
		t.Fatalf("new user: %q", pw)
	}
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "plain"}), host.Changed)
	if pw := h.Users["plain"].Password; pw != "!" {
		t.Fatalf("without no_password: %q", pw)
	}
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "plain", "no_password": true}), host.Changed)
	if pw := h.Users["plain"].Password; pw != "*" {
		t.Fatalf("unlocked: %q", pw)
	}
	h.Users["plain"].Password = "!$6$salt$hash"
	if _, err := build(t, env, "user", map[string]any{"name": "plain", "no_password": true}).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "locked with a hash") {
		t.Fatal(err)
	}
}
