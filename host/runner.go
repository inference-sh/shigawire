package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
)

// Resource is one piece of host state, or one action.
//
// A state (File, Line, Service, an Apt package) is in place or not: Check
// reads the host, and Apply acts on what that Check found. An action
// (Restart, DaemonReload, a Command without a guard) has no state
// to read: its Check reports drift with State.Action set, every time, and
// its Apply always acts. An action is a handler, or a task meant to run
// every time.
type Resource interface {
	// ID names the resource and what it puts in place ("file /etc/fstab").
	ID() string
	// Check reads the host and reports whether the resource is in place.
	// It changes nothing.
	Check(ctx context.Context) (State, error)
	// Apply puts the resource in place. The Runner calls it right after
	// Check, with what Check found, and only when that was not in place.
	// It reports Changed with what it did; a failure is the error.
	Apply(ctx context.Context, st State) (Result, error)
}

// State is what Check found.
type State struct {
	InPlace bool
	// Drift is what Apply would change, when not in place ("mode 0644,
	// want 0600"), or why it was skipped. It never holds a file's content.
	Drift string
	// Action: the resource is an action. Its Check read no state, and its
	// Apply runs whenever the Runner applies it. Plan reports it as what
	// would run; Verify does not run it and reports it OK. A resource that
	// wraps another (Lazy, When) passes on the Action of the one it wraps.
	Action bool
	// Skipped: the resource does not apply here (a When whose condition
	// is false). InPlace is set too, so nothing applies it; the Runner
	// reports it Skipped.
	Skipped bool
	// Data is what Check read that its Apply acts on (the packages
	// missing, the key fetched), for that resource alone: the Runner hands
	// it on untouched.
	Data any
}

func inPlace() (State, error)                 { return State{InPlace: true}, nil }
func drift(f string, a ...any) (State, error) { return State{Drift: fmt.Sprintf(f, a...)}, nil }

// action is an action's State: drift, every time.
func action(f string, a ...any) (State, error) {
	return State{Drift: fmt.Sprintf(f, a...), Action: true}, nil
}

// driftData is drift with what Apply acts on.
func driftData(data any, f string, a ...any) (State, error) {
	return State{Drift: fmt.Sprintf(f, a...), Data: data}, nil
}
func changed(f string, a ...any) (Result, error) {
	return Result{Status: Changed, Msg: fmt.Sprintf(f, a...)}, nil
}
func unchanged(msg string) (Result, error) { return Result{Status: OK, Msg: msg}, nil }

// Status is how a task ended.
type Status int

