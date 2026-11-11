package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeExec records the commands it is given and answers them with
// respond (nil: every command succeeds, saying nothing). A nonzero exit
// gets the Err OSExec would set.
//
// hosttest.Exec is the exported form of it, for tests outside package host; host's
// own tests cannot import hosttest, which imports host.
type fakeExec struct {
	calls   []string
	respond func(line string) Output
}

func (f *fakeExec) Run(_ context.Context, c Cmd) Output {
	line := c.String()
	f.calls = append(f.calls, line)
	if f.respond == nil {
		return Output{}
	}
	o := f.respond(line)
	if o.Code != 0 && o.Err == nil {
		o.Err = fmt.Errorf("%s: exit %d", c.Name, o.Code)
		if out := LastLines(o.Out, 2); out != "" {
			o.Err = fmt.Errorf("%s: exit %d: %s", c.Name, o.Code, out)
		}
	}
	return o
}

// ran is the calls that start with prefix.
func (f *fakeExec) ran(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func exit(code int) Output { return Output{Code: code} }

// runPlay runs p and returns every result.
func runPlay(ctx context.Context, p Play) ([]Result, error) {
	var got []Result
	err := (&Runner{OnResult: func(r Result) { got = append(got, r) }}).Run(ctx, p)
	return got, err
}

// apply runs r as a one-task play and returns its result.
func apply(t *testing.T, r Resource) Result {
	t.Helper()
	res, err := runPlay(context.Background(), Play{Tasks: []Task{{Resource: r}}})
	if err != nil {
		t.Fatalf("%s: %v", r.ID(), err)
	}
	return res[0]
}

// converge applies r, wants want, and applies it again: nothing changes.
func converge(t *testing.T, r Resource, want Status) Result {
	t.Helper()
	first := apply(t, r)
	if first.Status != want {
		t.Fatalf("%s: %s (%s), want %s", r.ID(), first.Status, first.Msg, want)
	}
	if again := apply(t, r); again.Status != OK {
		t.Fatalf("%s again: %s (%s), want ok", r.ID(), again.Status, again.Msg)
	}
	return first
}
