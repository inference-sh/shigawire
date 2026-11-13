package playbook

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
	"go.yaml.in/yaml/v3"
)

// Load reads the playbook at path, and the playbooks it includes (their
// steps first), and makes each task's resource with the module it names.
// Every error names the file and line it is at.
func Load(path string, opt Options) (*Playbook, error) {
	l, err := loadChecked(path, opt, nil)
	if err != nil {
		return nil, err
	}
	return l.pb, nil
}

// loadChecked is Load, returning the loader, as far as it got when there
// is an error; with chk set, it is Check's: every step is loaded, and
// the problems of each task are kept in chk and loading goes on.
func loadChecked(path string, opt Options, chk *checkState) (*loader, error) {
	if opt.Registry == nil {
		return nil, errors.New("playbook: Options.Registry is nil (builtin.Registry() is the standard modules)")
	}
	if opt.Env.Files == nil {
		opt.Env.Files = filesDir(path)
	}
	l := &loader{
		file:     path,
		reg:      opt.Registry,
		env:      opt.Env,
		handlers: map[string]*handlerDef{},
		records:  map[string]*record{},
		chk:      chk,
	}
	l.pb = &Playbook{Path: path, records: l.records, facts: &factCache{env: opt.Env}}
	srcs, err := l.read(path, opt.Env, nil, map[string]bool{})
	if err != nil {
		return l, err
	}
	top := srcs[len(srcs)-1]
	l.pb.Name = top.name
	defaults := map[string]string{}
	var hasSteps bool
	for _, src := range srcs {
		l.pb.Files = append(l.pb.Files, src.path)
		if src.vars != nil {
			if err := readVars(src.path, src.vars, defaults); err != nil {
				return l, err
			}
		}
		hasSteps = hasSteps || src.steps != nil
	}
	if l.vars, err = resolveVars(defaults, opt); err != nil {
		return l, err
	}
	l.pb.Vars = l.vars
	if !hasSteps {
		return l, errAt(path, top.root, "no steps")
	}
	var steps []*stepDef
	for _, src := range srcs {
		if src.steps == nil {
			continue
		}
		l.file, l.env = src.path, src.env
		if steps, err = l.parseSteps(src.steps, steps); err != nil {
			return l, err
		}
	}
	l.file, l.env = path, opt.Env
	if chk != nil {
		for _, s := range steps {
			s.selected = true
		}
	} else if err := l.selectSteps(steps, opt.Only); err != nil {
		return l, err
	}
	for _, s := range steps {
		if err := l.buildStep(s); err != nil {
			return l, err
		}
	}
	for _, s := range steps {
		for _, h := range s.handlers {
			if h.built != nil {
				l.pb.play.Handlers = append(l.pb.play.Handlers, h.built)
			}
		}
	}
	return l, nil
}

// source is one file of a playbook: the playbook Load was given, or one
// it includes.
type source struct {
	path string
	root *yaml.Node
	name string
	vars *yaml.Node
	// steps is nil when the file has no steps key.
	steps *yaml.Node
	// env is the Env of the file's tasks: its own files/ directory.
	env module.Env
}

// filesDir is the files directory next to the playbook at path; nil when
// there is none.
func filesDir(path string) fs.FS {
	dir := filepath.Join(filepath.Dir(path), "files")
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return os.DirFS(dir)
	}
	return nil
}

// fileKey is what tells two paths to one file apart from two files.
func fileKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return path
}

