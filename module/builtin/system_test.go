package builtin_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/module"
)

// unit is a fake systemd unit's state.
type unit struct{ enabled, active bool }

func fakeSystemd(h *hosttest.Host, units map[string]*unit) {
	h.Exec.Respond = func(line string) host.Output {
		f := strings.Fields(line)
		if f[0] != "systemctl" {
			return host.Output{}
		}
		name := f[len(f)-1]
		u := units[name]
		if u == nil {
			u = &unit{}
			units[name] = u
		}
		switch f[1] {
		case "show":
			return hosttest.Out(fmt.Sprintf("UnitFileState=%s\nActiveState=%s\n",
				map[bool]string{true: "enabled", false: "disabled"}[u.enabled], map[bool]string{true: "active", false: "inactive"}[u.active]))
		case "enable":
			u.enabled, u.active = true, u.active || f[2] == "--now"
		case "disable":
			u.enabled, u.active = false, u.active && f[2] != "--now"
		case "start":
			u.active = true
		case "stop":
			u.active = false
		}
		return host.Output{}
	}
}

func TestSystemd(t *testing.T) {
	h, env := newHost(t)
	units := map[string]*unit{}
	fakeSystemd(h, units)
	unitText := "[Service]\nExecStart=/usr/local/bin/app\n"
	r := build(t, env, "systemd", map[string]any{"name": "app", "enabled": true, "state": "started", "unit": unitText})
	if got := hosttest.Converge(t, r, host.Changed); got.Msg != "unit file: create; enable and start" {
		t.Fatal(got.Msg)
	}
	if h.Read("/etc/systemd/system/app.service") != unitText {
		t.Fatal("unit file")
	}
	if n := len(h.Exec.Ran("systemctl daemon-reload")); n != 1 {
		t.Fatalf("daemon-reload %d times", n)
	}
	hosttest.Converge(t, build(t, env, "service", map[string]any{"name": "app", "enabled": false, "state": "stopped"}), host.Changed)
	if *units["app"] != (unit{}) {
		t.Fatal(*units["app"])
	}
	refuse(t, env, "systemd", map[string]any{"name": "app"}, "enabled, state or unit")
	refuse(t, env, "systemd", map[string]any{"name": "app", "state": "restarted"}, "restart and reload modules")
	refuse(t, env, "systemd", map[string]any{"name": "a b", "state": "started"}, "not a unit name")
	refuse(t, env, "systemd", map[string]any{"name": "app", "enabled": "yes"}, "want true or false")
}

// fakeAccounts answers getent, id and the account commands from a user
// and a group table.
func fakeAccounts(h *hosttest.Host) (users map[string]string, groups map[string]int, member map[string][]string) {
	users, groups, member = map[string]string{}, map[string]int{"adm": 4, "docker": 999}, map[string][]string{}
	h.Exec.Respond = func(line string) host.Output {
		f := strings.Fields(line)
		name := f[len(f)-1]
		switch f[0] {
		case "getent":
			if f[1] == "passwd" {
				if u, ok := users[name]; ok {
					return hosttest.Out(u + "\n")
				}
			} else if gid, ok := groups[name]; ok {
				return hosttest.Out(fmt.Sprintf("%s:x:%d:\n", name, gid))
			}
			return hosttest.Exit(2)
		case "id":
			return hosttest.Out(name + " " + strings.Join(member[name], " ") + "\n")
		case "useradd":
			users[name] = fmt.Sprintf("%s:x:1001:1001::/home/%s:/bin/sh", name, name)
			if i := slices.Index(f, "--shell"); i > 0 {
				users[name] = fmt.Sprintf("%s:x:1001:1001::/home/%s:%s", name, name, f[i+1])
			}
			if i := slices.Index(f, "--groups"); i > 0 {
				member[name] = strings.Split(f[i+1], ",")
			}
		case "usermod":
			if i := slices.Index(f, "--groups"); i > 0 {
				member[name] = append(member[name], strings.Split(f[i+1], ",")...)
			}
		case "userdel":
			delete(users, name)
		case "groupadd":
			groups[name] = 1500
			if i := slices.Index(f, "--gid"); i > 0 {
				var g int
				fmt.Sscan(f[i+1], &g)
				groups[name] = g
			}
		case "groupdel":
			delete(groups, name)
		}
		return host.Output{}
	}
	return users, groups, member
}