const (
	// OK: in place already, nothing changed. In Verify, also an action,
	// which Verify does not run.
	OK Status = iota
	// Changed: Apply changed something. In Plan, Apply would; in Verify,
	// the resource is not in place (drift).
	Changed
	// Failed: Check or Apply failed. Only Run reports it.
	Failed
	// Ignored: failed, and the play went on (Task.IgnoreErrors).
	Ignored
	// Warn: Check or Apply returned a Warning; the play went on.
	Warn
	// Unknown: in Plan or Verify, Check failed, so whether the resource
	// is in place is not known (it may read what an earlier task would put
	// there).
	Unknown
	// Skipped: the task does not apply here (State.Skipped); nothing was
	// done.
	Skipped
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Changed:
		return "changed"
	case Failed:
		return "failed"
	case Ignored:
		return "ignored"
	case Warn:
		return "warn"
	case Unknown:
		return "unknown"
	case Skipped:
		return "skipped"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// warning is an error Warning marked.
type warning struct{ err error }

func (w *warning) Error() string { return w.err.Error() }
func (w *warning) Unwrap() error { return w.err }

// Warning marks err as a warning. A Check or an Apply that returns it is
// reported Warn, and the play goes on; the task notifies nothing. Warning
// of nil is nil.
func Warning(err error) error {
	if err == nil {
		return nil
	}
	return &warning{err}
}

// IsWarning reports whether err is, or wraps, one Warning marked.
func IsWarning(err error) bool {
	var w *warning
	return errors.As(err, &w)
}

// refusal is an error Refusal marked.
type refusal struct{ err error }

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

// Refusal marks err as a refusal: a Check that fails whatever the tasks
// before it change (a playbook's fail task), so the play stops there. Plan
// reports it Unknown like any failed Check; a caller can tell it from a
// Check that failed because an earlier task has not run (an owner a user
// task would create). Refusal of nil is nil.
func Refusal(err error) error {
	if err == nil {
		return nil
	}
	return &refusal{err}
}

// IsRefusal reports whether err is, or wraps, one Refusal marked.
func IsRefusal(err error) bool {
	var r *refusal
	return errors.As(err, &r)
}

// Result is one task's outcome, for the report.
type Result struct {
	Name    string // the task's name, or the resource's ID
	Status  Status
	Msg     string // what Apply did, else what Check found would change
	Err     error  // set when Failed, Ignored, Warn or Unknown
	Handler bool   // a handler, run because a task notified it
	// Action: Check said the resource is an action (State.Action).
	Action bool
	// Blocked: in Plan, an Unknown no change planned before it can
	// explain, so Run would fail there too: a Refusal, or a Check that
	// failed with no change planned before it. Another Unknown may read
	// what an earlier change would put in place (an owner a user task
	// creates).
	Blocked bool
}

// Task is a resource in a play.
type Task struct {
	// Name is the task's line in the report; "" uses the resource's ID.
	Name     string
	Resource Resource
	// Notify are the handlers to run when the task changed something.
	Notify []*Handler
	// IgnoreErrors: a failure is reported Ignored and the play goes on.
	IgnoreErrors bool

	flush bool
}

// Flush is a task that runs the handlers notified so far, there in the
// play instead of at its end: a unit written, then daemon-reload and a
// restart, then the unit enabled and started.
func Flush() Task { return Task{Name: "flush handlers", flush: true} }

// Handler is a resource that runs only when a task notified it, once,
// however many did.
type Handler struct {
	Name     string
	Resource Resource
	// Needs are the handlers this one depends on (a reload on the
	// validation of the config it reads). When one of them failed earlier
	// in the run, this one does not run and is reported Failed. One that
	// is notified too runs before this one.
	Needs []*Handler
}

func (h *Handler) name() string { return cmp.Or(h.Name, h.Resource.ID()) }

// Play is an ordered list of tasks and the handlers they may notify.
// Notified handlers run in the order Handlers lists them; one a task
// notifies that Handlers leaves out runs after those, in the order first
// notified.
type Play struct {
	Tasks    []Task
	Handlers []*Handler
}

// Runner runs plays: Run applies one, Plan and Verify only read the host.
type Runner struct {
	// OnResult, when set, gets each task's and handler's result as it
	// lands.
	OnResult func(Result)
}

// mode is what a run does with each resource.
type mode int

const (
	modeApply mode = iota
	modePlan
	modeVerify
)

// Run runs p's tasks in order. It stops at the first failure, and returns
// it as the error. The handlers notified before the failure still run
// (Ansible's force_handlers): a unit written is reloaded, and a service
// whose binary was replaced restarts on it. A Warning is reported Warn
// and the play goes on.
func (r *Runner) Run(ctx context.Context, p Play) error { return r.run(ctx, p, modeApply) }

// Plan runs every task's Check and no Apply: it reports what Run would do
// and changes nothing. A task not in place is reported Changed with its
// drift (an action: Result.Action, "run" when it says nothing more), and
// the handlers it notifies are planned in turn, as Run would run them. A
// Check that fails is Unknown (Result.Blocked when no earlier change can
// explain it), notifies nothing, and the plan goes on. The error is only
// for a play that cannot run, or ctx ending.
func (r *Runner) Plan(ctx context.Context, p Play) error { return r.run(ctx, p, modePlan) }

// Verify runs every task's Check and no Apply, and runs no handler: it
// reports drift. A task not in place is reported Changed with its drift;
// an action is reported OK (Result.Action) and not run; a Check that fails
// is Unknown. Verify reads every task, and returns an error when any was
// Changed or Unknown.
func (r *Runner) Verify(ctx context.Context, p Play) error { return r.run(ctx, p, modeVerify) }

func (p Play) validate() error {
	check := func(name string, hs []*Handler) error {
		for _, h := range hs {
			if h == nil || h.Resource == nil {
				return fmt.Errorf("%s a handler with no resource", name)
			}
			for _, n := range h.Needs {
				if n == nil || n.Resource == nil {
					return fmt.Errorf("handler %q needs a handler with no resource", h.Name)
				}
			}
		}
		return nil
	}
	if err := check("the play lists", p.Handlers); err != nil {
		return err
	}
	for _, t := range p.Tasks {
		if t.Resource == nil && !t.flush {
			return fmt.Errorf("task %q has no resource", t.Name)
		}
		if err := check(fmt.Sprintf("task %q notifies", t.name()), t.Notify); err != nil {
			return err
		}
	}
	return nil
}

// playRun is one run of a play.
type playRun struct {
	r    *Runner
	p    Play
	mode mode
	// notified are the handlers notified since the last flush, in the
	// order first notified; failed are those that failed in the run.
	notified []*Handler
	failed   map[*Handler]bool
	// drifted and unknown count Verify's Changed and Unknown.
	drifted, unknown int
	// changed: a task or handler reported Changed so far.
	changed bool
}

func (r *Runner) run(ctx context.Context, p Play, m mode) error {
	if err := p.validate(); err != nil {
		return err
	}
	if m == modeApply {
		ctx = withSaves(ctx, p)
	}
	pr := &playRun{r: r, p: p, mode: m, failed: map[*Handler]bool{}}
	for _, t := range p.Tasks {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, pr.flush(ctx))
		}
		if t.flush {
			if err := pr.flush(ctx); err != nil {
				return err
			}
			continue
		}
		res := pr.one(ctx, t.name(), t.Resource)
		if res.Status == Failed && t.IgnoreErrors {
			res.Status = Ignored
		}
		switch pr.report(res).Status {
		case Failed:
			return errors.Join(fmt.Errorf("%s: %w", res.Name, res.Err), pr.flush(ctx))
		case Changed:
			if m == modeVerify {
				continue
			}
			for _, h := range t.Notify {
				if !slices.Contains(pr.notified, h) {
					pr.notified = append(pr.notified, h)
				}
			}
		}
	}
	err := pr.flush(ctx)
	if m == modeVerify && (pr.drifted > 0 || pr.unknown > 0) {
		err = errors.Join(err, fmt.Errorf("%d not in place, %d unknown", pr.drifted, pr.unknown))
	}
	return err
}

