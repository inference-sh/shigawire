// Package hosttest is what tests of plays and modules run against instead
// of a host: Exec, a fake host.Exec that records the commands it is given
// and answers them as the test says, and Host, a temporary directory that
// stands for the host's filesystem.
//
// Resources that read and write files (File, Line, Block, Dir, Symlink)
// take full paths, so a test points them under Host.Root with Host.Path.
// Resources that run commands get Host.Exec.
package hosttest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/inference-sh/shigawire/host"
)

// Exec records the commands it is given and answers them: with the first
// On rule whose prefix the command line starts with, else with Respond,
// else with RespondCmd, else with success and no output. A nonzero exit
// with no Err gets the Err host.OSExec would set. It is safe for concurrent
// use.
type Exec struct {
	// Respond answers a command line no On rule matched; nil: RespondCmd.
	Respond func(line string) host.Output
	// RespondCmd answers a command no On rule matched when Respond is nil,
	// given the command itself (its arguments as they are, where the line
	// joins them with spaces); nil: success. Package sim sets it.
	RespondCmd func(c host.Cmd) host.Output

	mu    sync.Mutex
	calls []string
	rules []rule
}

type rule struct {
	prefix string
	out    host.Output
}

// On answers every command whose line (host.Cmd.String) starts with prefix
// with o. Rules are tried in the order added.
func (f *Exec) On(prefix string, o host.Output) *Exec {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{prefix, o})
	return f
}

func (f *Exec) Run(_ context.Context, c host.Cmd) host.Output {
	line := c.String()
	f.mu.Lock()
	f.calls = append(f.calls, line)
	var o host.Output
	matched := false
	for _, r := range f.rules {
		if strings.HasPrefix(line, r.prefix) {
			o, matched = r.out, true
			break
		}
	}
	respond, respondCmd := f.Respond, f.RespondCmd
	f.mu.Unlock()
	switch {
	case matched:
	case respond != nil:
		o = respond(line)
	case respondCmd != nil:
		o = respondCmd(c)
	}
	if o.Code != 0 && o.Err == nil {
		o.Err = fmt.Errorf("%s: exit %d", c.Name, o.Code)
		if out := host.LastLines(o.Out, 2); out != "" {
			o.Err = fmt.Errorf("%s: exit %d: %s", c.Name, o.Code, out)
		}
	}
	return o
}

// Calls are the command lines run so far, in order.
func (f *Exec) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Ran is the command lines run so far that start with prefix.
func (f *Exec) Ran(prefix string) []string {
	var out []string
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// Reset forgets the commands run so far; the rules stay.
func (f *Exec) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Exit is an output with exit code code.
func Exit(code int) host.Output { return host.Output{Code: code} }

// Out is an output that exits 0 and prints out.
func Out(out string) host.Output { return host.Output{Out: out} }

// Host is a fake host: a temporary root directory, removed when the test
// ends, and an Exec.
type Host struct {
	T    testing.TB
	Root string
	Exec *Exec
}

// New is a Host for t.
func New(t testing.TB) *Host {
	t.Helper()
	return &Host{T: t, Root: t.TempDir(), Exec: &Exec{}}
}

// Path is p (a host path, "/etc/fstab") under the root.
func (h *Host) Path(p string) string { return filepath.Join(h.Root, p) }

// Write writes content to the host path p, with its parents, mode 0644.
func (h *Host) Write(p, content string) {
	h.T.Helper()
	full := h.Path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.T.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.T.Fatal(err)
	}
}

// Read is the content of the host path p; the test fails when it cannot
// be read.
func (h *Host) Read(p string) string {
	h.T.Helper()
	b, err := os.ReadFile(h.Path(p))
	if err != nil {
		h.T.Fatal(err)
	}
	return string(b)
}

// Exists reports whether the host path p exists (a link is not followed).
func (h *Host) Exists(p string) bool {
	_, err := os.Lstat(h.Path(p))
	return err == nil
}

// Mode is one of host.Runner's entry points: (*host.Runner).Run, Plan or
// Verify.
type Mode func(r *host.Runner, ctx context.Context, p host.Play) error

var (
	Apply  Mode = (*host.Runner).Run
	Plan   Mode = (*host.Runner).Plan
	Verify Mode = (*host.Runner).Verify
)

// Run runs p with m and returns every result, in the order reported, and
// the run's error.
func Run(m Mode, p host.Play) ([]host.Result, error) {
	var got []host.Result
	r := &host.Runner{OnResult: func(r host.Result) { got = append(got, r) }}
	err := m(r, context.Background(), p)
	return got, err
}

// ApplyOne runs r as a one-task play and returns its result; the play's
// error fails the test.
func ApplyOne(t testing.TB, r host.Resource) host.Result {
	t.Helper()
	got, err := Run(Apply, host.Play{Tasks: []host.Task{{Resource: r}}})
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

// Converge applies r and wants want, then applies it again and wants OK:
// the second run changes nothing. It returns the first result.
func Converge(t testing.TB, r host.Resource, want host.Status) host.Result {
	t.Helper()
	first := ApplyOne(t, r)
	if first.Status != want {
		t.Fatalf("%s: %s (%s), want %s", r.ID(), first.Status, first.Msg, want)
	}
	if again := ApplyOne(t, r); again.Status != host.OK {
		t.Fatalf("%s again: %s (%s), want ok", r.ID(), again.Status, again.Msg)
	}
	return first
}

// Statuses is the results as "name=status" words, for comparing in a
// test.
func Statuses(rs []host.Result) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name + "=" + r.Status.String()
	}
	return strings.Join(out, " ")
}
