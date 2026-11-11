// Package modules is shigawire's standard modules: each turns a task's
// arguments into a host resource. Registry is a registry of them; Register
// adds them to a registry that holds other modules too:
//
//	reg := builtin.Registry()   // the standard modules
//	internal.Register(reg)      // a program's own modules beside them
//	pb, err := playbook.Load(path, playbook.Options{Registry: reg})
//
// The arguments are decoded strictly (module.Decode): a key a module does
// not take is an error. Names follow Ansible's where one fits; copy,
// lineinfile, blockinfile, get_url, shell, service and docker_compose are
// other names for file, line, block, download, command, systemd and
// compose. docs/modules.md lists every module with its arguments.
//
// Paths in arguments are the host's, and must be absolute. A module joins
// Env.Root to the paths it reads or writes itself; a command gets them as
// they are.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

// Cmd is a command as arguments give it: a string, run by /bin/sh -c, or
// a list, run as it is (the first item the program, found in PATH).
type Cmd []string

// UnmarshalText takes the string form.
func (c *Cmd) UnmarshalText(b []byte) error {
	if strings.TrimSpace(string(b)) == "" {
		return errors.New("an empty command")
	}
	*c = Cmd{"/bin/sh", "-c", string(b)}
	return nil
}

// cmd is c as a host.Cmd, an error when it is empty.
func (c Cmd) cmd(key string) (host.Cmd, error) {
	if len(c) == 0 || c[0] == "" {
		return host.Cmd{}, fmt.Errorf("%s: an empty command", key)
	}
	return host.Cmd{Name: c[0], Args: c[1:]}, nil
}

// List is a list of strings that also takes a single string.
type List []string

// UnmarshalText takes the single string.
func (l *List) UnmarshalText(b []byte) error {
	*l = List{string(b)}
	return nil
}

// absPath checks that the argument key holds an absolute path.
func absPath(key, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s is required", key)
	case !filepath.IsAbs(p):
		return fmt.Errorf("%s %q: want an absolute path", key, p)
	}
	return nil
}

// required checks that each key's value is set, in the order given.
func required(kv ...string) error {
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			return fmt.Errorf("%s is required", kv[i])
		}
	}
	return nil
}

// choice is the argument key's value v, one of allowed; "" is the first
// of them, the default.
func choice(key, v string, allowed ...string) (string, error) {
	if v == "" {
		return allowed[0], nil
	}
	if !slices.Contains(allowed, v) {
		return "", fmt.Errorf("%s %q: want %s", key, v, strings.Join(allowed, ", "))
	}
	return v, nil
}

// Owner is a path's owner and group, by name or number, as the host's
// user database has them (getent). An owner with no group gives the
// owner's primary group; a group with no owner is refused (host.Owner
// sets both).
type Owner struct {
	Owner string `yaml:"owner"`
	Group string `yaml:"group"`
}

func (o Owner) check() error {
	if o.Owner == "" && o.Group != "" {
		return errors.New("group needs owner")
	}
	return nil
}

// resolve reads the owner's and group's ids; nil when no owner is set.
func (o Owner) resolve(ctx context.Context, x host.Exec) (*host.Owner, error) {
	if o.Owner == "" {
		return nil, nil
	}
	ctx = host.Aside(ctx)
	p, err := host.LookupPasswd(ctx, x, o.Owner)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("there is no user %s", o.Owner)
	}
	own := &host.Owner{UID: p.UID, GID: p.GID}
	if o.Group != "" {
		g, err := host.LookupGroup(ctx, x, o.Group)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, fmt.Errorf("there is no group %s", o.Group)
		}
		own.GID = g.GID
	}
	return own, nil
}

// owned is the resource make makes with the owner o names: made at Check,
// when the play reaches it (the user may come from an earlier task), when
// o names one. An owner o cannot name is an error.
func owned(env module.Env, o Owner, id string, make func(*host.Owner) host.Resource) (host.Resource, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if o.Owner == "" {
		return make(nil), nil
	}
	return &host.Lazy{Name: id, Make: func(ctx context.Context) (host.Resource, error) {
		own, err := o.resolve(ctx, env.Exec)
		if err != nil {
			return nil, err
		}
		return make(own), nil
	}}, nil
}

// Register adds every standard module to r.
func Register(r *module.Registry) {
	registerFiles(r)
	registerRun(r)
	registerSystem(r)
	registerApt(r)
	registerUFW(r)
}

// Builtins is a new registry with the standard modules.
func Registry() *module.Registry {
	r := module.NewRegistry()
	Register(r)
	return r
}

// register registers ctor under each name.
func register(r *module.Registry, ctor module.Ctor, names ...string) {
	for _, n := range names {
		r.Register(n, ctor)
	}
}
