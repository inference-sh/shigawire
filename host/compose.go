package host

import "context"

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
