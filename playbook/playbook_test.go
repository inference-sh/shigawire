package playbook_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/playbook"
)

// Stand-in modules: the real ones live elsewhere.

type fileArgs struct {
	Path    string      `yaml:"path"`
	Content string      `yaml:"content"`
	Src     string      `yaml:"src"`
	Mode    fs.FileMode `yaml:"mode"`
}

type cmdArgs struct {
	Cmd     []string `yaml:"cmd"`
	Creates string   `yaml:"creates"`
	Unless  []string `yaml:"unless"`
}

type failing struct{}

func (failing) ID() string                                             { return "failing" }
func (failing) Check(context.Context) (host.State, error)              { return host.State{}, errors.New("broken") }
func (failing) Apply(context.Context, host.State) (host.Result, error) { panic("applied") }

// reg holds the stand-in modules.
var reg = module.NewRegistry()

func init() {
	reg.Register("test.file", module.Typed(func(env module.Env, a fileArgs) (host.Resource, error) {
		f := &host.File{Path: env.Path(a.Path), Content: []byte(a.Content), Mode: a.Mode, Parents: true}
		if a.Src != "" {
			if env.Files == nil {
				return nil, errors.New("no files directory")
			}
			b, err := fs.ReadFile(env.Files, a.Src)
			if err != nil {
				return nil, err
			}
			f.Content = b
		}
		return f, nil
	}))
	reg.Register("test.cmd", module.Typed(func(env module.Env, a cmdArgs) (host.Resource, error) {
		if len(a.Cmd) == 0 {
			return nil, errors.New("cmd is empty")
		}
		c := &host.Command{Exec: env.Exec, Cmd: host.Cmd{Name: a.Cmd[0], Args: a.Cmd[1:]}}
		if a.Creates != "" {
			c.Creates = env.Path(a.Creates)
		}
		if len(a.Unless) > 0 {
			c.Unless = &host.Cmd{Name: a.Unless[0], Args: a.Unless[1:]}
		}
		return c, nil
	}))
	reg.Register("test.fail", func(module.Env, map[string]any) (host.Resource, error) { return failing{}, nil })
}

// load is playbook.Load with the stand-in modules when opt names no
// registry.
func load(path string, opt playbook.Options) (*playbook.Playbook, error) {
	if opt.Registry == nil {
		opt.Registry = reg
	}
	return playbook.Load(path, opt)
}

func TestLoadNeedsRegistry(t *testing.T) {
	path := write(t, t.TempDir(), "p.yml", "steps: []\n")
	if _, err := playbook.Load(path, playbook.Options{}); err == nil || !strings.Contains(err.Error(), "Registry is nil") {
		t.Fatalf("err %v", err)
	}
}

func TestLoadUsesOnlyItsRegistry(t *testing.T) {
	path := write(t, t.TempDir(), "p.yml", "steps:\n  - name: s\n    tasks:\n      - test.file: {path: /x, content: x}\n")
	if _, err := playbook.Load(path, playbook.Options{Registry: module.NewRegistry()}); err == nil || !strings.Contains(err.Error(), `unknown module "test.file"`) {
		t.Fatalf("err %v", err)
	}
}

// write writes content to dir/name and returns its path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(h *hosttest.Host) module.Env { return module.NewEnv(h.Exec, h.Root) }

// run loads the playbook in content and runs it with m.
func run(t *testing.T, h *hosttest.Host, content string, opt playbook.Options, m func(*playbook.Playbook, context.Context, *host.Runner) error) ([]host.Result, error) {
	t.Helper()
	opt.Env = env(h)
	pb, err := load(write(t, t.TempDir(), "site.yml", content), opt)
	if err != nil {
		t.Fatal(err)
	}
	var got []host.Result
	err = m(pb, context.Background(), &host.Runner{OnResult: func(r host.Result) { got = append(got, r) }})
	return got, err
}

var (
	apply  = (*playbook.Playbook).Apply
	plan   = (*playbook.Playbook).Plan
	verify = (*playbook.Playbook).Verify
)

