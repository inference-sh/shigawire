package shigatest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
	pb "github.com/inference-sh/shigawire/playbook"
	"github.com/inference-sh/shigawire/shigatest"
)

// reg is the standard modules and one that never converges.
var reg = testRegistry()

func testRegistry() *module.Registry {
	r := builtin.Registry()
	r.Register("test.drifting", func(module.Env, map[string]any) (host.Resource, error) { return drifting{}, nil })
	return r
}

// drifting is never in place, and not an action: it never converges.
type drifting struct{}

func (drifting) ID() string { return "drifting" }
func (drifting) Check(context.Context) (host.State, error) {
	return host.State{Drift: "always"}, nil
}
func (drifting) Apply(context.Context, host.State) (host.Result, error) {
	return host.Result{Status: host.Changed}, nil
}

func TestRun(t *testing.T) {
	p := hosttest.Playbook(t, `
steps:
  - name: s
    tasks:
      - file: {path: /etc/motd, content: "${WHO}", parents: true}
        notify: reload
      - command: {cmd: [apt-get, update]}
    handlers:
      - name: reload
        command: {cmd: [systemctl, restart, x]}
`)
	h := sim.New(t)
	shigatest.RunOptions(t, h.Host, p, pb.Options{Registry: reg, Set: map[string]string{"WHO": "me"}})
	if h.Read("/etc/motd") != "me" {
		t.Fatal(h.Read("/etc/motd"))
	}
	if n := len(h.Exec.Ran("systemctl restart")); n != 1 || h.Units["x"].Restarts != 1 {
		t.Fatalf("restarted %d times", n)
	}
}

// fakeT records a failure and stops the test function, as testing.T does.
type fakeT struct {
	testing.TB
	msg string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(format string, a ...any) {
	f.msg = strings.TrimSpace(strings.SplitN(sprintf(format, a...), "\n", 2)[0])
	panic(f)
}
func (f *fakeT) Fatal(a ...any) { f.Fatalf("%v", a...) }

func TestRunFailsWhenNotConverging(t *testing.T) {
	p := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - test.drifting:\n")
	ft := &fakeT{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil && r != ft {
				panic(r)
			}
		}()
		shigatest.RunOptions(ft, sim.New(t).Host, p, pb.Options{Registry: reg})
	}()
	if !strings.Contains(ft.msg, "second apply changed s: drifting") {
		t.Fatalf("msg %q", ft.msg)
	}
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func TestRunOptionsAndLoad(t *testing.T) {
	p := hosttest.Playbook(t, `
steps:
  - {name: a, tasks: [{file: {path: /a, content: "${A}"}}]}
  - {name: b, tasks: [{file: {path: /b, content: x}}]}
`)
	h := sim.New(t)
	shigatest.RunOptions(t, h.Host, p, pb.Options{Registry: reg, Only: []string{"a"}, Set: map[string]string{"A": "1"}})
	if h.Read("/a") != "1" || h.Exists("/b") {
		t.Fatal("only a")
	}
	loaded, err := shigatest.Load(h.Host, p, pb.Options{Registry: reg, Set: map[string]string{"A": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := shigatest.Results(context.Background(), loaded.Plan)
	if err != nil || len(got) != 2 || got[0].Status != host.Changed || got[1].Status != host.Changed {
		t.Fatal(got, err)
	}
}

func TestDefaultRegistryIsBuiltins(t *testing.T) {
	p := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - file: {path: /etc/motd, content: hi}\n")
	if h := shigatest.Run(t, p, nil); h.Read("/etc/motd") != "hi" {
		t.Fatal(h.Read("/etc/motd"))
	}
	other := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - test.drifting: {}\n")
	if _, err := shigatest.Load(sim.New(t).Host, other, pb.Options{}); err == nil || !strings.Contains(err.Error(), `unknown module "test.drifting"`) {
		t.Fatalf("err %v", err)
	}
}
