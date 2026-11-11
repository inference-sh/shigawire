package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stub is a resource whose state the test sets.
type stub struct {
	id      string
	inPlace bool
	fail    error
	log     *[]string
}

func (s *stub) ID() string { return s.id }
func (s *stub) Check(context.Context) (State, error) {
	return State{InPlace: s.inPlace, Drift: "drift " + s.id}, nil
}
func (s *stub) Apply(context.Context, State) (Result, error) {
	*s.log = append(*s.log, s.id)
	if s.fail != nil {
		return Result{}, s.fail
	}
	s.inPlace = true
	return Result{Status: Changed, Msg: "did " + s.id}, nil
}

func statuses(rs []Result) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name+"="+r.Status.String())
	}
	return strings.Join(out, " ")
}

func TestRunner_NotifiedHandlersRunOnceAtTheEnd(t *testing.T) {
	var log []string
	reload := &Handler{Name: "reload", Resource: &stub{id: "reload", log: &log}}
	restart := &Handler{Name: "restart", Resource: &stub{id: "restart", log: &log}}
	other := &Handler{Name: "other", Resource: &stub{id: "other", log: &log}}
	p := Play{
		Tasks: []Task{
			{Resource: &stub{id: "unit", log: &log}, Notify: []*Handler{restart, reload}},
			{Resource: &stub{id: "binary", log: &log}, Notify: []*Handler{restart}},
			{Resource: &stub{id: "config", inPlace: true, log: &log}, Notify: []*Handler{other}},
		},
		Handlers: []*Handler{reload, restart, other},
	}
	res, err := runMode((*Runner).Run, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"unit", "binary", "reload", "restart"}; !slices.Equal(log, want) {
		t.Fatalf("applied %v, want %v", log, want)
	}
	if got, want := statuses(res), "unit=changed binary=changed config=ok reload=changed restart=changed"; got != want {
		t.Fatalf("results %q, want %q", got, want)
	}
	if !res[3].Handler || res[0].Handler {
		t.Fatal("Handler marks the handlers' results only")
	}
}

// A handler the play does not list runs after those it lists, in the
// order notified.
func TestRunner_UnlistedHandlersRunLast(t *testing.T) {
	var log []string
	a := &Handler{Name: "a", Resource: &stub{id: "a", log: &log}}
	b := &Handler{Name: "b", Resource: &stub{id: "b", log: &log}}
	listed := &Handler{Name: "listed", Resource: &stub{id: "listed", log: &log}}
	p := Play{Tasks: []Task{{Resource: &stub{id: "t", log: &log}, Notify: []*Handler{b, a, listed}}}, Handlers: []*Handler{listed}}
	if _, err := runMode((*Runner).Run, p); err != nil {
		t.Fatal(err)
	}
	if want := []string{"t", "listed", "b", "a"}; !slices.Equal(log, want) {
		t.Fatalf("applied %v, want %v", log, want)
	}
}

func TestRunner_FlushRunsHandlersInPlace(t *testing.T) {
	var log []string
	reload := &Handler{Name: "reload", Resource: &stub{id: "reload", log: &log}}
	p := Play{
		Tasks: []Task{
			{Resource: &stub{id: "unit", log: &log}, Notify: []*Handler{reload}},
			Flush(),
			{Resource: &stub{id: "start", log: &log}},
		},
		Handlers: []*Handler{reload},
	}
	if _, err := runMode((*Runner).Run, p); err != nil {
		t.Fatal(err)
	}
	if want := []string{"unit", "reload", "start"}; !slices.Equal(log, want) {
		t.Fatalf("applied %v, want %v", log, want)
	}
}

// A failure stops the play; what was notified before it still runs (a
// unit written is reloaded), what was not does not.
func TestRunner_FailureStopsAndFlushes(t *testing.T) {
	var log []string
	boom := errors.New("boom")
	reload := &Handler{Name: "reload", Resource: &stub{id: "reload", log: &log}}
	restart := &Handler{Name: "restart", Resource: &stub{id: "restart", log: &log}}
	p := Play{
		Tasks: []Task{
			{Resource: &stub{id: "unit", log: &log}, Notify: []*Handler{reload}},
			{Resource: &stub{id: "broken", fail: boom, log: &log}, Notify: []*Handler{restart}},
			{Resource: &stub{id: "after", log: &log}},
		},
		Handlers: []*Handler{reload, restart},
	}
	res, err := runMode((*Runner).Run, p)
	if !errors.Is(err, boom) {
		t.Fatalf("err %v, want boom", err)
	}
	if want := []string{"unit", "broken", "reload"}; !slices.Equal(log, want) {
		t.Fatalf("applied %v, want %v", log, want)
	}
	if got, want := statuses(res), "unit=changed broken=failed reload=changed"; got != want {
		t.Fatalf("results %q, want %q", got, want)
	}
}

