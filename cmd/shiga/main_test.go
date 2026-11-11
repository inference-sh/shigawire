package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
)

type fileArgs struct {
	Path    string `yaml:"path" doc:"the file's path"`
	Content string `yaml:"content"`
}

type failing struct{}

func (failing) ID() string                                { return "failing" }
func (failing) Check(context.Context) (host.State, error) { return host.State{}, errors.New("broken") }
func (failing) Apply(context.Context, host.State) (host.Result, error) {
	return host.Result{}, nil
}

// testRegistry is the standard modules and two test ones.
func testRegistry() *module.Registry {
	r := builtin.Registry()
	r.Register("test.file", module.Typed(func(env module.Env, a fileArgs) (host.Resource, error) {
		return &host.File{Path: env.Path(a.Path), Content: []byte(a.Content), Parents: true}, nil
	}))
	r.Register("test.fail", func(module.Env, map[string]any) (host.Resource, error) { return failing{}, nil })
	return r
}

type harness struct {
	h      *hosttest.Host
	c      *cli
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	h := hosttest.New(t)
	var out, errOut bytes.Buffer
	return &harness{h: h, stdout: &out, stderr: &errOut, c: &cli{
		stdout: &out, stderr: &errOut,
		environ: []string{"SHIGA_VAR_WHO=env"},
		env:     module.Env{Exec: h.Exec, Root: h.Root},
		reg:     testRegistry(),
	}}
}

func (x *harness) run(args ...string) int {
	x.stdout.Reset()
	x.stderr.Reset()
	return x.c.main(context.Background(), args)
}

