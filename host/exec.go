package host

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Cmd is one command to run.
type Cmd struct {
	Name string
	Args []string
	// Env is added to the inherited environment (a later value wins).
	Env []string
	// Timeout kills the command after this long; 0 leaves it to ctx.
	Timeout time.Duration
	// Dir is the directory it runs in, on the host; "" is /.
	Dir string
}

// String is the command line, for messages.
func (c Cmd) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Output is a command's combined stdout and stderr, and how it exited.
type Output struct {
	Out  string
	Code int // -1 when it did not run to an exit (missing binary, timeout)
	// Err is set whenever the command did not run and exit 0.
	Err error
}

// OK reports whether the command ran and exited 0.
func (o Output) OK() bool { return o.Err == nil }

// Exec runs commands. OSExec runs them on this host; tests use a fake.
type Exec interface {
	Run(ctx context.Context, c Cmd) Output
}

type asideKey struct{}

// Aside marks ctx for a command run beside the one a resource is about: a
// guard (Command.Unless) or a lookup (the owner a file gets). An Exec that
// keeps what a task ran (a playbook's register) leaves these out.
func Aside(ctx context.Context) context.Context { return context.WithValue(ctx, asideKey{}, true) }

// IsAside reports whether ctx is marked by Aside.
func IsAside(ctx context.Context) bool { v, _ := ctx.Value(asideKey{}).(bool); return v }

// RootPath is the PATH for commands run as root: a sudo or su that kept the
// caller's PATH does not decide which binaries root runs, and /usr/sbin is
// on it whatever the caller's lacked.
const RootPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// OSExec runs commands as processes, stdin closed, in Cmd.Dir, else in /:
// as root, the caller's working directory may be one another user can
// write, and some interpreters (Python) load code from it.
//
// A command runs in a process group of its own. A timeout, or ctx ending,
// sends the whole group SIGTERM and, killGrace later, SIGKILL; a command
// that exits while something it started still holds its output open
// returns killGrace after it exited. So an installer that leaves a daemon
// behind does not hold the run.
type OSExec struct {
	// Path, when set, is where a command named without a slash is found (a
	// PATH list), and the PATH it runs with: the caller's PATH does not
	// decide which binary runs. RootPath is the one for root.
	Path string
}

// killGrace is how long a command's process group has between SIGTERM and
// SIGKILL, and how long output left open is waited for (a test shortens
// it).
var killGrace = 10 * time.Second

// Run runs c. A nonzero exit is an Output with Err set and the last lines
// of the output in its message.
func (e OSExec) Run(ctx context.Context, c Cmd) Output {
	name, env := c.Name, c.Env
	if e.Path != "" {
		p, err := e.LookPath(c.Name)
		if err != nil {
			return Output{Code: -1, Err: err}
		}
		name, env = p, append([]string{"PATH=" + e.Path}, c.Env...)
	}
	runCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, name, c.Args...)
	cmd.Dir = cmp.Or(c.Dir, "/")
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	killGroup(cmd, killGrace)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	o := Output{Out: out.String()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		o.Code, o.Err = -1, fmt.Errorf("%s: %w", c.Name, ctx.Err())
	case runCtx.Err() != nil:
		o.Code, o.Err = -1, fmt.Errorf("%s: timed out after %s", c.Name, c.Timeout)
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// ErrWaitDelay: it exited 0, and what it left running kept the
		// output open.
	case errors.As(err, &exitErr):
		o.Code = exitErr.ExitCode()
		o.Err = ExitError(c.Name, o.Code, o.Out)
	default:
		o.Code, o.Err = -1, err
	}
	return o
}

// LookPath is the binary Run runs for name: found in Path's directories
// when Path is set, else in the caller's PATH. A name with a slash is
// itself.
func (e OSExec) LookPath(name string) (string, error) {
	if e.Path == "" {
		return exec.LookPath(name)
	}
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, d := range filepath.SplitList(e.Path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w in %s", name, fs.ErrNotExist, e.Path)
}

// ExitError is the error of a command name that exited code, having
// printed out: its last lines go in the message. OSExec sets it, and so
// do fake Execs, so a test sees the error a host gives.
func ExitError(name string, code int, out string) error {
	if last := LastLines(out, 2); last != "" {
		return fmt.Errorf("%s: exit %d: %s", name, code, last)
	}
	return fmt.Errorf("%s: exit %d", name, code)
}

// short is a command expected to finish soon (a lookup, useradd): it is
// killed after a minute.
func short(name string, args ...string) Cmd {
	return Cmd{Name: name, Args: args, Timeout: time.Minute}
}

// run runs c and makes a failure an error. An Exec sets Err for every
// failure (OSExec's names the exit and the output's last lines).
func run(ctx context.Context, e Exec, c Cmd) (Output, error) {
	o := e.Run(ctx, c)
	return o, o.Err
}

// cmdPlan is what a resource's Check found to do: the commands that put
// it in place, each with what it does.
type cmdPlan struct {
	cmds  []Cmd
	drift []string
}

func (p *cmdPlan) add(c Cmd, why string) {
	p.cmds, p.drift = append(p.cmds, c), append(p.drift, why)
}

// state is in place when there is nothing to run, else drift whose Data
// is the commands (runCmds runs them).
func (p *cmdPlan) state() (State, error) {
	if len(p.cmds) == 0 {
		return inPlace()
	}
	return driftData(p.cmds, "%s", strings.Join(p.drift, "; "))
}

// runCmds runs the commands a cmdPlan's state holds, in order.
func runCmds(ctx context.Context, x Exec, st State) (Result, error) {
	for _, c := range st.Data.([]Cmd) {
		if _, err := run(ctx, x, c); err != nil {
			return Result{}, err
		}
	}
	return changed("%s", st.Drift)
}

// tail is the last n non-empty lines of out.
func tail(out string, n int) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimRight(l, " \r\t"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// LastLines is the last n non-empty lines of out, joined with " | ".
func LastLines(out string, n int) string { return strings.Join(tail(out, n), " | ") }
