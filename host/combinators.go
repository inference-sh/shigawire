package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
)

// Lazy is a resource made when the play reaches it, from what the host is
// then: a filesystem's UUID after the mkfs before it, the driver branch an
// installer picked. Make runs at Check; a nil resource is nothing to do
// there (no headers meta-package for this kernel), in place.
type Lazy struct {
	// Name is its ID, what it will be ("the fstab line for /data").
	Name string
	Make func(ctx context.Context) (Resource, error)
}

// made is what a Lazy's Check made and found, for its Apply.
type made struct {
	r  Resource
	st State
}

func (l *Lazy) ID() string { return l.Name }

func (l *Lazy) Check(ctx context.Context) (State, error) {
	r, err := l.Make(ctx)
	if err != nil || r == nil {
		return State{InPlace: err == nil}, err
	}
	st, err := r.Check(ctx)
	if err != nil || st.InPlace {
		return st, err
	}
	return State{Drift: st.Drift, Action: st.Action, Data: made{r, st}}, nil
}

func (l *Lazy) Apply(ctx context.Context, st State) (Result, error) {
	m := st.Data.(made)
	return m.r.Apply(ctx, m.st)
}

// Assert is a condition the play stops at when it does not hold (a disk
// that is no longer blank). It changes nothing.
type Assert struct {
	Name string
	// Holds is nil when the condition holds, else why not.
	Holds func(ctx context.Context) error
	// Warn: a condition that does not hold is a Warning, and the play
	// goes on.
	Warn bool
}

func (a *Assert) ID() string { return a.Name }

func (a *Assert) Check(ctx context.Context) (State, error) {
	if err := a.Holds(ctx); err != nil {
		if a.Warn {
			return State{}, Warning(err)
		}
		return State{}, err
	}
	return inPlace()
}

// Apply is never called: Check either holds or fails.
func (a *Assert) Apply(context.Context, State) (Result, error) { return unchanged("holds") }

// Probe is state a command reads: in place when Test's output passes OK
// (nil OK: when Test exits 0). Apply runs Fix, in order, then runs Test
// again: still not in place is an error. With no Fix it is a check the
// play stops at, as Assert is: Check fails when it does not pass.
type Probe struct {
	// Name is its ID; "" is "probe" and the command.
	Name string
	Exec Exec
	Test Cmd
	OK   func(o Output) bool
	// Drift is what Check says when not in place; "" is Test's command
	// line and "fails".
	Drift string
	Fix   []Cmd
}

func (p *Probe) ID() string { return cmp.Or(p.Name, "probe "+p.Test.String()) }

func (p *Probe) passes(ctx context.Context) bool {
	o := p.Exec.Run(ctx, p.Test)
	if p.OK == nil {
		return o.OK()
	}
	return p.OK(o)
}

func (p *Probe) drift() string { return cmp.Or(p.Drift, p.Test.String()+" fails") }

func (p *Probe) Check(ctx context.Context) (State, error) {
	switch {
	case p.passes(ctx):
		return inPlace()
	case len(p.Fix) == 0:
		return State{}, errors.New(p.drift())
	}
	return drift("%s", p.drift())
}

func (p *Probe) Apply(ctx context.Context, st State) (Result, error) {
	var last string
	for _, c := range p.Fix {
		o, err := run(ctx, p.Exec, c)
		if err != nil {
			return Result{}, err
		}
		last = LastLines(o.Out, 1)
	}
	if !p.passes(ctx) {
		return Result{}, fmt.Errorf("ran %s, and still %s", p.Fix[len(p.Fix)-1].Name, p.drift())
	}
	return changed("%s", cmp.Or(last, st.Drift))
}

// When is Resource, only while Cond holds when the play reaches it (a
// part that runs only on a host with Docker). Cond false is in place and
// skipped (State.Skipped): nothing is done, and the Runner reports it
// Skipped. Otherwise its Check and Apply are Resource's.
type When struct {
	Cond     func(ctx context.Context) (bool, error)
	Resource Resource
	// Why is the skipped result's message ("not on a Docker host"); ""
	// says nothing.
	Why string
}

func (w *When) ID() string { return w.Resource.ID() }

func (w *When) Unwrap() Resource { return w.Resource }

func (w *When) Check(ctx context.Context) (State, error) {
	ok, err := w.Cond(ctx)
	if err != nil {
		return State{}, err
	}
	if !ok {
		return State{InPlace: true, Skipped: true, Drift: w.Why}, nil
	}
	return w.Resource.Check(ctx)
}

func (w *When) Apply(ctx context.Context, st State) (Result, error) {
	return w.Resource.Apply(ctx, st)
}

// Query is a read-only command, run when the resource is checked, so in
// Plan and Verify too. It is always in place and never applied: what it
// gives is the command's exit code and output, for a caller that records
// them (a playbook's register). A nonzero exit is not an error; a command
// that did not run to an exit (missing binary, timeout) is.
type Query struct {
	// Name is its ID; "" is "query" and the command.
	Name string
	Exec Exec
	Cmd  Cmd
}

func (q *Query) ID() string { return cmp.Or(q.Name, "query "+q.Cmd.String()) }

func (q *Query) Check(ctx context.Context) (State, error) {
	if o := q.Exec.Run(ctx, q.Cmd); o.Code < 0 && o.Err != nil {
		return State{}, o.Err
	}
	return inPlace()
}

func (q *Query) Apply(context.Context, State) (Result, error) {
	return Result{Status: OK}, nil
}