func TestVarsPrecedence(t *testing.T) {
	h := hosttest.New(t)
	dir := t.TempDir()
	vf1 := write(t, dir, "a.yml", "B: file1\nC: file1\n")
	vf2 := write(t, dir, "b.yml", "C: file2\n")
	pb := `
name: vars
vars:
  A: default
  B: default
  C: default
  D: default
  E: default
steps:
  - name: s
    tasks:
      - test.file: {path: /out, content: "${A} ${B} ${C} ${D} ${E} $${A}"}
`
	_, err := run(t, h, pb, playbook.Options{
		Environ:  []string{"SHIGA_VAR_B=env", "SHIGA_VAR_D=env", "E=plain-env", "SHIGA_VAR_9=x"},
		VarFiles: []string{vf1, vf2},
		Set:      map[string]string{"D": "flag"},
	}, apply)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Read("/out"); got != "default file1 file2 flag default ${A}" {
		t.Fatal(got)
	}
}

func TestTypedSubstitution(t *testing.T) {
	h := hosttest.New(t)
	pb := `
vars: {MODE: "0600", TEXT: "007"}
steps:
  - name: s
    tasks:
      - test.file: {path: /m, mode: "${MODE}", content: "${TEXT}"}
`
	if _, err := run(t, h, pb, playbook.Options{}, apply); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(h.Path("/m"))
	if fi.Mode().Perm() != 0o600 || h.Read("/m") != "007" {
		t.Fatal(fi.Mode(), h.Read("/m"))
	}
	// Unquoted, a fraction goes where the module wants a string: an error,
	// as if the fraction were written there (a whole number is its text).
	_, err := load(write(t, t.TempDir(), "p.yml", "vars: {N: 7.5}\nsteps:\n  - name: s\n    tasks:\n      - test.file:\n          path: /m\n          content: ${N}\n"), playbook.Options{Env: env(h)})
	if err == nil || !strings.Contains(err.Error(), "p.yml:5:9: test.file: content: want a string, got a number") {
		t.Fatal(err)
	}
}

func TestLoadErrors(t *testing.T) {
	h := hosttest.New(t)
	for _, c := range []struct{ yml, want string }{
		{"steps:\n  - name: s\n    tasks:\n      - test.file:\n          content: a ${NOPE}\n", "p.yml:5:20: undefined variable ${NOPE}"},
		{"steps:\n  - name: s\n    tasks:\n      - nope: {}\n", `p.yml:4:9: unknown module "nope"`},
		{"steps:\n  - name: s\n    tasks:\n      - test.file: {path: /x, contnet: y}\n", `p.yml:4:9: test.file: unknown key "contnet"`},
		{"steps:\n  - name: s\n    tasks:\n      - test.file: {path: /x}\n        test.cmd: {cmd: [x]}\n", `p.yml:5:9: "test.cmd": a task has one module, and it is "test.file"`},
		{"steps:\n  - name: s\n    tasks:\n      - name: x\n", "p.yml:4:9: the task names no module"},
		{"steps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        notify: nope\n", `p.yml:5:17: notify "nope": no handler of that name`},
		{"steps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        when: r.rc == 0\n", `p.yml:5:15: when: "r.rc": no task before this one registers "r"`},
		{"steps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        when: os.kernel == 6\n", `unknown fact "os.kernel"`},
		{"steps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        needs: r\n        register: r\n", `p.yml:5:16: needs "r": no task before this one registers it`},
		{"vars: {X: 1}\nsteps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        when: ${X} >= 2\n", `p.yml:6:15: when: ">=": want ==, != or contains`},
		{"vars: {ENV: prod}\nsteps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        when: ${ENV}==dev\n", `put spaces around ==`},
		{"vars: {ENV: prod}\nsteps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        when: not ${ENV}!=dev\n", `put spaces around !=`},
		{"steps:\n  - name: s\n  - name: s\n", `p.yml:3:5: step "s": a step of that name is at line 2`},
		{"steps:\n  - tasks: []\n", "p.yml:2:5: a step needs a name"},
		{"stepz: []\n", `p.yml:1:1: unknown key "stepz"`},
		{"steps:\n  - name: s\n    task: []\n", `p.yml:3:5: unknown key "task"`},
		{"steps:\n  - name: s\n    tasks: [\n", "p.yml:3: did not find expected node content"},
		{"steps:\n  - name: s\n    handlers:\n      - name: a\n        needs: b\n        test.cmd: {cmd: [x]}\n      - name: b\n        needs: a\n        test.cmd: {cmd: [x]}\n    tasks:\n      - test.cmd: {cmd: [x]}\n        notify: a\n",
			`handler "a" needs itself`},
		{"steps:\n  - name: s\n    tasks:\n      - test.cmd: {cmd: [x]}\n        register: r\n      - test.cmd: {cmd: [x]}\n        register: r\n", `p.yml:7:19: register "r": an earlier task registers that name`},
	} {
		_, err := load(write(t, t.TempDir(), "p.yml", c.yml), playbook.Options{Env: env(h)})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s\nerr  %v\nwant %s", c.yml, err, c.want)
		}
	}
}

