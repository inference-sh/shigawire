package shigatest_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	pb "github.com/inference-sh/shigawire/playbook"
	"github.com/inference-sh/shigawire/shigatest"
)

// A validate_reload handler with a when still puts back what the run
// wrote when the validation fails.
func TestValidateReloadWithWhenPutsBack(t *testing.T) {
	h := sim.New(t)
	h.Write("/etc/ssh/sshd_config", "good\n")
	h.Fallback = func(c host.Cmd) host.Output {
		if c.Name == "sshd" {
			return host.Output{Code: 1, Out: "bad"}
		}
		return host.Output{}
	}
	p := hosttest.Playbook(t, `
name: t
steps:
  - name: ssh
    tasks:
      - file: {path: /etc/ssh/sshd_config, content: "bad\n"}
        notify: v
    handlers:
      - name: v
        when: os.id == debian
        validate_reload: {validate: [sshd, -t], service: ssh, restore: [/etc/ssh/sshd_config]}
`)
	loaded, err := shigatest.Load(h.Host, p, pb.Options{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	_, err = shigatest.Results(context.Background(), loaded.Apply)
	if err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("apply: %v", err)
	}
	if got := h.Read("/etc/ssh/sshd_config"); got != "good\n" {
		t.Fatalf("sshd_config %q", got)
	}
}

// A mode is read from what was written: 0644 and 0o644 as octal, a
// decimal 644 or a variable that holds 640 refused rather than made 0204.
func TestModeAsWritten(t *testing.T) {
	for _, c := range []struct{ mode, vars, want string }{
		{"0644", "", "-rw-r--r--"},
		{"0o640", "", "-rw-r-----"},
		{`"0600"`, "", "-rw-------"},
		{"${MODE}", `{MODE: "0640"}`, "-rw-r-----"},
		{"644", "", "is a number"},
		{"${MODE}", `{MODE: "640"}`, "is a number"},
	} {
		h := sim.New(t)
		vars := ""
		if c.vars != "" {
			vars = "vars: " + c.vars
		}
		p := hosttest.Playbook(t, `
name: t
`+vars+`
steps:
  - name: s
    tasks:
      - file:
          path: /etc/m
          content: "x\n"
          mode: `+c.mode+`
`)
		loaded, err := shigatest.Load(h.Host, p, pb.Options{Registry: reg})
		if strings.HasPrefix(c.want, "-") {
			if err != nil {
				t.Fatalf("%s: %v", c.mode, err)
			}
			if _, err := shigatest.Results(context.Background(), loaded.Apply); err != nil {
				t.Fatalf("%s: %v", c.mode, err)
			}
			fi, err := os.Stat(h.Path("/etc/m"))
			if err != nil || fi.Mode().String() != c.want {
				t.Errorf("%s %s: %v %v", c.mode, c.vars, fi.Mode(), err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: %v", c.mode, c.vars, err)
		}
	}
}

// register keeps the task's own command: a guard that found it in place
// and an owner lookup leave nothing to read.
func TestRegisterLeavesOutGuards(t *testing.T) {
	h := sim.New(t)
	h.Write("/opt/.done", "")
	h.Users["app"] = &sim.User{UID: os.Getuid(), GID: os.Getgid(), Home: "/home/app", Shell: "/bin/sh"}
	p := hosttest.Playbook(t, `
name: t
steps:
  - name: s
    tasks:
      - command: {cmd: [/opt/setup.sh], unless: [test, -f, /opt/.done]}
        register: setup
      - file: {path: /etc/flag, content: "x\n"}
        when: setup.rc == 0
        ignore_errors: true
      - file: {path: /etc/owned, content: "x\n", owner: app}
        register: owned
      - file: {path: /etc/flag2, content: "x\n"}
        when: owned.rc == 0
        ignore_errors: true
`)
	loaded, err := shigatest.Load(h.Host, p, pb.Options{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := shigatest.Results(context.Background(), loaded.Apply)
	if len(got) != 4 || got[1].Status != host.Ignored || !strings.Contains(got[1].Msg, "setup ran no command") ||
		got[3].Status != host.Ignored || !strings.Contains(got[3].Msg, "owned ran no command") {
		t.Fatalf("%+v", got)
	}
	if h.Exists("/etc/flag") || h.Exists("/etc/flag2") {
		t.Fatal("a task ran on a guard's or a lookup's rc")
	}
}

// A task that needs a task that failed is not checked: on a locked swarm
// the plan does not report the network as one to create.
func TestNeedsAFailedTask(t *testing.T) {
	p := hosttest.Playbook(t, `
name: t
steps:
  - name: swarm
    tasks:
      - name: swarm
        docker_swarm: {}
        register: swarm
      - name: network
        probe:
          test: [docker, network, inspect, net]
          drift: create
          fix: [[docker, network, create, --driver, overlay, net]]
        needs: swarm
`)
	run := func(state string, mode func(*pb.Playbook) func(context.Context, *host.Runner) error) []host.Result {
		h := sim.New(t)
		h.Swarm = state
		loaded, err := shigatest.Load(h.Host, p, pb.Options{Registry: reg})
		if err != nil {
			t.Fatal(err)
		}
		rs, _ := shigatest.Results(context.Background(), mode(loaded))
		return rs
	}
	plan := func(p *pb.Playbook) func(context.Context, *host.Runner) error { return p.Plan }
	verify := func(p *pb.Playbook) func(context.Context, *host.Runner) error { return p.Verify }
	apply := func(p *pb.Playbook) func(context.Context, *host.Runner) error { return p.Apply }
	for _, mode := range []func(*pb.Playbook) func(context.Context, *host.Runner) error{plan, verify} {
		rs := run("locked", mode)
		if len(rs) != 2 || rs[0].Status != host.Unknown || rs[1].Status != host.Unknown || rs[1].Msg != "not checked: it needs swarm, which failed" {
			t.Fatalf("locked: %s %q", hosttest.Statuses(rs), rs[len(rs)-1].Msg)
		}
	}
	// Inactive: the plan inits the swarm and creates the network.
	rs := run("inactive", plan)
	if rs[0].Msg != "swarm inactive: init" || rs[1].Status != host.Changed || rs[1].Msg != "create" {
		t.Fatalf("inactive: %s %q %q", hosttest.Statuses(rs), rs[0].Msg, rs[1].Msg)
	}
	if rs := run("inactive", apply); hosttest.Statuses(rs) != "swarm: swarm=changed swarm: network=changed" {
		t.Fatal(hosttest.Statuses(rs))
	}
}