func (pr *playRun) report(res Result) Result {
	if pr.mode == modePlan && res.Status == Unknown && (IsRefusal(res.Err) || !pr.changed) {
		res.Blocked = true
	}
	if res.Status == Changed {
		pr.changed = true
	}
	if pr.mode == modeVerify {
		switch res.Status {
		case Changed:
			pr.drifted++
		case Unknown:
			pr.unknown++
		}
	}
	if pr.r.OnResult != nil {
		pr.r.OnResult(res)
	}
	return res
}

// flush runs the handlers notified since the last flush: in the play's
// order, a handler's notified Needs before it.
func (pr *playRun) flush(ctx context.Context) error {
	var order []*Handler
	for _, h := range slices.Concat(pr.p.Handlers, pr.notified) {
		if slices.Contains(pr.notified, h) && !slices.Contains(order, h) {
			order = append(order, h)
		}
	}
	pr.notified = nil
	var errs []error
	done := map[*Handler]bool{}
	var runH func(h *Handler)
	runH = func(h *Handler) {
		if done[h] {
			return
		}
		done[h] = true
		for _, n := range h.Needs {
			if slices.Contains(order, n) {
				runH(n)
			}
		}
		name := h.name()
		var res Result
		if i := slices.IndexFunc(h.Needs, func(n *Handler) bool { return pr.failed[n] }); i >= 0 {
			res = Result{Name: name, Status: Failed, Err: fmt.Errorf("not run: handler %q failed", h.Needs[i].name())}
			res.Msg = res.Err.Error()
		} else {
			res = pr.one(ctx, name, h.Resource)
		}
		res.Handler = true
		if pr.report(res).Status == Failed {
			pr.failed[h] = true
			errs = append(errs, fmt.Errorf("handler %q: %w", name, res.Err))
		}
	}
	for _, h := range order {
		runH(h)
	}
	return errors.Join(errs...)
}

