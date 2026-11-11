// Command shiga puts a host in the state a playbook describes.
//
//	shiga plan PLAYBOOK [flags]     what apply would change; changes nothing
//	shiga apply PLAYBOOK [flags]    put the host in the playbook's state
//	shiga verify PLAYBOOK [flags]   report drift; changes nothing
//	shiga check PLAYBOOK [flags]    validate the playbook; touches no host
//	shiga test PLAYBOOK [flags]     run it on a simulated host: converges?
//	shiga modules                   the modules and their arguments
//	shiga version
//
// docs/playbooks.md is the playbook format's reference.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/internal/simrun"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
	"github.com/inference-sh/shigawire/playbook"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3"); ""
// reads the module version Go recorded.
var version string

func main() {
	exec := host.OSExec{Path: host.RootPath}
	c := &cli{
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		environ: os.Environ(),
		env:     module.NewEnv(exec, ""),
		reg:     builtin.Registry(),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := c.main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// Exit codes.
const (
	exitOK      = 0
	exitFailed  = 1
	exitChanged = 2 // drift in verify; changes with --detailed-exitcode
)

const usage = `usage:
  shiga plan PLAYBOOK [flags]     what apply would change; changes nothing
  shiga apply PLAYBOOK [flags]    put the host in the playbook's state
  shiga verify PLAYBOOK [flags]   report drift; changes nothing
  shiga check PLAYBOOK [flags]    validate the playbook; runs nothing, reads nothing of the host
  shiga test PLAYBOOK [flags]     apply, apply again, verify on a simulated host
  shiga modules                   the modules and their arguments
  shiga version

flags:
  --var NAME=VALUE      set a variable (repeatable; over every other source)
  --vars FILE           read variables from a YAML file, or NAME=VALUE lines from a *.env file (repeatable; a later file wins)
  --only STEP[,STEP]    run only these steps, by name or tag (repeatable; not check)
  --quiet               print only what changed, failed, warned or is unknown (plan, apply, verify)
  --detailed-exitcode   exit 2 when plan or apply changed something
  --allow-undefined     check: an undefined variable is not a problem (set at apply time)

check reports every problem, a line each: YAML errors, unknown modules, module
argument errors, undefined variables, bad when conditions, notify or needs
naming no handler or registered task, a missing files/ src or key_src. It
checks every step, manual ones too.

test is a simulation: packages, units, accounts, sysctl, ufw and docker swarm
are modelled, and any other command succeeds with no output. It fails when a
task fails, when the second apply changes anything but an action, or when
verify finds drift.

plan reports unknown for a check that failed. One after a change planned
before it may depend on that change (a file owned by a user apply creates):
plan exits 0 and says how many there are. One apply would fail at whatever
runs before it makes plan exit 1: a refusal (a fail task whose when holds,
a docker swarm that is locked), or a check that failed with no change
planned before it.

Variables also come from the environment: SHIGA_VAR_NAME sets NAME.
Exit codes: 0 ok, 1 failure (check: a problem; test: no convergence; plan:
an unknown no earlier change can explain), 2 drift (verify) or changes
(--detailed-exitcode).
`

// cli is the command, with what it acts through.
type cli struct {
	stdout, stderr io.Writer
	environ        []string
	env            module.Env
	// reg is the modules a playbook may name.
	reg *module.Registry
}

func (c *cli) main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, usage)
		return exitFailed
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "plan", "apply", "verify":
		return c.run(ctx, cmd, rest)
	case "test":
		return c.test(ctx, rest)
	case "check":
		return c.check(rest)
	case "modules":
		c.modules()
		return exitOK
	case "version", "--version":
		fmt.Fprintln(c.stdout, "shiga", versionString())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(c.stderr, "shiga: unknown command %q\n\n%s", cmd, usage)
		return exitFailed
	}
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

// list is a repeatable flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

