package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Validated is an action, for a handler: Validate checks a config the
// play changed (sshd -t, visudo -c, nginx -t); when it passes, Then runs
// (a reload), in order. When it fails, Then does not run, the handler
// fails, and each path in Restore that a File, Line or Block of the same
// run wrote is put back as it was before the run: its content, mode and
// owner, or removed when the run created it. A Restore path no task wrote
// in the run is left as it is. Handlers that need this one (Handler.Needs)
// do not run after it failed.
//
// The Runner sets up keeping the Restore paths before the run, from the
// play's tasks and handlers, looking through Wrappers (When). A Validated
// with Restore that it did not see (made by a Lazy, or applied outside a
// Runner) fails before it validates.
type Validated struct {
	// Name is its ID; "" is "validate" and the command.
	Name     string
	Exec     Exec
	Validate Cmd
	Then     []Cmd
	Restore  []string
}

func (v *Validated) ID() string { return cmp.Or(v.Name, "validate "+v.Validate.String()) }

func (v *Validated) Check(context.Context) (State, error) {
	return action("%s", v.Validate.String())
}

func (v *Validated) Apply(ctx context.Context, _ State) (Result, error) {
	if err := canRestore(ctx, v.Restore); err != nil {
		return Result{}, err
	}
	if o := v.Exec.Run(ctx, v.Validate); !o.OK() {
		err := fmt.Errorf("%s: %w", v.Validate.String(), o.Err)
		var did []string
		var errs []error
		for _, p := range v.Restore {
			msg, rerr := restore(ctx, p)
			if rerr != nil {
				errs = append(errs, fmt.Errorf("put back %s: %w", p, rerr))
			} else if msg != "" {
				did = append(did, msg)
			}
		}
		if len(did) > 0 {
			err = fmt.Errorf("%w (%s)", err, strings.Join(did, ", "))
		}
		return Result{}, errors.Join(append([]error{err}, errs...)...)
	}
	did := []string{"valid"}
	for _, c := range v.Then {
		if _, err := run(ctx, v.Exec, c); err != nil {
			return Result{}, err
		}
		did = append(did, c.String())
	}
	return changed("%s", strings.Join(did, "; "))
}

func (v *Validated) restores() []string { return v.Restore }

// restorer is a resource that puts back files the run wrote (Validated).
type restorer interface{ restores() []string }

// Wrapper is a resource that runs another one (When, a caller's own
// wrapper): Unwrap returns it. The Runner looks through wrappers for what
// it sets up before a run, such as the files a Validated may put back.
type Wrapper interface {
	Resource
	Unwrap() Resource
}

// restoresOf is what r, or the resource it wraps, puts back.
func restoresOf(r Resource) []string {
	for r != nil {
		if rs, ok := r.(restorer); ok {
			return rs.restores()
		}
		w, ok := r.(Wrapper)
		if !ok {
			return nil
		}
		r = w.Unwrap()
	}
	return nil
}

// canRestore is an error when a path in paths was not set up to be kept
// in this run: a Validated the Runner did not see before the run (made
// by a Lazy, in a wrapper that is not a Wrapper, or applied outside a
// Runner) could not put it back.
func canRestore(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	s, _ := ctx.Value(saveKey{}).(*saves)
	for _, p := range paths {
		if s == nil || !s.want[filepath.Clean(p)] {
			return fmt.Errorf("restore %s: the run did not set up keeping it (a Validated with Restore must be a task or handler of the play, directly or in a Wrapper)", p)
		}
	}
	return nil
}

type saveKey struct{}

// saves are the files a run's handlers may put back (want, by path) and
// what each held before the run first wrote it.
type saves struct {
	mu   sync.Mutex
	want map[string]bool
	got  map[string]*saved
}

// saved is a file as it was before the run wrote it.
type saved struct {
	exists  bool
	link    string // a link's target; "" for a regular file
	content []byte
	mode    fs.FileMode
	owner   *Owner
}

// withSaves is ctx with where the writers keep what the play's handlers
// may put back, when any may.
func withSaves(ctx context.Context, p Play) context.Context {
	want := map[string]bool{}
	add := func(r Resource) {
		for _, path := range restoresOf(r) {
			want[filepath.Clean(path)] = true
			if real, err := filepath.EvalSymlinks(path); err == nil {
				want[real] = true
			}
		}
	}
	for _, h := range p.Handlers {
		add(h.Resource)
	}
	for _, t := range p.Tasks {
		add(t.Resource)
		for _, h := range t.Notify {
			add(h.Resource)
		}
	}
	if len(want) == 0 {
		return ctx
	}
	return context.WithValue(ctx, saveKey{}, &saves{want: want, got: map[string]*saved{}})
}

// saveBefore keeps what path holds, before an Apply writes it, when a
// handler of the play may put it back and nothing kept it yet in this
// run. One it cannot keep is an error: the write would not be undone.
func saveBefore(ctx context.Context, path string) error {
	s, _ := ctx.Value(saveKey{}).(*saves)
	if s == nil {
		return nil
	}
	path = filepath.Clean(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.want[path] || s.got[path] != nil {
		return nil
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.got[path] = &saved{}
		return nil
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		s.got[path] = &saved{exists: true, link: target}
		return nil
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is %s: it cannot be kept to put back", path, kind(fi))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sv := &saved{exists: true, content: b, mode: fi.Mode().Perm()}
	if o, ok := FileOwner(fi); ok {
		sv.owner = &o
	}
	s.got[path] = sv
	return nil
}

// restore puts path back as saveBefore kept it, and says what it did ("":
// the run did not write it).
func restore(ctx context.Context, path string) (string, error) {
	s, _ := ctx.Value(saveKey{}).(*saves)
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := filepath.Clean(path)
	sv := s.got[key]
	if sv == nil {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			key, sv = real, s.got[real]
		}
	}
	if sv == nil {
		return "", nil
	}
	delete(s.got, key)
	switch {
	case !sv.exists:
		if err := os.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return "removed " + key, nil
	case sv.link != "":
		if _, err := (&Symlink{Path: key, Target: sv.link, Force: true}).Apply(ctx, State{}); err != nil {
			return "", err
		}
	default:
		if err := writeAtomic(key, strings.NewReader(string(sv.content)), sv.mode, sv.owner); err != nil {
			return "", err
		}
	}
	return "put back " + key, nil
}