// one checks rc and, in Run, applies it, with what the check found, when
// it is not in place.
func (pr *playRun) one(ctx context.Context, name string, rc Resource) Result {
	res := Result{Name: name}
	fail := func(err error) Result {
		res.Err, res.Msg = err, err.Error()
		switch {
		case IsWarning(err):
			res.Status = Warn
		case pr.mode != modeApply:
			res.Status = Unknown
		default:
			res.Status = Failed
		}
		return res
	}
	st, err := rc.Check(ctx)
	if err != nil {
		return fail(err)
	}
	res.Action = st.Action
	if st.Skipped {
		res.Status, res.Msg = Skipped, st.Drift
		return res
	}
	if st.InPlace {
		res.Status = OK
		return res
	}
	switch {
	case pr.mode == modePlan:
		res.Status, res.Msg = Changed, cmp.Or(st.Drift, "run")
		return res
	case pr.mode == modeVerify && st.Action:
		res.Status, res.Msg = OK, "action, not run"
		return res
	case pr.mode == modeVerify:
		res.Status, res.Msg = Changed, st.Drift
		return res
	}
	got, err := rc.Apply(ctx, st)
	if err != nil {
		return fail(err)
	}
	res.Status, res.Msg = got.Status, cmp.Or(got.Msg, st.Drift)
	return res
}

func (t Task) name() string {
	if t.Name != "" {
		return t.Name
	}
	if t.Resource != nil {
		return t.Resource.ID()
	}
	return ""
}

// All is many resources as one task, for a tree of them (a directory's
// copy) whose parts are not worth a line each. Check reads every part;
// Apply puts in place, in order, those that were not, and says how many.
// It is an action when every part not in place is one.
type All struct {
	// What names them ("the engine data").
	What  string
	Parts []Resource
}

func (a *All) ID() string { return "all of " + a.What }

// allPart is a part not in place, with what its Check found.
type allPart struct {
	r  Resource
	st State
}

func (a *All) Check(ctx context.Context) (State, error) {
	var d []allPart
	for _, p := range a.Parts {
		st, err := p.Check(ctx)
		if err != nil {
			return State{}, fmt.Errorf("%s: %w", p.ID(), err)
		}
		if !st.InPlace {
			d = append(d, allPart{p, st})
		}
	}
	if len(d) == 0 {
		return inPlace()
	}
	st, err := driftData(d, "%d of %d to put in place", len(d), len(a.Parts))
	// An action when every part left to put in place is one.
	st.Action = !slices.ContainsFunc(d, func(p allPart) bool { return !p.st.Action })
	return st, err
}

func (a *All) Apply(ctx context.Context, st State) (Result, error) {
	d, _ := st.Data.([]allPart)
	n := 0
	for _, p := range d {
		r, err := p.r.Apply(ctx, p.st)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", p.r.ID(), err)
		}
		if r.Status == Changed {
			n++
		}
	}
	return changed("%s (%d of %d parts)", a.What, n, len(a.Parts))
}