func TestWhen(t *testing.T) {
	h := hosttest.New(t)
	h.Write("/etc/os-release", "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n")
	h.Write("/etc/marker", "")
	h.Exec.On("uname -m", hosttest.Out("x86_64\n"))
	h.Exec.On("systemctl is-active ufw", host.Output{Code: 3, Out: "inactive\n"})
	h.Exec.On("echo hi", hosttest.Out("hello there\n"))
	pb := `
vars: {ON: "yes", OFF: "no"}
steps:
  - name: s
    tasks:
      - {name: on, test.cmd: {cmd: ["true", on]}, when: "${ON}"}
      - {name: off, test.cmd: {cmd: ["true", off]}, when: "${OFF}"}
      - {name: not off, test.cmd: {cmd: ["true", notoff]}, when: "not ${OFF}"}
      - {name: ubuntu 24, test.cmd: {cmd: ["true", ubuntu]}, when: [os.id == ubuntu, os.version_id == "24.04", os.arch == amd64]}
      - {name: debian, test.cmd: {cmd: ["true", debian]}, when: os.id != ubuntu}
      - {name: marker, test.cmd: {cmd: ["true", marker]}, when: exists /etc/marker}
      - {name: no marker, test.cmd: {cmd: ["true", nomarker]}, when: not exists /etc/marker}
      - {name: ufw, test.cmd: {cmd: [systemctl, is-active, ufw]}, register: ufw, ignore_errors: true}
      - {name: hi, test.cmd: {cmd: [echo, hi]}, register: hi}
      - {name: ufw off, test.cmd: {cmd: ["true", ufwoff]}, when: [ufw.rc != 0, ufw.failed, not ufw.ok, hi.stdout contains there, hi.stdout == hello there, hi.changed]}
      - {name: ufw on, test.cmd: {cmd: ["true", ufwon]}, when: ufw.rc == 0}
`
	got, err := run(t, h, pb, playbook.Options{}, apply)
	if err != nil {
		t.Fatal(err)
	}
	want := "s: on=changed s: off=skipped s: not off=changed s: ubuntu 24=changed s: debian=skipped s: marker=changed s: no marker=skipped " +
		"s: ufw=ignored s: hi=changed s: ufw off=changed s: ufw on=skipped"
	if s := hosttest.Statuses(got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if got[1].Msg != "when ${OFF}" {
		t.Fatal(got[1].Msg)
	}
}

func TestStepWhenAndRegisterInPlan(t *testing.T) {
	h := hosttest.New(t)
	pb := `
steps:
  - name: probe
    tasks:
      - {test.cmd: {cmd: [docker, info]}, register: docker, ignore_errors: true}
  - name: docker
    when: docker.rc == 0
    tasks:
      - {name: compose, test.file: {path: /etc/compose, content: x}}
`
	got, err := run(t, h, pb, playbook.Options{}, apply)
	if err != nil || hosttest.Statuses(got) != "probe: command docker info=changed docker: compose=changed" {
		t.Fatal(hosttest.Statuses(got), err)
	}
	// In a plan the probe does not run, so the step it decides is unknown.
	got, err = run(t, hosttest.New(t), pb, playbook.Options{}, plan)
	if err != nil || hosttest.Statuses(got) != "probe: command docker info=changed docker: compose=unknown" {
		t.Fatal(hosttest.Statuses(got), err)
	}
	if !strings.Contains(got[1].Msg, "when docker.rc == 0: docker ran no command") {
		t.Fatal(got[1].Msg)
	}
	// Not selected, the probe has not run.
	got, _ = run(t, hosttest.New(t), pb, playbook.Options{Only: []string{"docker"}}, apply)
	if hosttest.Statuses(got) != "docker: compose=failed" || !strings.Contains(got[0].Msg, "docker has not run") {
		t.Fatal(hosttest.Statuses(got), got[0].Msg)
	}
}

func TestHandlers(t *testing.T) {
	h := hosttest.New(t)
	pb := `
steps:
  - name: ssh
    tasks:
      - {name: a, test.file: {path: /a, content: a}, notify: reload}
      - {name: b, test.file: {path: /b, content: b}, notify: [reload]}
    handlers:
      - {name: validate, test.cmd: {cmd: [sshd, -t]}}
      - {name: reload, needs: validate, test.cmd: {cmd: [systemctl, reload, ssh]}}
  - name: next
    tasks:
      - {name: c, test.file: {path: /c, content: c}, notify: validate}
`
	got, err := run(t, h, pb, playbook.Options{}, apply)
	if err != nil {
		t.Fatal(err)
	}
	// reload runs once, at the end of its step; validate is notified only
	// by next, so reload does not run it.
	want := "ssh: a=changed ssh: b=changed reload=changed next: c=changed validate=changed"
	if s := hosttest.Statuses(got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	got, _ = run(t, h, pb, playbook.Options{}, apply)
	if s := hosttest.Statuses(got); s != "ssh: a=ok ssh: b=ok next: c=ok" {
		t.Fatal(s)
	}
}

func TestOnly(t *testing.T) {
	pb := `
steps:
  - {name: one, tags: [base], tasks: [{test.file: {path: /1, content: "${ONE}"}}]}
  - {name: two, tags: base, tasks: [{test.file: {path: /2, content: x}}]}
  - {name: three, tasks: [{test.file: {path: /3, content: x}}]}
`
	h := hosttest.New(t)
	opt := playbook.Options{Env: env(h), Only: []string{"three"}}
	path := write(t, t.TempDir(), "p.yml", pb)
	// An excluded step's variables need no value.
	p, err := load(path, opt)
	if err != nil || strings.Join(p.Steps, ",") != "three" {
		t.Fatal(err, p)
	}
	opt.Only, opt.Set = []string{"base"}, map[string]string{"ONE": "1"}
	if p, err = load(path, opt); err != nil || strings.Join(p.Steps, ",") != "one,two" {
		t.Fatal(err, p)
	}
	opt.Only = []string{"four"}
	if _, err = load(path, opt); err == nil || !strings.Contains(err.Error(), `no step named or tagged "four"`) {
		t.Fatal(err)
	}
}

func TestFilesAndModes(t *testing.T) {
	h := hosttest.New(t)
	dir := t.TempDir()
	write(t, dir, "files/motd", "welcome\n")
	path := write(t, dir, "site.yml", "name: f\nsteps:\n  - name: s\n    tasks:\n      - test.file: {path: /etc/motd, src: motd}\n      - test.fail:\n")
	pb, err := load(path, playbook.Options{Env: env(h)})
	if err != nil {
		t.Fatal(err)
	}
	if pb.Name != "f" {
		t.Fatal(pb.Name)
	}
	got, err := hosttest.Run(func(r *host.Runner, ctx context.Context, _ host.Play) error { return pb.Plan(ctx, r) }, host.Play{})
	if err != nil || hosttest.Statuses(got) != "s: file "+h.Path("/etc/motd")+"=changed s: failing=unknown" {
		t.Fatal(hosttest.Statuses(got), err)
	}
	if h.Exists("/etc/motd") {
		t.Fatal("plan wrote")
	}
	got, err = hosttest.Run(func(r *host.Runner, ctx context.Context, _ host.Play) error { return pb.Apply(ctx, r) }, host.Play{})
	if err == nil || hosttest.Statuses(got) != "s: file "+h.Path("/etc/motd")+"=changed s: failing=failed" {
		t.Fatal(hosttest.Statuses(got), err)
	}
	if h.Read("/etc/motd") != "welcome\n" {
		t.Fatal(h.Read("/etc/motd"))
	}
	if err := pb.Verify(context.Background(), &host.Runner{}); err == nil {
		t.Fatal("verify passed a failing check")
	}
}

func TestWhenMode(t *testing.T) {
	pb := `
steps:
  - name: setup
    tasks:
      - {name: file, test.file: {path: /etc/f, content: x}}
  - name: health
    when: mode == verify
    tasks:
      - {name: check, test.cmd: {cmd: [check], unless: [check]}}
`
	for m, want := range map[string]string{
		"apply":  "setup: file=changed health: check=skipped",
		"plan":   "setup: file=changed health: check=skipped",
		"verify": "setup: file=changed health: check=ok",
	} {
		f := map[string]func(*playbook.Playbook, context.Context, *host.Runner) error{"apply": apply, "plan": plan, "verify": verify}[m]
		got, _ := run(t, hosttest.New(t), pb, playbook.Options{}, f)
		if s := hosttest.Statuses(got); s != want {
			t.Errorf("%s: got %s, want %s", m, s, want)
		}
	}
}

func TestEnvVarsFile(t *testing.T) {
	h := hosttest.New(t)
	dir := t.TempDir()
	envFile := write(t, dir, "domains.env", "# domains\nexport A=one\nB=\"two words\"\n\nC='${NOT_EXPANDED}'\nUNUSED=x\n")
	pb := `
vars: {A: a, B: b, C: c}
steps:
  - name: s
    tasks:
      - test.file: {path: /out, content: "${A}|${B}|${C}"}
`
	if _, err := run(t, h, pb, playbook.Options{VarFiles: []string{envFile}}, apply); err != nil {
		t.Fatal(err)
	}
	if got := h.Read("/out"); got != "one|two words|${NOT_EXPANDED}" {
		t.Fatal(got)
	}
	bad := write(t, dir, "bad.env", "A=1\nnot a pair\n")
	if _, err := load(write(t, dir, "p.yml", pb), playbook.Options{Env: env(h), VarFiles: []string{bad}}); err == nil || !strings.Contains(err.Error(), "bad.env:2: want NAME=VALUE") {
		t.Fatal(err)
	}
}

func TestManualStep(t *testing.T) {
	pb := `
steps:
  - {name: one, tasks: [{test.file: {path: /1, content: x}}]}
  - {name: risky, manual: true, tags: [firewall], tasks: [{test.file: {path: /2, content: "${KEY}"}}]}
`
	h := hosttest.New(t)
	path := write(t, t.TempDir(), "p.yml", pb)
	// Left out of a full run: its variables need no value.
	p, err := load(path, playbook.Options{Env: env(h)})
	if err != nil || strings.Join(p.Steps, ",") != "one" {
		t.Fatal(err, p)
	}
	for _, only := range []string{"risky", "firewall"} {
		p, err := load(path, playbook.Options{Env: env(h), Only: []string{only}, Set: map[string]string{"KEY": "k"}})
		if err != nil || strings.Join(p.Steps, ",") != "risky" {
			t.Fatal(only, err, p)
		}
	}
	if _, err := load(write(t, t.TempDir(), "p.yml", "steps: [{name: a, manual: yes, tasks: []}]"), playbook.Options{Env: env(h)}); err == nil || !strings.Contains(err.Error(), "manual: want true or false") {
		t.Fatal(err)
	}
}

// A task whose when is false on variables alone is not built: arguments
// that hold variables left empty for it are not checked.
func TestConstantFalseNotBuilt(t *testing.T) {
	h := hosttest.New(t)
	pb := `
vars: {SRC: "", ON: ""}
steps:
  - name: s
    tasks:
      - {name: copy, test.file: {path: "${SRC}"}, when: "${SRC}"}
      - {name: on, test.cmd: {cmd: []}, when: ["exists /x", "${ON}"]}
      - {name: kept, test.file: {path: /k, content: x}, when: "not ${ON}"}
    handlers:
      - {name: h, test.cmd: {cmd: []}, when: "${ON}"}
`
	got, err := run(t, h, pb, playbook.Options{}, apply)
	if err != nil || hosttest.Statuses(got) != "s: copy=skipped s: on=skipped s: kept=changed" || got[0].Msg != "when ${SRC}" {
		t.Fatal(hosttest.Statuses(got), got[0].Msg, err)
	}
	// Set, the arguments are checked.
	if _, err := load(write(t, t.TempDir(), "p.yml", pb), playbook.Options{Env: env(h), Set: map[string]string{"ON": "yes"}}); err == nil || !strings.Contains(err.Error(), "cmd is empty") {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "p.yml", `
steps:
  - name: s
    tasks:
      - test.file: {path: /a, content: x}
        notify: [h, h2]
      - test.file: {path: /b, content: x}
        notify: h
    handlers:
      - {name: h, test.cmd: {cmd: []}}
      - {name: h2, test.cmd: {cmd: [x]}, needs: h}
`)
	rep := playbook.Check(path, playbook.Options{Registry: reg})
	// The handler three paths reach is one problem.
	if rep.Steps != 1 || rep.Tasks != 2 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Error(), "p.yml:10:19: test.cmd: cmd is empty") {
		t.Fatalf("%+v", rep)
	}
	if rep := playbook.Check(path, playbook.Options{}); len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Error(), "Registry is nil") {
		t.Fatalf("%+v", rep)
	}
	// Load stops at the first problem, as before.
	if _, err := load(path, playbook.Options{Env: env(hosttest.New(t))}); err == nil || !strings.Contains(err.Error(), "cmd is empty") {
		t.Fatal(err)
	}
}
