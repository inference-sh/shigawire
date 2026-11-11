package simrun_test

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/internal/simrun"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
	"github.com/inference-sh/shigawire/playbook"
)

func TestRunConverges(t *testing.T) {
	p := hosttest.Playbook(t, `
steps:
  - name: base
    tasks:
      - apt: {name: nginx}
      - file: {path: /etc/motd, content: "${WHO}\n"}
        notify: restart nginx
      - command: {cmd: "echo hi"}
    handlers:
      - name: restart nginx
        restart: {name: nginx}
`)
	o, err := simrun.Run(context.Background(), p, playbook.Options{Set: map[string]string{"WHO": "me"}})
	if err != nil {
		t.Fatal(err)
	}
	if !o.OK() || o.Failure() != "" {
		t.Fatal(o.Failure())
	}
	if len(o.Apply) == 0 || len(o.Again) == 0 || len(o.Verify) == 0 {
		t.Fatalf("runs %d %d %d", len(o.Apply), len(o.Again), len(o.Verify))
	}
}

func TestRunDoesNotConverge(t *testing.T) {
	// The guard's file never appears: the command runs every time.
	p := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - command: {cmd: make, creates: /opt/built}\n")
	o, err := simrun.Run(context.Background(), p, playbook.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Failed != simrun.StageAgain || len(o.Changed) != 1 || !strings.HasPrefix(o.Failure(), "second apply changed s: command /bin/sh -c make") {
		t.Fatalf("%s %v\n%s", o.Failed, o.Changed, o.Failure())
	}
}

func TestRunApplyFails(t *testing.T) {
	p := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - fail: {msg: no}\n")
	o, err := simrun.Run(context.Background(), p, playbook.Options{})
	if err != nil || o.Failed != simrun.StageApply || !strings.HasPrefix(o.Failure(), "apply: ") {
		t.Fatalf("%v %s\n%s", err, o.Failed, o.Failure())
	}
}

// lateDrift is in place for its first two checks and drifts after.
type lateDrift struct{ n *atomic.Int32 }

func (d lateDrift) ID() string { return "late" }
func (d lateDrift) Check(context.Context) (host.State, error) {
	if d.n.Add(1) > 2 {
		return host.State{Drift: "moved"}, nil
	}
	return host.State{InPlace: true}, nil
}
func (d lateDrift) Apply(context.Context, host.State) (host.Result, error) {
	return host.Result{Status: host.Changed}, nil
}

func TestRunVerifyDrifts(t *testing.T) {
	reg := builtin.Registry()
	var n atomic.Int32
	reg.Register("late", func(module.Env, map[string]any) (host.Resource, error) { return lateDrift{&n}, nil })
	p := hosttest.Playbook(t, "steps:\n  - name: s\n    tasks:\n      - late:\n")
	o, err := simrun.Run(context.Background(), p, playbook.Options{Registry: reg})
	if err != nil || o.Failed != simrun.StageVerify || !strings.Contains(o.Failure(), "changed  s: late: moved") {
		t.Fatalf("%v %s\n%s", err, o.Failed, o.Failure())
	}
	// The default registry has no such module.
	if _, err := simrun.Run(context.Background(), p, playbook.Options{}); err == nil || !strings.Contains(err.Error(), `unknown module "late"`) {
		t.Fatalf("err %v", err)
	}
}

func TestNewHostRemovesItsFiles(t *testing.T) {
	h, done, err := simrun.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Read("/etc/os-release"), "trixie") {
		t.Fatal("no os-release")
	}
	root := h.Root
	done()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root left behind: %v", err)
	}
}
