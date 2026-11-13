package playbook_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/playbook"
)

// includeTree writes a playbook tree under a new directory:
//
//	site.yml          includes lib/core.yml and lib/extra.yml
//	lib/core.yml      includes base.yml
//	lib/base.yml
//	lib/extra.yml     includes base.yml again (loaded once)
//	lib/files/motd    core.yml's files
func includeTree(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	hosttest.WriteFile(t, dir, "lib/files/motd", "from lib\n")
	hosttest.WriteFile(t, dir, "lib/base.yml", `
name: base
vars: {A: base, B: base, C: base}
steps:
  - name: base
    tasks:
      - test.file: {path: /base, content: "${A} ${B} ${C}"}
`)
	hosttest.WriteFile(t, dir, "lib/core.yml", `
name: core
include: base.yml
vars: {B: core}
steps:
  - name: core
    tasks:
      - test.file: {path: /motd, src: motd}
        notify: core done
    handlers:
      - name: core done
        test.file: {path: /core-done, content: x}
`)
	hosttest.WriteFile(t, dir, "lib/extra.yml", `
include: [./base.yml]
steps:
  - name: extra
    tasks:
      - test.file: {path: /extra, content: "${B}"}
`)
	hosttest.WriteFile(t, dir, "site.yml", `
name: site
include: [lib/core.yml, lib/extra.yml]
vars: {C: site}
steps:
  - name: own
    tasks:
      - test.file: {path: /own, content: "${A} ${B} ${C}"}
`)
	return dir
}

func TestInclude(t *testing.T) {
	dir := includeTree(t)
	h := hosttest.New(t)
	pb, err := load(filepath.Join(dir, "site.yml"), playbook.Options{Env: env(h)})
	if err != nil {
		t.Fatal(err)
	}
	// Included steps first, depth first in list order; base.yml, included
	// by core.yml and by extra.yml, loads once.
	if got := strings.Join(pb.Steps, ","); got != "base,core,extra,own" {
		t.Fatal(got)
	}
	if pb.Name != "site" {
		t.Fatal(pb.Name)
	}
	var files []string
	for _, f := range pb.Files {
		rel, _ := filepath.Rel(dir, f)
		files = append(files, rel)
	}
	if got := strings.Join(files, ","); got != "lib/base.yml,lib/core.yml,lib/extra.yml,site.yml" {
		t.Fatal(got)
	}
	// Vars merge: an includer's default wins over an included one's.
	if pb.Vars["A"] != "base" || pb.Vars["B"] != "core" || pb.Vars["C"] != "site" {
		t.Fatal(pb.Vars)
	}

	// Plan, apply and verify run the merged play.
	ctx := context.Background()
	got, err := hosttest.Results(ctx, func(ctx context.Context, r *host.Runner) error { return pb.Plan(ctx, r) })
	// The four files and core.yml's handler.
	if err != nil || len(got) != 5 || strings.Count(hosttest.Statuses(got), "=changed") != 5 || h.Exists("/own") {
		t.Fatalf("plan: %s %v", hosttest.Statuses(got), err)
	}
	got, err = hosttest.Results(ctx, func(ctx context.Context, r *host.Runner) error { return pb.Apply(ctx, r) })
	if err != nil {
		t.Fatalf("apply: %s %v", hosttest.Statuses(got), err)
	}
	for path, want := range map[string]string{
		"/base":      "base core site",
		"/motd":      "from lib\n", // core.yml's src, from lib/files
		"/core-done": "x",          // its handler
		"/extra":     "core",
		"/own":       "base core site",
	} {
		if got := h.Read(path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	got, err = hosttest.Results(ctx, func(ctx context.Context, r *host.Runner) error { return pb.Verify(ctx, r) })
	if err != nil || len(got) != 4 || strings.Contains(hosttest.Statuses(got), "changed") {
		t.Fatalf("verify: %s %v", hosttest.Statuses(got), err)
	}
	h.Write("/base", "drift")
	got, _ = hosttest.Results(ctx, func(ctx context.Context, r *host.Runner) error { return pb.Verify(ctx, r) })
	if !strings.Contains(hosttest.Statuses(got), "base: file "+h.Path("/base")+"=changed") {
		t.Fatalf("verify after drift: %s", hosttest.Statuses(got))
	}

	// --only selects included steps too.
	pb, err = load(filepath.Join(dir, "site.yml"), playbook.Options{Env: env(h), Only: []string{"core"}})
	if err != nil || strings.Join(pb.Steps, ",") != "core" {
		t.Fatal(err, pb)
	}
}

// A playbook with only an include is the included one under another name.
func TestIncludeOnly(t *testing.T) {
	dir := includeTree(t)
	path := hosttest.WriteFile(t, dir, "alias.yml", "include: lib/base.yml\n")
	pb, err := load(path, playbook.Options{Env: env(hosttest.New(t))})
	if err != nil || strings.Join(pb.Steps, ",") != "base" {
		t.Fatal(err, pb)
	}
}

func TestIncludeErrors(t *testing.T) {
	h := hosttest.New(t)
	step := func(name string) string {
		return "steps:\n  - name: " + name + "\n    tasks:\n      - test.file: {path: /" + name + ", content: x}\n"
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing", map[string]string{"p.yml": "include: [nope.yml]\n" + step("s")},
			"p.yml:1:10: include DIR/nope.yml: no such file or directory"},
		{"self", map[string]string{"p.yml": "include: p.yml\n" + step("s")},
			"p.yml:1:10: include DIR/p.yml: a cycle: DIR/p.yml -> DIR/p.yml"},
		{"cycle", map[string]string{
			"p.yml":     "include: sub/a.yml\n" + step("p"),
			"sub/a.yml": "include: ../b.yml\n" + step("a"),
			"b.yml":     "include: sub/a.yml\n" + step("b"),
		}, "b.yml:1:10: include DIR/sub/a.yml: a cycle: DIR/sub/a.yml -> DIR/b.yml -> DIR/sub/a.yml"},
		{"bad key in included", map[string]string{
			"p.yml": "include: a.yml\n" + step("p"),
			"a.yml": "stepz: []\n",
		}, `a.yml:1:1: unknown key "stepz" (a playbook has name, include, vars and steps)`},
		{"step in two files", map[string]string{
			"p.yml": "include: a.yml\n" + step("s"),
			"a.yml": "\n" + step("s"),
		}, `p.yml:3:5: step "s": a step of that name is at DIR/a.yml:3`},
		{"handler in two files", map[string]string{
			"p.yml": "include: a.yml\nsteps:\n  - name: p\n    handlers:\n      - {name: h, test.cmd: {cmd: [x]}}\n",
			"a.yml": "steps:\n  - name: a\n    handlers:\n      - {name: h, test.cmd: {cmd: [x]}}\n",
		}, `p.yml:5:9: handler "h": a handler of that name is at DIR/a.yml:4`},
		{"error in included task", map[string]string{
			"p.yml": "include: a.yml\n" + step("p"),
			"a.yml": "steps:\n  - name: a\n    tasks:\n      - nope: {}\n",
		}, `a.yml:4:9: unknown module "nope"`},
		{"no steps anywhere", map[string]string{
			"p.yml": "include: a.yml\n",
			"a.yml": "vars: {A: 1}\n",
		}, "p.yml:1:1: no steps"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range c.files {
				hosttest.WriteFile(t, dir, name, content)
			}
			_, err := load(filepath.Join(dir, "p.yml"), playbook.Options{Env: env(h)})
			want := strings.ReplaceAll(c.want, "DIR", dir)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err  %v\nwant %s", err, want)
			}
		})
	}
}