func writePlaybook(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "site.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const site = `
name: site
steps:
  - name: motd
    tags: [base]
    tasks:
      - name: motd
        test.file: {path: /etc/motd, content: "hi ${WHO}"}
  - name: issue
    tasks:
      - name: issue
        test.file: {path: /etc/issue, content: x}
        when: exists /etc/want-issue
`

func TestPlanApplyVerify(t *testing.T) {
	x := newHarness(t)
	p := writePlaybook(t, site)

	if code := x.run("verify", p); code != exitChanged {
		t.Fatalf("verify before apply: %d\n%s%s", code, x.stdout, x.stderr)
	}
	if code := x.run("plan", p, "--detailed-exitcode"); code != exitChanged {
		t.Fatalf("plan: %d\n%s%s", code, x.stdout, x.stderr)
	}
	want := "changed  motd: motd: create\nskipped  issue: issue: when exists /etc/want-issue\nplan: 1 changed, 1 skipped\n"
	if x.stdout.String() != want {
		t.Fatalf("got\n%swant\n%s", x.stdout, want)
	}
	if code := x.run("plan", p); code != exitOK {
		t.Fatal(code)
	}
	if x.h.Exists("/etc/motd") {
		t.Fatal("plan wrote")
	}
	if code := x.run("apply", "--var", "WHO=flag", p, "--quiet"); code != exitOK {
		t.Fatalf("apply: %d\n%s%s", code, x.stdout, x.stderr)
	}
	if x.stdout.String() != "changed  motd: motd: create\napply: 1 changed, 1 skipped\n" {
		t.Fatal(x.stdout)
	}
	if x.h.Read("/etc/motd") != "hi flag" {
		t.Fatal(x.h.Read("/etc/motd"))
	}
	// The environment's WHO differs from the flag's: drift.
	if code := x.run("verify", p); code != exitChanged {
		t.Fatal(code, x.stdout)
	}
	if code := x.run("verify", p, "--var=WHO=flag"); code != exitOK {
		t.Fatalf("verify: %d\n%s", code, x.stdout)
	}
	if code := x.run("apply", p, "--var=WHO=flag", "--detailed-exitcode", "--only", "base"); code != exitOK {
		t.Fatalf("apply again: %d\n%s", code, x.stdout)
	}
	if x.stdout.String() != "ok       motd: motd\napply: 1 ok\n" {
		t.Fatal(x.stdout)
	}
}

func TestFailures(t *testing.T) {
	x := newHarness(t)
	bad := writePlaybook(t, "steps:\n  - name: s\n    tasks:\n      - test.fail:\n      - test.file: {path: /x, content: y}\n")
	if code := x.run("apply", bad); code != exitFailed {
		t.Fatal(code)
	}
	if x.stdout.String() != "failed   s: failing: broken\napply: 1 failed\n" || x.stderr.Len() != 0 {
		t.Fatalf("%q %q", x.stdout, x.stderr)
	}
	if code := x.run("verify", bad); code != exitFailed || !strings.Contains(x.stdout.String(), "unknown  s: failing: broken") {
		t.Fatal(code, x.stdout)
	}
	// An unknown with no change planned before it: apply reads the same
	// host there, and fails.
	if code := x.run("plan", bad); code != exitFailed || !strings.HasSuffix(x.stdout.String(), "1 unknown that no earlier change can explain: apply stops at the first\n") {
		t.Fatal(code, x.stdout)
	}
	// After a change it may depend on: plan cannot tell, exits 0, and says
	// so.
	after := writePlaybook(t, "steps:\n  - name: s\n    tasks:\n      - test.file: {path: /x, content: y}\n      - test.fail:\n")
	if code := x.run("plan", after); code != exitOK || !strings.HasSuffix(x.stdout.String(),
		"plan: 1 changed, 1 unknown\n1 unknown: checks that may depend on changes planned before them, which apply makes first\n") {
		t.Fatal(code, x.stdout)
	}
	// A fail task the plan reaches is a refusal apply stops at.
	refused := writePlaybook(t, "steps:\n  - name: s\n    tasks:\n      - test.file: {path: /x, content: y}\n      - fail: {msg: not for this host}\n")
	for _, args := range [][]string{{"plan", refused}, {"plan", refused, "--detailed-exitcode"}} {
		if code := x.run(args...); code != exitFailed || !strings.Contains(x.stdout.String(), "unknown  s: fail: not for this host") {
			t.Fatal(args, code, x.stdout)
		}
	}
	for _, args := range [][]string{
		{},
		{"nope"},
		{"apply"},
		{"apply", "a.yml", "b.yml"},
		{"apply", bad, "--var", "novalue"},
		{"apply", bad, "--bogus"},
		{"apply", filepath.Join(t.TempDir(), "missing.yml")},
		{"apply", bad, "--only", "zzz"},
	} {
		if code := x.run(args...); code != exitFailed || x.stderr.Len() == 0 {
			t.Errorf("%v: %d %q", args, code, x.stderr)
		}
	}
}

func TestVersionAndModules(t *testing.T) {
	x := newHarness(t)
	if code := x.run("version"); code != exitOK || !strings.HasPrefix(x.stdout.String(), "shiga ") {
		t.Fatal(code, x.stdout)
	}
	if code := x.run("modules"); code != exitOK {
		t.Fatal(code)
	}
	out := x.stdout.String()
	for _, want := range []string{"test.fail\n  (arguments not described)\n", "test.file\n", "  path     string  the file's path\n", "  content  string"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestParseInterspersed(t *testing.T) {
	x := newHarness(t)
	p := writePlaybook(t, site)
	if code := x.run("plan", "--quiet", p, "--", "--only"); code != exitFailed || !strings.Contains(x.stderr.String(), "want one playbook, got 2") {
		t.Fatal(code, x.stderr)
	}
}

// writeDir writes files (name to content) under a new directory and
// returns the path of its site.yml.
func writeDir(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "site.yml")
}

const goodSite = `
name: good
vars:
  PORT: 22
steps:
  - name: ssh
    tasks:
      - name: port
        file: {path: /etc/ssh/sshd_config.d/port.conf, content: "Port ${PORT}\n", owner: root}
        notify: reload ssh
      - name: motd
        copy: {dest: /etc/motd, src: motd}
      - apt: {name: [openssh-server]}
    handlers:
      - name: reload ssh
        restart: {name: ssh}
  - name: later
    manual: true
    tasks:
      - user: {name: deploy, shell: /bin/bash}
`

func TestCheckGood(t *testing.T) {
	x := newHarness(t)
	p := writeDir(t, map[string]string{"site.yml": goodSite, "files/motd": "hi\n"})
	if code := x.run("check", p); code != exitOK {
		t.Fatalf("%d\n%s%s", code, x.stdout, x.stderr)
	}
	if x.stdout.String() != "ok: 2 steps, 4 tasks\n" {
		t.Fatalf("%q", x.stdout)
	}
	// Nothing ran and nothing was written.
	if calls := x.h.Exec.Calls(); len(calls) != 0 {
		t.Fatal(calls)
	}
	if x.h.Exists("/etc/motd") {
		t.Fatal("check wrote")
	}
}

func TestCheckProblems(t *testing.T) {
	const head = "steps:\n  - name: s\n    tasks:\n"
	for _, c := range []struct {
		name, yml string
		want      string
		files     bool // the playbook has a files/ directory
	}{
		{"yaml", head + "      - [file\n", "site.yml:3: did not find expected ',' or ']'", false},
		{"structure", "steps:\n  - name: s\n    taks: []\n", `site.yml:3:5: unknown key "taks"`, false},
		{"unknown module", head + "      - fiel: {path: /x, content: x}\n", `site.yml:4:9: unknown module "fiel"`, false},
		{"unknown module, when false", "vars: {ON: no}\n" + head + "      - fiel: {}\n        when: ${ON} == yes\n", `site.yml:5:9: unknown module "fiel"`, false},
		{"argument", head + "      - file: {path: /x, contnet: x}\n", `site.yml:4:9: file: unknown key "contnet"`, false},
		{"argument type", head + "      - user: {name: bob, shell: [7]}\n", "site.yml:4:9: user: shell: want a string, got a list", false},
		{"argument value", head + "      - file: {path: x, content: x}\n", `site.yml:4:9: file: path "x": want an absolute path`, false},
		{"undefined", head + "      - file: {path: /x, content: \"${NOPE}\"}\n", "site.yml:4:35: undefined variable ${NOPE}", false},
		{"undefined in when", head + "      - file: {path: /x, content: x}\n        when: ${NOPE} == x\n", "site.yml:5:15: when: undefined variable ${NOPE}", false},
		{"when", head + "      - file: {path: /x, content: x}\n        when: os.flavor == x\n", `site.yml:5:15: when: unknown fact "os.flavor"`, false},
		{"when register", head + "      - file: {path: /x, content: x}\n        when: r.rc == 0\n", `site.yml:5:15: when: "r.rc": no task before this one registers "r"`, false},
		{"notify", head + "      - file: {path: /x, content: x}\n        notify: reload\n", `site.yml:5:17: notify "reload": no handler of that name`, false},
		{"needs", head + "      - file: {path: /x, content: x}\n        notify: a\n    handlers:\n      - {name: a, needs: b, reload: {name: x}}\n", `needs "b": no handler of that name`, false},
		{"src", head + "      - copy: {dest: /x, src: nothere.conf}\n", "site.yml:4:9: copy: src: open nothere.conf", true},
		{"src, no files", head + "      - copy: {dest: /x, src: a/b.conf}\n", `site.yml:4:9: copy: src "a/b.conf": relative, and the playbook has no files/ directory`, false},
		{"manual step", head + "      - file: {path: /x, content: x}\n  - name: m\n    manual: true\n    tasks:\n      - fiel: {}\n", `site.yml:8:9: unknown module "fiel"`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newHarness(t)
			files := map[string]string{"site.yml": c.yml}
			if c.files {
				files["files/other"] = "x"
			}
			p := writeDir(t, files)
			code := x.run("check", p)
			out := x.stdout.String()
			if code != exitFailed || !strings.Contains(out, c.want) || !strings.HasSuffix(out, "check: 1 problem\n") {
				t.Fatalf("%d\n%s%s\nwant %q", code, out, x.stderr, c.want)
			}
			if !strings.HasPrefix(out, p+":") && !strings.HasPrefix(out, p) {
				t.Fatalf("not at the file: %q", out)
			}
		})
	}
}

func TestCheckManyProblems(t *testing.T) {
	x := newHarness(t)
	p := writeDir(t, map[string]string{"site.yml": `
steps:
  - name: s
    tasks:
      - fiel: {}
        notify: nope
      - file: {path: /x, contnet: x}
      - file: {path: /y, content: "${A}"}
        when: bogus
`})
	if code := x.run("check", p); code != exitFailed {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(x.stdout.String()), "\n")
	want := []string{":5:9: unknown module", ":6:17: notify \"nope\"", ":7:9: file: unknown key", ":8:35: undefined variable ${A}", ":9:15: when: \"bogus\"", "check: 5 problems"}
	if len(lines) != len(want) {
		t.Fatalf("%d lines\n%s", len(lines), x.stdout)
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d %q, want %q", i, lines[i], w)
		}
	}
}

func TestCheckVariables(t *testing.T) {
	x := newHarness(t)
	yml := "steps:\n  - name: s\n    tasks:\n      - file: {path: \"/etc/${NAME}\", content: x}\n        when: ${ON} == yes\n"
	p := writeDir(t, map[string]string{"site.yml": yml, "vars.yml": "ON: yes\n"})
	if code := x.run("check", p); code != exitFailed || strings.Count(x.stdout.String(), "undefined variable") != 2 {
		t.Fatalf("%d %s", code, x.stdout)
	}
	if code := x.run("check", p, "--allow-undefined"); code != exitOK {
		t.Fatalf("%d %s", code, x.stdout)
	}
	if code := x.run("check", p, "--var", "NAME=motd", "--vars", filepath.Join(filepath.Dir(p), "vars.yml")); code != exitOK {
		t.Fatalf("%d %s", code, x.stdout)
	}
	// The variable makes the path relative: the module's check still runs.
	if code := x.run("check", p, "--var", "NAME=", "--var", "ON=yes"); code != exitOK {
		t.Fatalf("%d %s", code, x.stdout)
	}
	if code := x.run("check", p, "--allow-undefined", "--only", "s"); code != exitFailed || x.stderr.Len() == 0 {
		t.Fatalf("--only is not a check flag: %d %s", code, x.stderr)
	}
}

func TestTestCommand(t *testing.T) {
	x := newHarness(t)
	p := writeDir(t, map[string]string{"site.yml": goodSite, "files/motd": "hi\n"})
	if code := x.run("test", p, "--only", "ssh,later"); code != exitOK {
		t.Fatalf("%d\n%s%s", code, x.stdout, x.stderr)
	}
	out := x.stdout.String()
	for _, want := range []string{"simulated host:", "any other command succeeds with no output", "apply: ", "apply: 5 changed\n", "apply again: 4 ok\n", "verify: 4 ok\n", "ok: the playbook converges on the simulated host\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	// It ran on the simulation, not through the harness's host.
	if calls := x.h.Exec.Calls(); len(calls) != 0 || x.h.Exists("/etc/motd") {
		t.Fatal("ran on the host", calls)
	}

	loop := writePlaybook(t, "steps:\n  - name: s\n    tasks:\n      - command: {cmd: make, creates: /opt/built}\n      - test.file: {path: /etc/x, content: x}\n")
	if code := x.run("test", loop); code != exitFailed {
		t.Fatal(code, x.stdout)
	}
	out = x.stdout.String()
	if !strings.Contains(out, "changed  s: command /bin/sh -c make") || strings.Contains(out, "s: test.file") ||
		!strings.Contains(out, "FAIL: apply again changed 1: the playbook does not converge") {
		t.Fatal(out)
	}

	broken := writePlaybook(t, "steps:\n  - name: s\n    tasks:\n      - test.fail:\n")
	if code := x.run("test", broken); code != exitFailed || !strings.Contains(x.stdout.String(), "failed   s: failing: broken\nFAIL: apply: ") {
		t.Fatal(code, x.stdout)
	}
	if code := x.run("test", writePlaybook(t, "steps: [\n")); code != exitFailed || x.stderr.Len() == 0 {
		t.Fatal(code, x.stderr)
	}
}
