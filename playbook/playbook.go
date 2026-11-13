// Package playbook loads a YAML playbook into a host.Play, through the
// module registry it is given (Options.Registry), and runs it. docs/playbooks.md
// is the format's reference.
//
// A playbook is a list of steps; a step is a list of tasks and the
// handlers they notify; a task is one module and its arguments:
//
//	name: web
//	vars:
//	  SSH_PORT: 22
//	steps:
//	  - name: ssh
//	    tasks:
//	      - name: sshd port
//	        file: {path: /etc/ssh/sshd_config.d/port.conf, content: "Port ${SSH_PORT}\n"}
//	        notify: reload ssh
//	    handlers:
//	      - name: reload ssh
//	        command: {cmd: [systemctl, reload, ssh]}
//
// The module names and their arguments (file, command) are those of the
// registry in Options.Registry; there is no default registry.
//
// Variables (${NAME}) are expanded when the playbook loads. There are no
// templates and no loops. A when condition is read when the play reaches
// the task, so it can test the host's facts and the results earlier tasks
// registered.
package playbook

import (
	"context"
	"fmt"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
	"go.yaml.in/yaml/v3"
)

// Options are how Load reads a playbook.
type Options struct {
	// Registry is the modules the playbook's tasks may name; required.
	// builtin.Registry() is the standard ones. A playbook naming a module
	// not in it does not load.
	Registry *module.Registry
	// Env is what the modules act through. When Files is nil, Load sets it
	// to the files directory next to the playbook, when there is one. The
	// tasks of an included playbook get the files directory next to it.
	Env module.Env
	// Environ is the environment, as os.Environ gives it. Each
	// SHIGA_VAR_NAME in it sets the variable NAME.
	Environ []string
	// VarFiles are YAML files that map variable names to values, read in
	// order: a later file's value wins over an earlier one's.
	VarFiles []string
	// Set are variables set one by one (shiga --var), over every other
	// source.
	Set map[string]string
	// Only are the steps to load, each named by its name or one of its
	// tags; empty loads every step but the manual ones. Check ignores it.
	Only []string
	// AllowUndefined, for Check only: an undefined variable is not a
	// problem (it is meant to be set when the playbook is applied), and
	// the arguments, when or name that use one are left unchecked.
	AllowUndefined bool
}

// Playbook is a loaded playbook, ready to run. Its runs read and write
// what tasks registered, so they must not overlap.
type Playbook struct {
	Name string
	Path string
	// Files are the playbook files loaded, in the order their steps run:
	// the included ones, then Path.
	Files []string
	// Steps are the names of the steps loaded, in order.
	Steps []string
	// Vars are the variables, resolved.
	Vars map[string]string

	play    host.Play
	records map[string]*record
	facts   *factCache
}

// Apply runs the playbook (host.Runner.Run).
func (p *Playbook) Apply(ctx context.Context, r *host.Runner) error {
	p.reset("apply")
	return r.Run(ctx, p.play)
}

// Plan reports what Apply would change (host.Runner.Plan).
func (p *Playbook) Plan(ctx context.Context, r *host.Runner) error {
	p.reset("plan")
	return r.Plan(ctx, p.play)
}

// Verify reports drift (host.Runner.Verify).
func (p *Playbook) Verify(ctx context.Context, r *host.Runner) error {
	p.reset("verify")
	return r.Verify(ctx, p.play)
}

func (p *Playbook) reset(mode string) {
	for _, r := range p.records {
		*r = record{}
	}
	p.facts.reset(mode)
}

// record is what a task with register did in this run.
type record struct {
	ran, changed, failed, skipped bool
	// out is the last command the task ran, nil when it ran none.
	out *host.Output
}

// registered is a task's resource that keeps what it did in rec.
type registered struct {
	host.Resource
	rec *record
}

func (r *registered) Unwrap() host.Resource { return r.Resource }

func (r *registered) Check(ctx context.Context) (host.State, error) {
	st, err := r.Resource.Check(ctx)
	r.rec.ran = true
	r.rec.failed = err != nil && !host.IsWarning(err)
	r.rec.skipped = st.Skipped
	// In Plan and Verify there is no Apply: changed is what would change.
	r.rec.changed = err == nil && !st.InPlace
	return st, err
}

func (r *registered) Apply(ctx context.Context, st host.State) (host.Result, error) {
	got, err := r.Resource.Apply(ctx, st)
	r.rec.failed = err != nil && !host.IsWarning(err)
	r.rec.changed = err == nil && got.Status == host.Changed
	return got, err
}

// need is a task another one needs, by the name it registers.
type need struct {
	name string
	rec  *record
}

// needing is a task's resource that is not checked when a task it needs
// failed in this run: in plan and verify it is unknown with that reason,
// not a drift read from a host the failed task left as it was (a network
// on a swarm that is locked); in apply it fails.
type needing struct {
	host.Resource
	needs []need
}

func (n *needing) Unwrap() host.Resource { return n.Resource }

func (n *needing) Check(ctx context.Context) (host.State, error) {
	for _, d := range n.needs {
		if d.rec.failed {
			return host.State{}, fmt.Errorf("not checked: it needs %s, which failed", d.name)
		}
	}
	return n.Resource.Check(ctx)
}

// recordExec is an Exec that keeps the last command's output in rec,
// leaving out commands run aside (host.Aside: a guard, an owner lookup).
type recordExec struct {
	host.Exec
	rec *record
}

func (e recordExec) Run(ctx context.Context, c host.Cmd) host.Output {
	o := e.Exec.Run(ctx, c)
	if !host.IsAside(ctx) {
		e.rec.out = &o
	}
	return o
}

// errAt is an error at node n of file: "site.yml:12:5: ...". format may
// wrap an error with %w.
func errAt(file string, n *yaml.Node, format string, a ...any) error {
	return fmt.Errorf("%s:%d:%d: "+format, append([]any{file, n.Line, n.Column}, a...)...)
}
