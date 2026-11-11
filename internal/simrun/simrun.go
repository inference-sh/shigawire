// Package simrun runs a playbook on a simulated Debian host (package sim)
// and checks that it converges: apply, apply again with nothing changed,
// then verify with no drift. It is the core of package shigatest and of
// shiga test; it imports no test framework of its own and fails nothing:
// it returns what happened.
//
// It is a simulation. The host is a temporary directory and sim's model
// of packages, units, accounts, sysctl, ufw and docker swarm; any other
// command succeeds with no output.
package simrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
	"github.com/inference-sh/shigawire/playbook"
)

// Load loads the playbook at path to run on h: opt.Env is h's (its Exec
// and Root, a fresh host.Apt; opt.Env.Files is kept), and a nil
// opt.Registry is builtin.Registry().
func Load(h *hosttest.Host, path string, opt playbook.Options) (*playbook.Playbook, error) {
	if opt.Registry == nil {
		opt.Registry = builtin.Registry()
	}
	files := opt.Env.Files
	opt.Env = module.NewEnv(h.Exec, h.Root)
	opt.Env.Files = files
	return playbook.Load(path, opt)
}

// Stage is a run of Converge.
type Stage string

const (
	StageApply  Stage = "apply"
	StageAgain  Stage = "second apply"
	StageVerify Stage = "verify"
)

// Outcome is what Converge saw.
type Outcome struct {
	// Apply, Again and Verify are each run's results, in order; a run
	// not reached has none.
	Apply, Again, Verify []host.Result
	// Failed is the run that failed; "" when the playbook converged.
	Failed Stage
	// Err is the failed run's error; nil when the second apply ran
	// cleanly but changed something.
	Err error
	// Changed are the results of the tasks and handlers the second apply
	// changed (actions aside): why it failed when Err is nil.
	Changed []host.Result
}

// OK reports whether the playbook converged.
func (o *Outcome) OK() bool { return o.Failed == "" }

// Ran reports whether Converge reached the run s.
func (o *Outcome) Ran(s Stage) bool {
	switch s {
	case StageAgain:
		return o.Failed != StageApply
	case StageVerify:
		return o.Failed == "" || o.Failed == StageVerify
	}
	return true
}

// Failure is the failure as one message, with the failed run's results
// one a line; "" when the playbook converged.
func (o *Outcome) Failure() string {
	switch o.Failed {
	case StageApply:
		return fmt.Sprintf("apply: %v\n%s", o.Err, Lines(o.Apply))
	case StageAgain:
		if o.Err != nil {
			return fmt.Sprintf("second apply: %v\n%s", o.Err, Lines(o.Again))
		}
		names := make([]string, len(o.Changed))
		for i, r := range o.Changed {
			names[i] = r.Name
		}
		return fmt.Sprintf("second apply changed %s: the playbook does not converge\n%s", strings.Join(names, ", "), Lines(o.Again))
	case StageVerify:
		return fmt.Sprintf("verify after apply: %v\n%s", o.Err, Lines(o.Verify))
	}
	return ""
}

// Converge runs pb:
//
//   - apply: every task ok, changed, skipped or warn;
//   - apply again: nothing changed but actions (State.Action: a command
//     with no guard, which runs every time) and no handler run;
//   - verify: no drift.
//
// It stops at the first run that fails.
func Converge(ctx context.Context, pb *playbook.Playbook) Outcome {
	var o Outcome
	var err error
	if o.Apply, err = hosttest.Results(ctx, pb.Apply); err != nil {
		o.Failed, o.Err = StageApply, err
		return o
	}
	if o.Again, err = hosttest.Results(ctx, pb.Apply); err != nil {
		o.Failed, o.Err = StageAgain, err
		return o
	}
	for _, r := range o.Again {
		if r.Status == host.Changed && (r.Handler || !r.Action) {
			o.Changed = append(o.Changed, r)
		}
	}
	if len(o.Changed) > 0 {
		o.Failed = StageAgain
		return o
	}
	if o.Verify, err = hosttest.Results(ctx, pb.Verify); err != nil {
		o.Failed, o.Err = StageVerify, err
	}
	return o
}

// Lines are the results, one a line, indented.
func Lines(rs []host.Result) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "  %-8s %s", r.Status, r.Name)
		if r.Msg != "" {
			fmt.Fprintf(&b, ": %s", r.Msg)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Run makes a simulated host (NewHost), loads the playbook at path on it
// (Load) and runs it (Converge), outside a test. The host is removed when
// Run returns. An error is a load error, or the simulation failing (a
// temporary file it could not write); a playbook that does not converge
// is a failed Outcome.
func Run(ctx context.Context, path string, opt playbook.Options) (o Outcome, err error) {
	h, done, err := NewHost()
	if err != nil {
		return o, err
	}
	defer done()
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(hostFailure)
			if !ok {
				panic(r)
			}
			err = f
		}
	}()
	pb, err := Load(h.Host, path, opt)
	if err != nil {
		return o, err
	}
	return Converge(ctx, pb), nil
}

// NewHost is a fresh simulated host (sim.New) outside a test, under a new
// temporary directory that done removes. The host stops a test with
// t.Fatal when it cannot write its files; here that is a panic Run
// recovers and returns as an error, so call the host's methods within
// Run, or recover it yourself.
func NewHost() (h *sim.Host, done func(), err error) {
	dir, err := os.MkdirTemp("", "shiga-sim-")
	if err != nil {
		return nil, nil, err
	}
	t := &hostT{dir: dir}
	done = func() {
		for i := len(t.cleanups) - 1; i >= 0; i-- {
			t.cleanups[i]()
		}
		os.RemoveAll(dir)
	}
	defer func() {
		if r := recover(); r != nil {
			done()
			f, ok := r.(hostFailure)
			if !ok {
				panic(r)
			}
			h, done, err = nil, nil, f
		}
	}()
	return sim.New(t), done, nil
}

// hostFailure is what hostT's Fatal panics with.
type hostFailure struct{ msg string }

func (f hostFailure) Error() string { return "simulated host: " + f.msg }

// hostT is the testing.TB sim and hosttest are given outside a test: a
// temporary directory, and Fatal as a panic. Only the methods they call
// are implemented; the embedded TB is nil.
type hostT struct {
	testing.TB
	dir      string
	cleanups []func()
}

func (t *hostT) Helper()      {}
func (t *hostT) Name() string { return "simrun" }
func (t *hostT) Cleanup(f func()) {
	t.cleanups = append(t.cleanups, f)
}
func (t *hostT) TempDir() string {
	d, err := os.MkdirTemp(t.dir, "root-")
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func (t *hostT) Fatal(a ...any)                 { panic(hostFailure{fmt.Sprint(a...)}) }
func (t *hostT) Fatalf(format string, a ...any) { panic(hostFailure{fmt.Sprintf(format, a...)}) }
func (t *hostT) Error(a ...any)                 { t.Fatal(a...) }
func (t *hostT) Errorf(format string, a ...any) { t.Fatalf(format, a...) }
func (t *hostT) FailNow()                       { t.Fatal("failed") }
func (t *hostT) Log(...any)                     {}
func (t *hostT) Logf(string, ...any)            {}