// A registered result and a handler are the play's, whichever file they
// are in.
func TestIncludeRegisterAndNotifyAcross(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteFile(t, dir, "core.yml", `
steps:
  - name: core
    tasks:
      - test.cmd: {cmd: [probe]}
        register: probe
    handlers:
      - name: mark
        test.file: {path: /marked, content: x}
`)
	path := hosttest.WriteFile(t, dir, "p.yml", `
include: core.yml
steps:
  - name: own
    tasks:
      - test.file: {path: /own, content: x}
        when: probe.rc == 0
        notify: mark
`)
	h := hosttest.New(t)
	pb, err := load(path, playbook.Options{Env: env(h)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Apply(context.Background(), &host.Runner{}); err != nil {
		t.Fatal(err)
	}
	if !h.Exists("/own") || !h.Exists("/marked") {
		t.Fatal("the includer's task did not see the included register or handler")
	}
}

// Check reports the problems of every file, in the order the steps run.
func TestCheckInclude(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteFile(t, dir, "core.yml", "steps:\n  - name: core\n    tasks:\n      - test.cmd: {cmd: []}\n      - test.file: {path: /x, src: nope}\n")
	path := hosttest.WriteFile(t, dir, "p.yml", "include: core.yml\nsteps:\n  - name: own\n    tasks:\n      - test.cmd: {cmd: []}\n")
	rep := playbook.Check(path, playbook.Options{Registry: reg})
	if rep.Steps != 2 || rep.Tasks != 3 || len(rep.Problems) != 3 {
		t.Fatalf("%+v", rep)
	}
	for i, want := range []string{
		filepath.Join(dir, "core.yml") + ":4:9: test.cmd: cmd is empty",
		filepath.Join(dir, "core.yml") + ":5:9: test.file: no files directory",
		path + ":5:9: test.cmd: cmd is empty",
	} {
		if !strings.HasPrefix(rep.Problems[i].Error(), want) {
			t.Errorf("problem %d: %v, want %s", i, rep.Problems[i], want)
		}
	}
}

// An included file's tasks read the files/ next to it, not the
// includer's.
func TestIncludeFilesDir(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteFile(t, dir, "files/motd", "top\n")
	hosttest.WriteFile(t, dir, "sub/files/motd", "sub\n")
	hosttest.WriteFile(t, dir, "sub/s.yml", "steps:\n  - name: sub\n    tasks:\n      - test.file: {path: /sub, src: motd}\n")
	path := hosttest.WriteFile(t, dir, "p.yml", "include: sub/s.yml\nsteps:\n  - name: top\n    tasks:\n      - test.file: {path: /top, src: motd}\n")
	h := hosttest.New(t)
	pb, err := load(path, playbook.Options{Env: env(h)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Apply(context.Background(), &host.Runner{}); err != nil {
		t.Fatal(err)
	}
	if h.Read("/top") != "top\n" || h.Read("/sub") != "sub\n" {
		t.Fatal(h.Read("/top"), h.Read("/sub"))
	}
	if err := os.RemoveAll(filepath.Join(dir, "sub/files")); err != nil {
		t.Fatal(err)
	}
	if _, err := load(path, playbook.Options{Env: env(h)}); err == nil || !strings.Contains(err.Error(), "sub/s.yml:4:9: test.file: no files directory") {
		t.Fatal(err)
	}
}
