package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

func registerRun(r *module.Registry) {
	register(r, module.Typed(newDownload), "download", "get_url")
	register(r, module.Typed(newCommand), "command", "shell")
	register(r, module.Typed(newCompose), "compose", "docker_compose")
	r.Register("docker_swarm", module.Typed(newSwarm))
	r.Register("assert", module.Typed(newAssert))
	r.Register("query", module.Typed(newQuery))
	r.Register("probe", module.Typed(newProbe))
	r.Register("fail", module.Typed(newFail))
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type downloadArgs struct {
	URL  string `yaml:"url"`
	Dest string `yaml:"dest"`
	// SHA256 is the hex digest the file must have; a file that has it is
	// not fetched again.
	SHA256 string      `yaml:"sha256"`
	Mode   fs.FileMode `yaml:"mode"`
	// Force fetches a file with no sha256 again each run; it is changed
	// only when the content is new.
	Force bool `yaml:"force"`
}

func newDownload(env module.Env, a downloadArgs) (host.Resource, error) {
	if err := required("url", a.URL); err != nil {
		return nil, err
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("url %q: want an http or https URL", a.URL)
	}
	if err := absPath("dest", a.Dest); err != nil {
		return nil, err
	}
	if a.SHA256 != "" && !sha256Hex.MatchString(a.SHA256) {
		return nil, fmt.Errorf("sha256 %q: want 64 hex digits", a.SHA256)
	}
	return &host.Download{URL: a.URL, Path: env.Path(a.Dest), SHA256: a.SHA256, Mode: a.Mode, Force: a.Force}, nil
}

type commandArgs struct {
	Cmd Cmd `yaml:"cmd"`
	// Creates: the command has run when this path exists.
	Creates string `yaml:"creates"`
	// Removes: the command has run when this path does not exist.
	Removes string `yaml:"removes"`
	// Unless: the command has run when this one exits 0.
	Unless  Cmd `yaml:"unless"`
	runArgs `yaml:",inline"`
	// Watch: the command changed something only when one of these
	// files' content did.
	Watch []string `yaml:"watch"`
	// ChangesPackages: the command installs or removes packages (an
	// installer that runs apt-get): apt reads what is installed again.
	ChangesPackages bool `yaml:"changes_packages"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runArgs are how a command runs, for the modules that run one.
type runArgs struct {
	// Chdir is the directory the command runs in; unset is /.
	Chdir string            `yaml:"chdir"`
	Env   map[string]string `yaml:"env"`
	// Timeout kills the command after this long ("10m").
	Timeout time.Duration `yaml:"timeout"`
}

// cmd is c run as r says; def is its timeout when r sets none (0: none).
func (r runArgs) cmd(c host.Cmd, def time.Duration) (host.Cmd, error) {
	if r.Chdir != "" {
		if err := absPath("chdir", r.Chdir); err != nil {
			return c, err
		}
	}
	if r.Timeout < 0 {
		return c, fmt.Errorf("timeout %s: want a positive duration", r.Timeout)
	}
	env, err := envList(r.Env)
	if err != nil {
		return c, err
	}
	c.Dir, c.Env, c.Timeout = r.Chdir, env, cmp.Or(r.Timeout, def)
	return c, nil
}

func newCommand(env module.Env, a commandArgs) (host.Resource, error) {
	c, err := a.Cmd.cmd("cmd")
	if err != nil {
		return nil, err
	}
	if c, err = a.runArgs.cmd(c, 0); err != nil {
		return nil, err
	}
	r := &host.Command{Exec: env.Exec, Cmd: c}
	for _, g := range []struct {
		key, path string
		dst       *string
	}{{"creates", a.Creates, &r.Creates}, {"removes", a.Removes, &r.Removes}} {
		if g.path == "" {
			continue
		}
		if err := absPath(g.key, g.path); err != nil {
			return nil, err
		}
		*g.dst = env.Path(g.path)
	}
	if a.Unless != nil {
		u, err := a.Unless.cmd("unless")
		if err != nil {
			return nil, err
		}
		u.Dir, u.Env = c.Dir, c.Env
		r.Unless = &u
	}
	for _, p := range a.Watch {
		if err := absPath("watch", p); err != nil {
			return nil, err
		}
		r.Watch = append(r.Watch, env.Path(p))
	}
	if a.ChangesPackages {
		if env.Apt == nil {
			return nil, errors.New("changes_packages: the run has no Apt")
		}
		r.Apt = env.Apt
	}
	return r, nil
}

type composeArgs struct {
	// ProjectSrc is the project directory; the commands run in it.
	ProjectSrc string `yaml:"project_src"`
	// Files are the compose files, relative to project_src; unset is
	// compose's default.
	Files List `yaml:"files"`
	// ProjectName is the project's name; unset is compose's default.
	ProjectName string `yaml:"project_name"`
	// State is present (the default: up) or absent (down).
	State string `yaml:"state"`
}

func newCompose(env module.Env, a composeArgs) (host.Resource, error) {
	if err := absPath("project_src", a.ProjectSrc); err != nil {
		return nil, err
	}
	state, err := choice("state", a.State, "present", "absent")
	if err != nil {
		return nil, err
	}
	return &host.Compose{Exec: env.Exec, Dir: a.ProjectSrc, Files: a.Files, Project: a.ProjectName, Absent: state == "absent"}, nil
}

type swarmArgs struct {
	// State is init (the default) or join.
	State         string `yaml:"state"`
	AdvertiseAddr string `yaml:"advertise_addr"`
	ListenAddr    string `yaml:"listen_addr"`
	// RemoteAddrs are managers of the swarm to join (host:port).
	RemoteAddrs List `yaml:"remote_addrs"`
	// JoinToken is the swarm's worker or manager token.
	JoinToken string `yaml:"join_token"`
}

func newSwarm(env module.Env, a swarmArgs) (host.Resource, error) {
	state, err := choice("state", a.State, "init", "join")
	if err != nil {
		return nil, err
	}
	sw := &host.Swarm{Exec: env.Exec, Join: state == "join", AdvertiseAddr: a.AdvertiseAddr, ListenAddr: a.ListenAddr,
		RemoteAddrs: a.RemoteAddrs, JoinToken: a.JoinToken}
	if err := sw.Validate(); err != nil {
		return nil, err
	}
	return sw, nil
}

type assertArgs struct {
	// Name is the check's line in the report; unset is "assert" and the
	// command.
	Name string `yaml:"name"`
	Cmd  Cmd    `yaml:"cmd"`
	// Regex is what the output must match. With min or max, its first
	// group is the number they bound (the whole output when it has none).
	Regex string `yaml:"regex"`
	// Min and Max bound the number: outside them, the check fails.
	Min *float64 `yaml:"min"`
	Max *float64 `yaml:"max"`
	// WarnMin and WarnMax bound it too: outside them (and inside min and
	// max), the check warns.
	WarnMin *float64 `yaml:"warn_min"`
	WarnMax *float64 `yaml:"warn_max"`
	// Warn: a check that fails warns instead.
	Warn bool `yaml:"warn"`
	// Timeout kills the command after this long; unset is 1m.
	Timeout time.Duration `yaml:"timeout"`
}

// check is an assert's test of a command's output: nil when it passes, a
// host.Warning when it warns, else why it fails.
type check struct {
	re                         *regexp.Regexp
	min, max, warnMin, warnMax *float64
}

func (c check) bounded() bool {
	return c.min != nil || c.max != nil || c.warnMin != nil || c.warnMax != nil
}

func (c check) test(out string) error {
	out = strings.TrimSpace(out)
	num, rest := out, ""
	if c.re != nil {
		loc := c.re.FindStringSubmatchIndex(out)
		if loc == nil {
			return fmt.Errorf("output %q does not match %s", host.LastLines(out, 1), c.re)
		}
		if len(loc) > 2 && loc[2] >= 0 {
			num = out[loc[2]:loc[3]]
		}
		// The rest of the matched line, outside the match, goes with a
		// finding: a check that prints a level and what it found.
		start := strings.LastIndexByte(out[:loc[0]], '\n') + 1
		end := len(out)
		if i := strings.IndexByte(out[loc[1]:], '\n'); i >= 0 {
			end = loc[1] + i
		}
		rest = strings.TrimSpace(out[start:loc[0]] + " " + out[loc[1]:end])
	}
	if !c.bounded() {
		return nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return fmt.Errorf("%q is not a number", num)
	}
	say := ""
	if rest != "" {
		say = ": " + rest
	}
	f := strconv.FormatFloat
	switch {
	case c.min != nil && n < *c.min:
		return fmt.Errorf("%s, below %s%s", f(n, 'g', -1, 64), f(*c.min, 'g', -1, 64), say)
	case c.max != nil && n > *c.max:
		return fmt.Errorf("%s, above %s%s", f(n, 'g', -1, 64), f(*c.max, 'g', -1, 64), say)
	case c.warnMin != nil && n < *c.warnMin:
		return host.Warning(fmt.Errorf("%s, below %s%s", f(n, 'g', -1, 64), f(*c.warnMin, 'g', -1, 64), say))
	case c.warnMax != nil && n > *c.warnMax:
		return host.Warning(fmt.Errorf("%s, above %s%s", f(n, 'g', -1, 64), f(*c.warnMax, 'g', -1, 64), say))
	}
	return nil
}

func newAssert(env module.Env, a assertArgs) (host.Resource, error) {
	c, err := a.Cmd.cmd("cmd")
	if err != nil {
		return nil, err
	}
	if c, err = (runArgs{Timeout: a.Timeout}).cmd(c, time.Minute); err != nil {
		return nil, err
	}
	ck := check{min: a.Min, max: a.Max, warnMin: a.WarnMin, warnMax: a.WarnMax}
	if a.Regex != "" {
		if ck.re, err = regexp.Compile(a.Regex); err != nil {
			return nil, fmt.Errorf("regex: %w", err)
		}
	}
	if (a.Min != nil && a.Max != nil && *a.Min > *a.Max) || (a.WarnMin != nil && a.WarnMax != nil && *a.WarnMin > *a.WarnMax) {
		return nil, errors.New("min above max")
	}
	name := a.Name
	if name == "" {
		name = "assert " + c.String()
	}
	return &host.Assert{Name: name, Warn: a.Warn, Holds: func(ctx context.Context) error {
		o := env.Exec.Run(ctx, c)
		if o.Err != nil {
			return o.Err
		}
		return ck.test(o.Out)
	}}, nil
}

type queryArgs struct {
	// Name is the task's resource ID; unset is "query" and the command.
	Name    string `yaml:"name"`
	Cmd     Cmd    `yaml:"cmd"`
	runArgs `yaml:",inline"`
}

// newQuery is a read-only command run in every mode, for register.
func newQuery(env module.Env, a queryArgs) (host.Resource, error) {
	c, err := a.Cmd.cmd("cmd")
	if err != nil {
		return nil, err
	}
	if c, err = a.runArgs.cmd(c, time.Minute); err != nil {
		return nil, err
	}
	return &host.Query{Name: a.Name, Exec: env.Exec, Cmd: c}, nil
}

type probeArgs struct {
	// Name is the task's resource ID; unset is "probe" and the test.
	Name string `yaml:"name"`
	// Test reads the state: in place when it exits 0 and, with Regex,
	// its output, trimmed of surrounding whitespace, matches.
	Test  Cmd    `yaml:"test"`
	Regex string `yaml:"regex"`
	// Drift is what is reported when not in place; unset is the test's
	// command line and "fails".
	Drift string `yaml:"drift"`
	// Fix are the commands that put it in place, run in order; the test
	// runs again after them, and still failing is an error. With no fix,
	// a failing test fails the task.
	Fix     []Cmd             `yaml:"fix"`
	Env     map[string]string `yaml:"env"`
	Timeout time.Duration     `yaml:"timeout"`
}

func newProbe(env module.Env, a probeArgs) (host.Resource, error) {
	t, err := a.Test.cmd("test")
	if err != nil {
		return nil, err
	}
	// The test is bounded at a minute; Timeout bounds each fix.
	if t, err = (runArgs{Env: a.Env}).cmd(t, time.Minute); err != nil {
		return nil, err
	}
	p := &host.Probe{Name: a.Name, Exec: env.Exec, Test: t, Drift: a.Drift}
	if a.Regex != "" {
		re, err := regexp.Compile(a.Regex)
		if err != nil {
			return nil, fmt.Errorf("regex: %w", err)
		}
		p.OK = func(o host.Output) bool { return o.OK() && re.MatchString(strings.TrimSpace(o.Out)) }
	}
	for i, f := range a.Fix {
		c, err := f.cmd(fmt.Sprintf("fix[%d]", i))
		if err != nil {
			return nil, err
		}
		if c, err = (runArgs{Env: a.Env, Timeout: a.Timeout}).cmd(c, 10*time.Minute); err != nil {
			return nil, err
		}
		p.Fix = append(p.Fix, c)
	}
	return p, nil
}

// envList is a command's added environment, sorted by name.
func envList(m map[string]string) ([]string, error) {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("env %q: not a variable name", k)
		}
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

type failArgs struct {
	// Msg is why the play stops.
	Msg string `yaml:"msg"`
}

// newFail is a task that always fails with msg: with when, a refusal (a
// required variable left empty, a host the playbook is not for).
func newFail(env module.Env, a failArgs) (host.Resource, error) {
	if err := required("msg", a.Msg); err != nil {
		return nil, err
	}
	return &host.Assert{Name: "fail", Holds: func(context.Context) error { return host.Refusal(errors.New(a.Msg)) }}, nil
}