// flags parses args: the playbook, and the flags --var, --vars and
// --only (when withOnly) into opt, with whatever more fs defines. ok is
// false when they do not parse; the error is printed.
func (c *cli) flags(fs *flag.FlagSet, args []string, withOnly bool) (path string, opt playbook.Options, ok bool) {
	cmd := fs.Name()
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, usage) }
	var sets, varFiles, only list
	fs.Var(&sets, "var", "")
	fs.Var(&varFiles, "vars", "")
	if withOnly {
		fs.Var(&only, "only", "")
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return "", opt, false
	}
	if len(pos) != 1 {
		fmt.Fprintf(c.stderr, "%s: want one playbook, got %d\n\n%s", cmd, len(pos), usage)
		return "", opt, false
	}
	opt = playbook.Options{Registry: c.reg, Env: c.env, Environ: c.environ, VarFiles: varFiles, Set: map[string]string{}}
	for _, kv := range sets {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			fmt.Fprintf(c.stderr, "shiga: --var %q: want NAME=VALUE\n", kv)
			return "", opt, false
		}
		opt.Set[k] = v
	}
	for _, o := range only {
		for _, s := range strings.Split(o, ",") {
			if s = strings.TrimSpace(s); s != "" {
				opt.Only = append(opt.Only, s)
			}
		}
	}
	return pos[0], opt, true
}

func (c *cli) run(ctx context.Context, cmd string, args []string) int {
	fs := flag.NewFlagSet("shiga "+cmd, flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "")
	detailed := fs.Bool("detailed-exitcode", false, "")
	path, opt, ok := c.flags(fs, args, true)
	if !ok {
		return exitFailed
	}
	pb, err := playbook.Load(path, opt)
	if err != nil {
		fmt.Fprintf(c.stderr, "shiga: %v\n", err)
		return exitFailed
	}
	counts := map[host.Status]int{}
	// A plan's unknowns: stuck are those apply fails at whatever runs
	// before them (Result.Blocked); waiting may depend on a change before
	// them, which the plan cannot tell.
	var waiting, stuck int
	r := &host.Runner{OnResult: func(rr host.Result) {
		counts[rr.Status]++
		switch {
		case rr.Blocked:
			stuck++
		case rr.Status == host.Unknown:
			waiting++
		}
		c.line(rr, *quiet)
	}}
	var runErr error
	switch cmd {
	case "plan":
		runErr = pb.Plan(ctx, r)
	case "apply":
		runErr = pb.Apply(ctx, r)
	case "verify":
		runErr = pb.Verify(ctx, r)
	}
	c.summary(cmd, counts)
	if cmd == "plan" {
		if waiting > 0 {
			fmt.Fprintf(c.stdout, "%d unknown: checks that may depend on changes planned before them, which apply makes first\n", waiting)
		}
		if stuck > 0 {
			fmt.Fprintf(c.stdout, "%d unknown that no earlier change can explain: apply stops at the first\n", stuck)
		}
	}
	switch {
	case cmd == "verify" && (counts[host.Unknown] > 0 || counts[host.Failed] > 0):
		return exitFailed
	case cmd == "plan" && stuck > 0:
		return exitFailed
	case cmd == "verify" && counts[host.Changed] > 0:
		return exitChanged
	case runErr != nil:
		// A failed task's error is on its line already.
		if counts[host.Failed] == 0 {
			fmt.Fprintf(c.stderr, "shiga: %v\n", runErr)
		}
		return exitFailed
	case *detailed && counts[host.Changed] > 0:
		return exitChanged
	}
	return exitOK
}

// parseInterspersed parses fs's flags wherever they are among args, and
// returns the other arguments. Everything after -- is an argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// Parse stops at the first argument, or after --.
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// line prints one task's or handler's result.
func (c *cli) line(r host.Result, quiet bool) {
	if quiet && (r.Status == host.OK || r.Status == host.Skipped) {
		return
	}
	name := r.Name
	if r.Handler {
		name = "handler " + name
	}
	msg := r.Msg
	if msg == "" && r.Err != nil {
		msg = r.Err.Error()
	}
	if msg != "" {
		name += ": " + oneLine(msg)
	}
	fmt.Fprintf(c.stdout, "%-8s %s\n", r.Status, name)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// summaryOrder is the order of the counts in the summary.
var summaryOrder = []host.Status{host.OK, host.Changed, host.Failed, host.Ignored, host.Warn, host.Unknown, host.Skipped}

func (c *cli) summary(cmd string, counts map[host.Status]int) {
	var parts []string
	for _, s := range summaryOrder {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no tasks"}
	}
	fmt.Fprintf(c.stdout, "%s: %s\n", cmd, strings.Join(parts, ", "))
}

// modules lists the registered modules and their arguments.
func (c *cli) modules() {
	names := c.reg.Names()
	if len(names) == 0 {
		fmt.Fprintln(c.stdout, "no modules registered")
		return
	}
	tw := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	for i, n := range names {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintln(tw, n)
		args, ok := c.reg.Args(n)
		switch {
		case !ok:
			fmt.Fprintln(tw, "  (arguments not described)")
		case len(args) == 0:
			fmt.Fprintln(tw, "  (no arguments)")
		}
		for _, a := range args {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Key, a.Type, a.Doc)
		}
	}
	tw.Flush()
}

