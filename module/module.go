// Package module is how a playbook's tasks become resources. A module
// turns a task's arguments, as a YAML decoder gives them
// (map[string]any), into a host.Resource; a Registry maps the names a
// playbook uses to modules.
//
// There is no global registry: the program that loads a playbook passes
// one. A program that embeds shigawire, such as an engine with internal
// modules, runs its own playbooks with a registry of the built-ins plus
// its private modules, and untrusted playbooks with one of the built-ins
// only, so those cannot name the private modules. A package of modules
// exposes
//
//	func Register(r *module.Registry)
//
// as package builtin does for the standard ones; the program calls it on
// each registry the modules belong in.
//
// Decode fills a module's argument struct from the arguments, strictly: a
// key the struct has no field for is an error that names it.
package module

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/inference-sh/shigawire/host"
)

// Env is what a module's resources act through, shared by every task of
// a run.
type Env struct {
	// Exec runs the commands.
	Exec host.Exec
	// Apt is the run's one Apt (host.Apt is meant to be one per run).
	Apt *host.Apt
	// Root is the directory the host's paths are under: "" or "/" is the
	// host itself, a test sets a temporary directory. A module puts every
	// host path it acts on through Path. Commands run through Exec are not
	// moved under it.
	Root string
	// Files are the files that come with the playbook (its files/
	// directory), for an argument that names one ("src: motd"); nil when
	// there are none.
	Files fs.FS

	// describe, when set, asks a Typed Ctor for its arguments (Args).
	describe func([]Arg)
}

// Path is the host path p under Root.
func (e Env) Path(p string) string {
	if e.Root == "" || e.Root == "/" {
		return p
	}
	return filepath.Join(e.Root, p)
}

// Ctor makes a module's resource from a task's arguments.
type Ctor func(env Env, args map[string]any) (host.Resource, error)

// Registry maps module names to their Ctors. A program makes one
// (NewRegistry), adds the modules it allows (builtin.Register for the
// standard ones, a plugin package's Register for its own) and gives it
// to the playbook loader. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	ctors map[string]Ctor
}

// NewRegistry is an empty registry.
func NewRegistry() *Registry {
	return &Registry{ctors: map[string]Ctor{}}
}

// Register adds the module name, made by ctor. It panics when name is
// empty, ctor is nil or name is registered already: two modules with one
// name is a bug in the program that registers them.
func (r *Registry) Register(name string, ctor Ctor) {
	if name == "" || ctor == nil {
		panic("module: Register with an empty name or a nil ctor")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.ctors[name]; ok {
		panic(fmt.Sprintf("module: %q registered twice", name))
	}
	r.ctors[name] = ctor
}

// Lookup is the module registered as name.
func (r *Registry) Lookup(name string) (Ctor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.ctors[name]
	return c, ok
}

// Names are the registered modules' names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.ctors))
	for n := range r.ctors {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Typed is a Ctor that decodes the arguments into an A with Decode, then
// calls f with it. Args lists the keys of A for a module registered with
// it; a field's doc tag describes its key.
func Typed[A any](f func(env Env, args A) (host.Resource, error)) Ctor {
	return func(env Env, raw map[string]any) (host.Resource, error) {
		if env.describe != nil {
			var args []Arg
			if t := reflect.TypeFor[A](); t.Kind() == reflect.Struct {
				args = argsOf("", t, nil)
			}
			env.describe(args)
			return nil, errDescribe
		}
		var a A
		if err := Decode(raw, &a); err != nil {
			return nil, err
		}
		return f(env, a)
	}
}