func TestRunner_RefusesABadPlay(t *testing.T) {
	var log []string
	for name, p := range map[string]Play{
		"empty handler": {Tasks: []Task{{Resource: &stub{id: "a", log: &log}, Notify: []*Handler{{Name: "nope"}}}}},
		"no resource":   {Tasks: []Task{{Name: "empty"}}},
	} {
		if _, err := runMode((*Runner).Run, p); err == nil {
			t.Errorf("%s: ran", name)
		}
	}
	if len(log) != 0 {
		t.Fatalf("a refused play applied %v", log)
	}
}

func TestRunner_StopsWhenCanceled(t *testing.T) {
	var log []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Runner{}).Run(ctx, Play{Tasks: []Task{{Resource: &stub{id: "a", log: &log}}}})
	if !errors.Is(err, context.Canceled) || len(log) != 0 {
		t.Fatalf("err %v, applied %v", err, log)
	}
}

// A task that may fail is reported ignored, and the play goes on.
func TestRunner_IgnoreErrors(t *testing.T) {
	var log []string
	p := Play{Tasks: []Task{
		{Name: "fm", IgnoreErrors: true, Resource: &stub{id: "fm", fail: errors.New("no unit"), log: &log}},
		{Name: "b", Resource: &stub{id: "b", log: &log}},
	}}
	got, err := runMode((*Runner).Run, p)
	if err != nil || statuses(got) != "fm=ignored b=changed" || got[0].Err == nil {
		t.Fatalf("%s %v", statuses(got), err)
	}
}

// Lazy makes its resource at Check, and its Apply acts on what that
// made; a nil resource is nothing to do.
func TestLazy(t *testing.T) {
	var log []string
	made := 0
	l := &Lazy{Name: "later", Make: func(context.Context) (Resource, error) {
		made++
		return &stub{id: "made", log: &log}, nil
	}}
	got, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: l}}})
	if err != nil || statuses(got) != "later=changed" || made != 1 || got[0].Msg != "did made" {
		t.Fatalf("%s %v, made %d", statuses(got), err, made)
	}
	none := &Lazy{Name: "none", Make: func(context.Context) (Resource, error) { return nil, nil }}
	if got, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: none}}}); err != nil || statuses(got) != "none=ok" {
		t.Fatalf("nothing to do: %s %v", statuses(got), err)
	}
	bad := &Lazy{Name: "bad", Make: func(context.Context) (Resource, error) { return nil, errors.New("no uuid") }}
	if _, err := bad.Check(context.Background()); err == nil {
		t.Fatal("a Make that failed passed")
	}
}

func TestAssertAndAction(t *testing.T) {
	var log []string
	h := &Handler{Name: "h", Resource: &stub{id: "h", log: &log}}
	p := Play{Tasks: []Task{
		{Name: "blank", Resource: &Assert{Name: "blank", Holds: func(context.Context) error { return nil }}},
		{Name: "key", Notify: []*Handler{h}, Resource: &Restart{Exec: &fakeExec{}, Name: "key"}},
		{Name: "stop", Resource: &Assert{Name: "stop", Holds: func(context.Context) error { return errors.New("not blank") }}},
		{Name: "never", Resource: &stub{id: "never", log: &log}},
	}, Handlers: []*Handler{h}}
	got, err := runMode((*Runner).Run, p)
	if err == nil || statuses(got) != "blank=ok key=changed stop=failed h=changed" || strings.Join(log, " ") != "h" {
		t.Fatalf("%s %v %v", statuses(got), err, log)
	}
}

// All is its parts as one task: in place when every part is, and its
// Apply puts in place, in order, only those that were not.
func TestAll(t *testing.T) {
	dir := t.TempDir()
	a := &All{What: "the data", Parts: []Resource{
		&Dir{Path: filepath.Join(dir, "d"), Mode: 0o700},
		&File{Path: filepath.Join(dir, "d", "f"), Content: []byte("x"), Mode: 0o600},
		&File{Path: filepath.Join(dir, "g"), Content: []byte("y"), Mode: 0o600},
	}}
	if err := os.WriteFile(filepath.Join(dir, "g"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := converge(t, a, Changed)
	if r.Msg != "the data (2 of 3 parts)" {
		t.Errorf("msg %q", r.Msg)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "d", "f")); err != nil || string(b) != "x" {
		t.Errorf("d/f: %q %v", b, err)
	}
}