// simNote is what shiga test prints first, so no one takes its result
// for a run on a host.
const simNote = "simulated host: packages, units, accounts, sysctl, ufw and docker swarm are modelled; any other command succeeds with no output"

// test runs the playbook on a simulated host (package simrun): apply,
// apply again wanting nothing changed, verify wanting no drift.
func (c *cli) test(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("shiga test", flag.ContinueOnError)
	path, opt, ok := c.flags(fs, args, true)
	if !ok {
		return exitFailed
	}
	fmt.Fprintln(c.stdout, simNote)
	o, err := simrun.Run(ctx, path, opt)
	if err != nil {
		fmt.Fprintf(c.stderr, "shiga: %v\n", err)
		return exitFailed
	}
	for _, r := range []struct {
		name  string
		stage simrun.Stage
		rs    []host.Result
	}{
		{"apply", simrun.StageApply, o.Apply},
		{"apply again", simrun.StageAgain, o.Again},
		{"verify", simrun.StageVerify, o.Verify},
	} {
		if o.Ran(r.stage) {
			c.summary(r.name, countOf(r.rs))
		}
	}
	switch o.Failed {
	case "":
		fmt.Fprintln(c.stdout, "ok: the playbook converges on the simulated host")
		return exitOK
	case simrun.StageApply:
		c.lines(o.Apply, func(r host.Result) bool { return r.Status == host.Failed || r.Status == host.Unknown })
		fmt.Fprintf(c.stdout, "FAIL: apply: %s\n", oneLine(o.Err.Error()))
	case simrun.StageAgain:
		if o.Err != nil {
			c.lines(o.Again, func(r host.Result) bool { return r.Status == host.Failed || r.Status == host.Unknown })
			fmt.Fprintf(c.stdout, "FAIL: apply again: %s\n", oneLine(o.Err.Error()))
			break
		}
		for _, r := range o.Changed {
			c.line(r, false)
		}
		fmt.Fprintf(c.stdout, "FAIL: apply again changed %d: the playbook does not converge\n", len(o.Changed))
	case simrun.StageVerify:
		c.lines(o.Verify, func(r host.Result) bool { return r.Status != host.OK && r.Status != host.Skipped })
		fmt.Fprintf(c.stdout, "FAIL: verify after apply: %s\n", oneLine(o.Err.Error()))
	}
	return exitFailed
}

// lines prints the results keep keeps.
func (c *cli) lines(rs []host.Result, keep func(host.Result) bool) {
	for _, r := range rs {
		if keep(r) {
			c.line(r, false)
		}
	}
}

func countOf(rs []host.Result) map[host.Status]int {
	counts := map[host.Status]int{}
	for _, r := range rs {
		counts[r.Status]++
	}
	return counts
}

// check loads and validates the playbook, touching nothing
// (playbook.Check): a line per problem, or ok and the counts.
func (c *cli) check(args []string) int {
	fs := flag.NewFlagSet("shiga check", flag.ContinueOnError)
	allowUndefined := fs.Bool("allow-undefined", false, "")
	path, opt, ok := c.flags(fs, args, false)
	if !ok {
		return exitFailed
	}
	opt.AllowUndefined = *allowUndefined
	rep := playbook.Check(path, opt)
	for _, p := range rep.Problems {
		fmt.Fprintln(c.stdout, p)
	}
	if n := len(rep.Problems); n > 0 {
		fmt.Fprintf(c.stdout, "check: %d %s\n", n, plural(n, "problem", "problems"))
		return exitFailed
	}
	fmt.Fprintf(c.stdout, "ok: %d %s, %d %s\n", rep.Steps, plural(rep.Steps, "step", "steps"), rep.Tasks, plural(rep.Tasks, "task", "tasks"))
	return exitOK
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
