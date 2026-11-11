package host

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Sysctl is a kernel parameter set in a sysctl.d drop-in and in the
// running kernel. The drop-in holds "key = value" in place of any line
// that sets the key; when the line changed, or the kernel has another
// value, sysctl -p loads the drop-in (every key in it), and the kernel's
// value is read again: one that still differs is an error.
type Sysctl struct {
	Exec Exec
	// Path is the drop-in, as the host knows it
	// (/etc/sysctl.d/99-local.conf).
	Path  string
	Key   string
	Value string
	// Root is where the host's files are ("" is /).
	Root string
}

func (s *Sysctl) ID() string { return "sysctl " + s.Key + " = " + s.Value }

func (s *Sysctl) line() *Line {
	return &Line{
		Path:    filepath.Join(cmp.Or(s.Root, "/"), s.Path),
		Line:    s.Key + " = " + s.Value,
		Replace: regexp.MustCompile(`^\s*-?` + regexp.QuoteMeta(s.Key) + `\s*=`),
	}
}

// oneSpaced is a value with its whitespace made single spaces: the kernel
// prints some values tab-separated (net.ipv4.ip_local_port_range).
func oneSpaced(v string) string { return strings.Join(strings.Fields(v), " ") }

// live is the kernel's value of the key.
func (s *Sysctl) live(ctx context.Context) (string, error) {
	o, err := run(ctx, s.Exec, Cmd{Name: "sysctl", Args: []string{"-n", s.Key}, Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	return oneSpaced(o.Out), nil
}

// sysctlTodo is what Sysctl.Check found: the line's state, when it is to
// be written.
type sysctlTodo struct {
	line *Line
	st   *State
}

func (s *Sysctl) Check(ctx context.Context) (State, error) {
	l := s.line()
	st, err := l.Check(ctx)
	if err != nil {
		return State{}, err
	}
	got, err := s.live(ctx)
	if err != nil {
		return State{}, err
	}
	var d []string
	t := sysctlTodo{line: l}
	if !st.InPlace {
		t.st = &st
		d = append(d, s.Path+": "+st.Drift)
	}
	if got != oneSpaced(s.Value) {
		d = append(d, fmt.Sprintf("the kernel has %q", got))
	}
	if len(d) == 0 {
		return inPlace()
	}
	return driftData(t, "%s", strings.Join(d, "; "))
}

func (s *Sysctl) Apply(ctx context.Context, st State) (Result, error) {
	t := st.Data.(sysctlTodo)
	if t.st != nil {
		if _, err := t.line.Apply(ctx, *t.st); err != nil {
			return Result{}, err
		}
	}
	if _, err := run(ctx, s.Exec, Cmd{Name: "sysctl", Args: []string{"-p", s.Path}, Timeout: time.Minute}); err != nil {
		return Result{}, err
	}
	got, err := s.live(ctx)
	if err != nil {
		return Result{}, err
	}
	if got != oneSpaced(s.Value) {
		return Result{}, fmt.Errorf("sysctl -p %s ran, and the kernel has %s = %q", s.Path, s.Key, got)
	}
	return changed("%s", st.Drift)
}
