package playbook

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

// Report is what Check found.
type Report struct {
	// Steps and Tasks count the steps and tasks loaded; Tasks leaves out
	// handlers.
	Steps, Tasks int
	// Problems are the problems found, each "file:line[:col]: ...", in
	// the order of their lines; none when the playbook is good.
	Problems []error
}

// Check loads the playbook at path as Load does, without acting on
// anything: no command runs and no file of the host is read, and no
// task is checked against the host. It reports every problem it finds,
// where Load stops at the first one:
//
//   - YAML errors and errors in the playbook's structure (these stop it:
//     what follows them cannot be read);
//   - a module the registry does not have;
//   - a module's argument errors, from building each task's resource
//     through the registry with an Exec that runs nothing and a Root
//     that is a new empty directory;
//   - an undefined variable, unless opt.AllowUndefined;
//   - a when that does not parse;
//   - a notify or a handler's needs naming no handler, a task's needs
//     naming no earlier registered task;
//   - a relative src (or apt_repo key_src) missing from the playbook's
//     files/ directory.
//
// Every step is checked, the manual ones too: opt.Only is ignored, and
// so is opt.Env but for its Files. A task whose when is false when the
// playbook loads (it reads only variables) has its module looked up and
// its arguments left unchecked, as Load leaves them.
//
// What a module reads from the host when the task runs is not checked:
// an owner or group that must exist, an absolute src, an apt package
// that must be known. The modules that read nothing at build time
// (all the standard ones) are checked fully otherwise.
func Check(path string, opt Options) Report {
	var rep Report
	chk := &checkState{allowUndefined: opt.AllowUndefined, seen: map[string]bool{}}
	root, err := os.MkdirTemp("", "shiga-check-")
	if err != nil {
		rep.Problems = append(rep.Problems, err)
		return rep
	}
	defer os.RemoveAll(root)
	files := opt.Env.Files
	opt.Env = module.NewEnv(noExec{}, root)
	opt.Env.Files = files
	opt.Only = nil
	l, err := loadChecked(path, opt, chk)
	if err != nil {
		// A YAML type error is a line per value.
		for _, line := range strings.Split(err.Error(), "\n") {
			chk.add(errors.New(line))
		}
	}
	if l != nil {
		rep.Steps, rep.Tasks = len(l.pb.Steps), l.tasks
	}
	rep.Problems = chk.problems
	order := []string{path}
	if l != nil && len(l.pb.Files) > 0 {
		order = l.pb.Files
	}
	// In the order of the files, as their steps run, and of the lines in
	// each: a handler is built when a task first notifies it, after the
	// tasks before that one.
	slices.SortStableFunc(rep.Problems, func(a, b error) int {
		fa, la, ca := position(order, a)
		fb, lb, cb := position(order, b)
		return cmp.Or(cmp.Compare(fa, fb), cmp.Compare(la, lb), cmp.Compare(ca, cb))
	})
	return rep
}

// position is the index in files of the file at the start of err's
// message, and the line and column after it, when it is
// "file:line[:col]: ..."; -1, 0, 0 otherwise.
func position(files []string, err error) (file, line, col int) {
	file = -1
	var rest string
	for i, f := range files {
		if r, ok := strings.CutPrefix(err.Error(), f+":"); ok {
			file, rest = i, r
			break
		}
	}
	if file < 0 {
		return -1, 0, 0
	}
	parts := strings.SplitN(rest, ":", 3)
	line, _ = strconv.Atoi(parts[0])
	if len(parts) > 1 {
		col, _ = strconv.Atoi(parts[1])
	}
	return file, line, col
}

// checkState is what Check keeps while the loader runs.
type checkState struct {
	allowUndefined bool
	problems       []error
	seen           map[string]bool
}

// add keeps err, once: a handler two tasks notify is built once, but a
// problem found on two paths to it is the same problem.
func (c *checkState) add(err error) {
	if msg := err.Error(); !c.seen[msg] {
		c.seen[msg] = true
		c.problems = append(c.problems, err)
	}
}

// errNoExec is what noExec's commands fail with.
var errNoExec = errors.New("shiga check runs no command")

// noExec is the Exec of Check's modules: it runs nothing. A module that
// ran a command while its resource was built would get this error.
type noExec struct{}

func (noExec) Run(context.Context, host.Cmd) host.Output {
	return host.Output{Code: 1, Err: errNoExec}
}