// read reads the playbook at path and, before it, the files it includes,
// depth first, in the order of each include list: the sources in the
// order their steps run, path's last. stack are the files including path
// (a cycle names them); seen are the files read already, which a second
// include of skips.
func (l *loader) read(path string, env module.Env, stack []string, seen map[string]bool) ([]*source, error) {
	key := fileKey(path)
	seen[key] = true
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, yamlErr(path, err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: empty playbook", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errAt(path, root, "a playbook is a mapping with name, include, vars and steps")
	}
	src := &source{path: path, root: root, env: env}
	var include *yaml.Node
	err = eachKey(path, root, func(k, v *yaml.Node) error {
		switch k.Value {
		case "name":
			return scalar(path, v, &src.name)
		case "include":
			include = v
		case "vars":
			src.vars = v
		case "steps":
			src.steps = v
		default:
			return errAt(path, k, "unknown key %q (a playbook has name, include, vars and steps)", k.Value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var srcs []*source
	if include != nil {
		var paths []string
		if err := scalars(path, include, &paths); err != nil {
			return nil, err
		}
		stack = append(slices.Clip(stack), path)
		for _, p := range paths {
			if p == "" {
				return nil, errAt(path, include, "include: an empty path")
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			k := fileKey(p)
			if i := slices.IndexFunc(stack, func(s string) bool { return fileKey(s) == k }); i >= 0 {
				return nil, errAt(path, include, "include %s: a cycle: %s -> %s", p, strings.Join(stack[i:], " -> "), p)
			}
			if seen[k] {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				return nil, errAt(path, include, "include %s: %v", p, errors.Unwrap(err))
			}
			ienv := env
			ienv.Files = filesDir(p)
			more, err := l.read(p, ienv, stack, seen)
			if err != nil {
				return nil, err
			}
			srcs = append(srcs, more...)
		}
	}
	return append(srcs, src), nil
}

type loader struct {
	file     string
	reg      *module.Registry
	env      module.Env
	vars     map[string]string
	handlers map[string]*handlerDef
	// records are the registered results by name; registered are those
	// registered by the tasks read so far.
	records    map[string]*record
	registered map[string]*record
	pb         *Playbook
	// chk is set when Check loads: problems are kept there.
	chk *checkState
	// tasks counts the tasks built.
	tasks int
}

// fail is err when loading stops at it; nil when Check keeps it (or,
// with AllowUndefined, drops it for an undefined variable) and loading
// goes on.
func (l *loader) fail(err error) error {
	if l.chk == nil || err == nil {
		return err
	}
	var u *undefinedError
	if l.chk.allowUndefined && errors.As(err, &u) {
		return nil
	}
	l.chk.add(err)
	return nil
}

type stepDef struct {
	node *yaml.Node
	// file and env are those of the playbook file the step is in.
	file string
	env  module.Env
	name string
	tags []string
	// manual: the step runs only when --only names it or one of its tags.
	manual   bool
	when     *yaml.Node
	tasks    []*yaml.Node
	handlers []*handlerDef
	selected bool
}

type handlerDef struct {
	node     *yaml.Node
	file     string
	env      module.Env
	name     string
	building bool
	built    *host.Handler
}

// parseSteps reads the steps n lists, of l.file, after steps (those of
// the files read before it).
func (l *loader) parseSteps(n *yaml.Node, steps []*stepDef) ([]*stepDef, error) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return steps, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errAt(l.file, n, "steps is a list")
	}
	for _, sn := range n.Content {
		if sn.Kind != yaml.MappingNode {
			return nil, errAt(l.file, sn, "a step is a mapping with name, tags, manual, when, tasks and handlers")
		}
		s := &stepDef{node: sn, file: l.file, env: l.env}
		err := eachKey(l.file, sn, func(k, v *yaml.Node) error {
			switch k.Value {
			case "name":
				return scalar(l.file, v, &s.name)
			case "tags":
				return scalars(l.file, v, &s.tags)
			case "manual":
				if v.Kind != yaml.ScalarNode || (v.Value != "true" && v.Value != "false") {
					return errAt(l.file, v, "manual: want true or false")
				}
				s.manual = v.Value == "true"
			case "when":
				s.when = v
			case "tasks":
				return seq(l.file, v, "tasks", &s.tasks)
			case "handlers":
				var hs []*yaml.Node
				if err := seq(l.file, v, "handlers", &hs); err != nil {
					return err
				}
				for _, hn := range hs {
					h := &handlerDef{node: hn, file: l.file, env: l.env}
					if err := l.handlerName(h); err != nil {
						return err
					}
					s.handlers = append(s.handlers, h)
				}
			default:
				return errAt(l.file, k, "unknown key %q (a step has name, tags, manual, when, tasks and handlers)", k.Value)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if s.name == "" {
			return nil, errAt(l.file, sn, "a step needs a name")
		}
		if i := slices.IndexFunc(steps, func(o *stepDef) bool { return o.name == s.name }); i >= 0 {
			return nil, errAt(l.file, sn, "step %q: a step of that name is at %s", s.name, l.at(steps[i].file, steps[i].node))
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// handlerName reads h's name, which must be unique in the playbook.
func (l *loader) handlerName(h *handlerDef) error {
	if h.node.Kind != yaml.MappingNode {
		return errAt(l.file, h.node, "a handler is a mapping with name, needs, when and one module")
	}
	for i := 0; i < len(h.node.Content); i += 2 {
		if h.node.Content[i].Value == "name" {
			if err := scalar(l.file, h.node.Content[i+1], &h.name); err != nil {
				return err
			}
		}
	}
	if h.name == "" {
		return errAt(l.file, h.node, "a handler needs a name (tasks notify it by name)")
	}
	if o := l.handlers[h.name]; o != nil {
		return errAt(l.file, h.node, "handler %q: a handler of that name is at %s", h.name, l.at(o.file, o.node))
	}
	l.handlers[h.name] = h
	return nil
}

// at is where n is, for a message about l.file: "line N" in l.file,
// "file:N" in another.
func (l *loader) at(file string, n *yaml.Node) string {
	if file == l.file {
		return fmt.Sprintf("line %d", n.Line)
	}
	return fmt.Sprintf("%s:%d", file, n.Line)
}

// selectSteps marks the steps only names (by name or tag); all of them
// but the manual ones when only is empty.
func (l *loader) selectSteps(steps []*stepDef, only []string) error {
	for _, s := range steps {
		s.selected = len(only) == 0 && !s.manual
	}
	for _, o := range only {
		found := false
		for _, s := range steps {
			if s.name == o || slices.Contains(s.tags, o) {
				s.selected, found = true, true
			}
		}
		if !found {
			return fmt.Errorf("%s: no step named or tagged %q", l.file, o)
		}
	}
	return nil
}

// buildStep makes the tasks of s, when it is selected, and records the
// names its tasks register either way, so a later step's when reads them
// (as not run, when s was not selected).
func (l *loader) buildStep(s *stepDef) error {
	if l.registered == nil {
		l.registered = map[string]*record{}
	}
	file, env := l.file, l.env
	l.file, l.env = s.file, s.env
	defer func() { l.file, l.env = file, env }()
	if !s.selected {
		for _, tn := range s.tasks {
			if err := l.declareRegister(tn); err != nil {
				return err
			}
		}
		return nil
	}
	l.pb.Steps = append(l.pb.Steps, s.name)
	stepConds, err := l.conds(s.when)
	if err != nil {
		if err = l.fail(err); err != nil {
			return err
		}
	}
	for _, tn := range s.tasks {
		l.tasks++
		t, err := l.task(s, tn, stepConds)
		if err != nil {
			if err = l.fail(err); err != nil {
				return err
			}
			continue
		}
		l.pb.play.Tasks = append(l.pb.play.Tasks, t)
	}
	for _, h := range s.handlers {
		if _, err := l.handler(h); err != nil {
			if err = l.fail(err); err != nil {
				return err
			}
		}
	}
	// A step's handlers run when it ends, before the next step.
	l.pb.play.Tasks = append(l.pb.play.Tasks, host.Flush())
	return nil
}

// declareRegister records the name task n registers, if any.
func (l *loader) declareRegister(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errAt(l.file, n, "a task is a mapping with one module")
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == "register" {
			_, err := l.register(n.Content[i+1])
			return err
		}
	}
	return nil
}

func (l *loader) register(v *yaml.Node) (*record, error) {
	var name string
	if err := scalar(l.file, v, &name); err != nil {
		return nil, err
	}
	switch {
	case !validName(name):
		return nil, errAt(l.file, v, "register %q: a name is letters, digits and _, not starting with a digit", name)
	case name == "os":
		return nil, errAt(l.file, v, "register %q: os is the facts' name", name)
	case l.records[name] != nil:
		return nil, errAt(l.file, v, "register %q: an earlier task registers that name", name)
	}
	rec := &record{}
	l.records[name], l.registered[name] = rec, rec
	return rec, nil
}

// taskKeys are the keys a task has beside its module.
var taskKeys = []string{"name", "notify", "when", "register", "needs", "ignore_errors"}

// handlerKeys are the keys a handler has beside its module.
var handlerKeys = []string{"name", "needs", "when"}

// parsed is a task or handler's keys, its module's aside.
type parsed struct {
	keys    map[string]*yaml.Node
	modKey  *yaml.Node
	modArgs *yaml.Node
}

func (l *loader) parse(n *yaml.Node, what string, known []string) (*parsed, error) {
	if n.Kind != yaml.MappingNode {
		return nil, errAt(l.file, n, "a %s is a mapping with one module", what)
	}
	p := &parsed{keys: map[string]*yaml.Node{}}
	err := eachKey(l.file, n, func(k, v *yaml.Node) error {
		if slices.Contains(known, k.Value) {
			p.keys[k.Value] = v
			return nil
		}
		if p.modKey != nil {
			return errAt(l.file, k, "%q: a %s has one module, and it is %q (other keys: %s)", k.Value, what, p.modKey.Value, strings.Join(known, ", "))
		}
		p.modKey, p.modArgs = k, v
		return nil
	})
	if err != nil {
		return nil, err
	}
	if p.modKey == nil {
		return nil, errAt(l.file, n, "the %s names no module", what)
	}
	return p, nil
}

func (l *loader) unknownModule(p *parsed) error {
	return errAt(l.file, p.modKey, "unknown module %q (shiga modules lists them)", p.modKey.Value)
}

// resource makes the resource of p's module, with env.
func (l *loader) resource(p *parsed, env module.Env) (host.Resource, error) {
	ctor, ok := l.reg.Lookup(p.modKey.Value)
	if !ok {
		return nil, l.unknownModule(p)
	}
	args := map[string]any{}
	switch {
	case p.modArgs.Kind == yaml.ScalarNode && p.modArgs.Tag == "!!null":
	case p.modArgs.Kind != yaml.MappingNode:
		return nil, errAt(l.file, p.modArgs, "%s: the arguments are a mapping", p.modKey.Value)
	default:
		if err := expandNode(l.file, p.modArgs, l.vars); err != nil {
			return nil, err
		}
		octalAsText(p.modArgs)
		if err := p.modArgs.Decode(&args); err != nil {
			return nil, yamlErr(l.file, err)
		}
	}
	r, err := ctor(env, args)
	if err != nil {
		return nil, errAt(l.file, p.modKey, "%s: %v", p.modKey.Value, err)
	}
	if r == nil {
		return nil, errAt(l.file, p.modKey, "%s: the module made no resource", p.modKey.Value)
	}
	return r, nil
}

func (l *loader) task(s *stepDef, n *yaml.Node, stepConds []*cond) (host.Task, error) {
	p, err := l.parse(n, "task", taskKeys)
	if err != nil {
		return host.Task{}, err
	}
	var t host.Task
	var name string
	if v := p.keys["name"]; v != nil {
		if err := scalar(l.file, v, &name); err != nil {
			return t, err
		}
		if name, err = expand(name, l.vars); err != nil {
			if err := l.fail(errAt(l.file, v, "%w", err)); err != nil {
				return t, err
			}
			name = v.Value
		}
	}
	if v := p.keys["ignore_errors"]; v != nil {
		if err := v.Decode(&t.IgnoreErrors); err != nil {
			if err := l.fail(errAt(l.file, v, "ignore_errors: want true or false")); err != nil {
				return t, err
			}
		}
	}
	// The conditions read what earlier tasks registered, not this one.
	conds, err := l.conds(p.keys["when"])
	if err != nil {
		if err := l.fail(err); err != nil {
			return t, err
		}
	}
	conds = append(slices.Clip(stepConds), conds...)
	needs, err := l.taskNeeds(p.keys["needs"])
	if err != nil {
		if err := l.fail(err); err != nil {
			return t, err
		}
	}
	var rec *record
	if v := p.keys["register"]; v != nil {
		if rec, err = l.register(v); err != nil {
			if err := l.fail(err); err != nil {
				return t, err
			}
		}
	}
	env := l.env
	if rec != nil && env.Exec != nil {
		env.Exec = recordExec{Exec: env.Exec, rec: rec}
	}
	r, err := l.build(p, env, conds, needs)
	if err != nil {
		return t, err
	}
	if rec != nil {
		r = &registered{Resource: r, rec: rec}
	}
	t.Resource = r
	t.Name = s.name + ": " + cmp.Or(name, r.ID())
	t.Notify, err = l.handlerRefs(p.keys["notify"], "notify")
	return t, err
}

// handlerRefs are the handlers v names (a task's notify, a handler's
// needs), each built; what is the key, for the messages.
func (l *loader) handlerRefs(v *yaml.Node, what string) ([]*host.Handler, error) {
	if v == nil {
		return nil, nil
	}
	var names []string
	if err := scalars(l.file, v, &names); err != nil {
		return nil, err
	}
	var out []*host.Handler
	for _, n := range names {
		hd := l.handlers[n]
		if hd == nil {
			if err := l.fail(errAt(l.file, v, "%s %q: no handler of that name", what, n)); err != nil {
				return nil, err
			}
			continue
		}
		h, err := l.handler(hd)
		if err != nil {
			if err := l.fail(err); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// build makes p's resource with env, under conds. When a condition is
// false at load time (constFalse) its module is not made, only looked up
// for check: its arguments may use variables left empty for it. A module
// that fails is notBuilt (check goes on past it). needs wrap it before
// the conditions.
func (l *loader) build(p *parsed, env module.Env, conds []*cond, needs []need) (host.Resource, error) {
	if why := constFalse(conds); why != "" {
		if l.chk != nil {
			if _, ok := l.reg.Lookup(p.modKey.Value); !ok {
				l.chk.add(l.unknownModule(p))
			}
		}
		return unbuilt(p, why), nil
	}
	r, err := l.resource(p, env)
	if err != nil {
		if err := l.fail(err); err != nil {
			return nil, err
		}
		r = notBuilt(p.modKey.Value)
	}
	if len(needs) > 0 {
		r = &needing{Resource: r, needs: needs}
	}
	return l.when(conds, r), nil
}

// taskNeeds reads a task's needs: names earlier tasks register.
func (l *loader) taskNeeds(v *yaml.Node) ([]need, error) {
	if v == nil {
		return nil, nil
	}
	var names []string
	if err := scalars(l.file, v, &names); err != nil {
		return nil, err
	}
	var out []need
	for _, n := range names {
		rec := l.registered[n]
		if rec == nil {
			return nil, errAt(l.file, v, "needs %q: no task before this one registers it", n)
		}
		out = append(out, need{n, rec})
	}
	return out, nil
}

// handler makes hd's handler, once, and those it needs.
func (l *loader) handler(hd *handlerDef) (*host.Handler, error) {
	if hd.built != nil {
		return hd.built, nil
	}
	if hd.building {
		return nil, errAt(l.file, hd.node, "handler %q needs itself, through the handlers it needs", hd.name)
	}
	hd.building = true
	file, env := l.file, l.env
	l.file, l.env = hd.file, hd.env
	defer func() { hd.building, l.file, l.env = false, file, env }()
	p, err := l.parse(hd.node, "handler", handlerKeys)
	if err != nil {
		return nil, err
	}
	h := &host.Handler{Name: hd.name}
	if h.Needs, err = l.handlerRefs(p.keys["needs"], "needs"); err != nil {
		return nil, err
	}
	// A handler runs only when a task that ran notified it, so its step's
	// when does not apply to it. Its own when reads variables and facts:
	// it runs after the step, and what tasks registered is not for it.
	saved := l.registered
	l.registered = map[string]*record{}
	conds, err := l.conds(p.keys["when"])
	l.registered = saved
	if err != nil {
		if err := l.fail(err); err != nil {
			return nil, err
		}
	}
	if h.Resource, err = l.build(p, l.env, conds, nil); err != nil {
		return nil, err
	}
	hd.built = h
	return h, nil
}

// constFalse is "when" and the first condition in conds that reads only
// variables and is false, known when the playbook loads; "" when there is
// none.
func constFalse(conds []*cond) string {
	for _, c := range conds {
		if c.exists != "" || c.subj.constant == nil {
			continue
		}
		if ok, err := c.eval(context.Background(), module.Env{}, nil); err == nil && !ok {
			return "when " + c.raw
		}
	}
	return ""
}

// unbuilt stands for a task whose when is false when the playbook loads
// (constFalse): its module is not made, so arguments that hold variables
// left empty for it are not checked. The play reports it skipped.
func unbuilt(p *parsed, why string) host.Resource {
	return &host.When{Why: why, Cond: func(context.Context) (bool, error) { return false, nil },
		Resource: notBuilt(p.modKey.Value)}
}

// notBuilt is the resource of a task that is never checked: its module's
// name.
type notBuilt string

func (n notBuilt) ID() string { return string(n) }
func (n notBuilt) Check(context.Context) (host.State, error) {
	return host.State{}, fmt.Errorf("%s: not built", string(n))
}
func (n notBuilt) Apply(context.Context, host.State) (host.Result, error) {
	return host.Result{}, fmt.Errorf("%s: not built", string(n))
}

// conds parses a when: one condition, or a list of them that must all
// hold.
func (l *loader) conds(n *yaml.Node) ([]*cond, error) {
	if n == nil {
		return nil, nil
	}
	var raws []string
	var nodes []*yaml.Node
	switch n.Kind {
	case yaml.ScalarNode:
		raws, nodes = []string{n.Value}, []*yaml.Node{n}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return nil, errAt(l.file, c, "a condition is one line")
			}
			raws, nodes = append(raws, c.Value), append(nodes, c)
		}
	default:
		return nil, errAt(l.file, n, "when is a condition or a list of them")
	}
	var out []*cond
	for i, raw := range raws {
		c, err := parseCond(raw, l.vars, l.registered)
		if err != nil {
			return nil, errAt(l.file, nodes[i], "when: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// when is r, run only while every condition holds.
func (l *loader) when(conds []*cond, r host.Resource) host.Resource {
	if len(conds) == 0 {
		return r
	}
	w := &host.When{Resource: r}
	env, facts := l.env, l.pb.facts
	w.Cond = func(ctx context.Context) (bool, error) {
		for _, c := range conds {
			ok, err := c.eval(ctx, env, facts)
			if err != nil || !ok {
				w.Why = "when " + c.raw
				return false, err
			}
		}
		return true, nil
	}
	return w
}

// eachKey calls f with each key of mapping n and its value, in order. A
// key given twice is an error.
func eachKey(file string, n *yaml.Node, f func(k, v *yaml.Node) error) error {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return errAt(file, k, "a key is a plain string")
		}
		if seen[k.Value] {
			return errAt(file, k, "%q given twice", k.Value)
		}
		seen[k.Value] = true
		if err := f(k, v); err != nil {
			return err
		}
	}
	return nil
}

func scalar(file string, n *yaml.Node, dst *string) error {
	if n.Kind != yaml.ScalarNode {
		return errAt(file, n, "want a single value")
	}
	*dst = n.Value
	return nil
}

// scalars reads one value, or a list of them.
func scalars(file string, n *yaml.Node, dst *[]string) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*dst = []string{n.Value}
		return nil
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return errAt(file, c, "want a single value")
			}
			*dst = append(*dst, c.Value)
		}
		return nil
	}
	return errAt(file, n, "want a value or a list of them")
}

func seq(file string, n *yaml.Node, what string, dst *[]*yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return errAt(file, n, "%s is a list", what)
	}
	*dst = n.Content
	return nil
}

var yamlLine = regexp.MustCompile(`^(?:yaml: )?line (\d+): `)

// yamlErr is a YAML decoder's error as "file:line: ...".
func yamlErr(file string, err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		var lines []string
		for _, e := range te.Errors {
			lines = append(lines, lineErr(file, e))
		}
		return errors.New(strings.Join(lines, "\n"))
	}
	return errors.New(lineErr(file, err.Error()))
}

func lineErr(file, msg string) string {
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return fmt.Sprintf("%s:%d: %s", file, n, msg[len(m[0]):])
		}
	}
	return file + ": " + strings.TrimPrefix(msg, "yaml: ")
}