func TestUser(t *testing.T) {
	h, env := newHost(t)
	users, _, member := fakeAccounts(h)
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "shell": "/bin/bash", "groups": []any{"docker"}}), host.Changed)
	if !strings.HasSuffix(users["deploy"], ":/bin/bash") || !slices.Contains(member["deploy"], "docker") {
		t.Fatal(users, member)
	}
	if got := h.Exec.Ran("useradd"); len(got) != 1 || !strings.Contains(got[0], "--create-home") {
		t.Fatal(got)
	}
	// Another group is added; docker is kept.
	h.Exec.Reset()
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "groups": []any{"docker", "adm"}}), host.Changed)
	if got := h.Exec.Ran("usermod"); len(got) != 1 || got[0] != "usermod --append --groups adm deploy" {
		t.Fatal(got)
	}
	hosttest.Converge(t, build(t, env, "user", map[string]any{"name": "deploy", "state": "absent"}), host.Changed)
	if _, ok := users["deploy"]; ok {
		t.Fatal("not removed")
	}
	refuse(t, env, "user", map[string]any{"name": "Bad Name"}, "not a user name")
	refuse(t, env, "user", map[string]any{"name": "u", "home": "home/u"}, "absolute")
	refuse(t, env, "user", map[string]any{"name": "u", "comment": "a:b"}, "no colon")
	refuse(t, env, "user", map[string]any{"name": "u", "uid": 1.5}, "whole number")
}

func TestGroup(t *testing.T) {
	h, env := newHost(t)
	_, groups, _ := fakeAccounts(h)
	hosttest.Converge(t, build(t, env, "group", map[string]any{"name": "app", "gid": 2000, "system": true}), host.Changed)
	if groups["app"] != 2000 {
		t.Fatal(groups)
	}
	if got := h.Exec.Ran("groupadd"); len(got) != 1 || got[0] != "groupadd --system --gid 2000 app" {
		t.Fatal(got)
	}
	hosttest.Converge(t, build(t, env, "group", map[string]any{"name": "app", "state": "absent"}), host.Changed)
	refuse(t, env, "group", map[string]any{}, "name is required")
	refuse(t, env, "group", map[string]any{"name": "app", "gid": -1}, "positive")
}

func TestSysctl(t *testing.T) {
	h, env := newHost(t)
	kernel := map[string]string{"net.ipv4.ip_forward": "0", "net.ipv4.ip_local_port_range": "32768\t60999"}
	h.Exec.Respond = func(line string) host.Output {
		f := strings.Fields(line)
		switch {
		case len(f) == 3 && f[1] == "-n":
			return hosttest.Out(kernel[f[2]] + "\n")
		case len(f) == 3 && f[1] == "-p":
			for _, l := range strings.Split(h.Read(f[2]), "\n") {
				if k, v, ok := strings.Cut(l, "="); ok {
					kernel[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		}
		return host.Output{}
	}
	h.Write("/etc/sysctl.d/99-shigawire.conf", "net.ipv4.ip_forward=0\n")
	hosttest.Converge(t, build(t, env, "sysctl", map[string]any{"name": "net.ipv4.ip_forward", "value": "1"}), host.Changed)
	if got := h.Read("/etc/sysctl.d/99-shigawire.conf"); got != "net.ipv4.ip_forward = 1\n" || kernel["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("%q %v", got, kernel)
	}
	// The kernel's tab-separated value is the same value.
	h.Write("/etc/sysctl.d/90-ports.conf", "net.ipv4.ip_local_port_range = 32768 60999\n")
	if r := hosttest.ApplyOne(t, build(t, env, "sysctl", map[string]any{"name": "net.ipv4.ip_local_port_range", "value": "32768 60999", "file": "/etc/sysctl.d/90-ports.conf"})); r.Status != host.OK {
		t.Fatalf("%s %s", r.Status, r.Msg)
	}
	// A kernel that does not take the value: an error, not a change each run.
	h.Exec.On("sysctl -p", host.Output{})
	kernel["vm.swappiness"] = "60"
	got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "sysctl", map[string]any{"name": "vm.swappiness", "value": "10"})})
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
	h := sim.New(t)
	env := module.NewEnv(h.Exec, h.Root)
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
