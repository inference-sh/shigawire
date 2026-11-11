// Package shigatest tests a playbook on a fake host: a simulated Debian
// host (package sim) on hosttest's temporary root and fake Exec. Run
// applies the playbook, applies it again and wants nothing changed the
// second time, then verifies it and wants no drift.
//
//	func TestSite(t *testing.T) {
//		h := shigatest.Run(t, "site.yml", map[string]string{"DOMAIN": "example.com"})
//		if !strings.Contains(h.Read("/etc/nginx/sites-enabled/site"), "example.com") {
//			t.Fatal("no server name")
//		}
//	}
//
// The playbook may name the standard modules (builtin.Registry()). A
// test of a playbook that names other modules passes a registry that has
// them in playbook.Options.Registry (RunOptions, Load):
//
//	reg := builtin.Registry()
//	internal.Register(reg)
//	shigatest.RunOptions(t, sim.New(t).Host, "site.yml", playbook.Options{Registry: reg})
//
// Commands get the simulation's answers: packages, units, accounts,
// sysctl, ufw and docker swarm change as on a host; any other command
// succeeds with no output unless the test says otherwise (RunOn, with a
// host set up beforehand). The runs are package simrun's, which shiga
// test uses too.
package shigatest

import (
	"context"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/internal/simrun"
	"github.com/inference-sh/shigawire/playbook"
)

// Run runs the playbook at path, with the variables vars, on a new
// simulated host (sim.New, then RunOn), and returns the host.
func Run(t testing.TB, path string, vars map[string]string) *sim.Host {
	t.Helper()
	h := sim.New(t)
	RunOn(t, h.Host, path, vars)
	return h
}

// RunOn runs the playbook at path on h (a sim.Host's Host, or a bare
// hosttest.Host whose Exec the test set up), as simrun.Converge does:
//
//   - apply: every task ok, changed, skipped or warn;
//   - apply again: nothing changed but actions (State.Action: a command
//     with no guard, which runs every time) and no handler run;
//   - verify: no drift.
//
// The host's environment sets no variable: vars are all of them beside the
// playbook's own. Any other outcome fails t, with every task's line.
func RunOn(t testing.TB, h *hosttest.Host, path string, vars map[string]string) {
	t.Helper()
	RunOptions(t, h, path, playbook.Options{Set: vars})
}

// RunOptions is RunOn with the playbook's options: variables from files,
// the steps --only selects, the module registry (nil:
// builtin.Registry()). opt.Env is h's (any Env given is replaced) and
// opt.Environ is ignored: the host's environment sets no variable.
func RunOptions(t testing.TB, h *hosttest.Host, path string, opt playbook.Options) {
	t.Helper()
	pb, err := Load(h, path, opt)
	if err != nil {
		t.Fatal(err)
	}
	if o := simrun.Converge(context.Background(), pb); !o.OK() {
		t.Fatal(o.Failure())
	}
}

// Load loads the playbook at path to run on h, for a test that runs it
// itself (one that wants a run to fail): opt.Env is h's, with a fresh
// host.Apt, opt.Environ is ignored, and a nil opt.Registry is
// builtin.Registry() (simrun.Load).
func Load(h *hosttest.Host, path string, opt playbook.Options) (*playbook.Playbook, error) {
	opt.Environ = nil
	return simrun.Load(h, path, opt)
}

// Results runs f (a Playbook's Apply, Plan or Verify) and returns every
// result and the run's error.
func Results(ctx context.Context, f func(context.Context, *host.Runner) error) ([]host.Result, error) {
	return hosttest.Results(ctx, f)
}
