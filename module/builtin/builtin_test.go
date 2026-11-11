package builtin_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/module"
	"github.com/inference-sh/shigawire/module/builtin"
)

// builtins are the standard modules, for the tests.
var builtins = builtin.Registry()

// newHost is a fake host and the Env its modules act through.
func newHost(t *testing.T) (*hosttest.Host, module.Env) {
	h := hosttest.New(t)
	return h, module.NewEnv(h.Exec, h.Root)
}

// build is the resource the module name makes from args.
func build(t *testing.T, env module.Env, name string, args map[string]any) host.Resource {
	t.Helper()
	ctor, ok := builtins.Lookup(name)
	if !ok {
		t.Fatalf("no module %s", name)
	}
	r, err := ctor(env, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

// refuse wants the module name to refuse args with an error holding want.
func refuse(t *testing.T, env module.Env, name string, args map[string]any, want string) {
	t.Helper()
	ctor, ok := builtins.Lookup(name)
	if !ok {
		t.Fatalf("no module %s", name)
	}
	_, err := ctor(env, args)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s %v: err %v, want %q", name, args, err, want)
	}
}

// play runs the tasks with m and returns the results and the error.
func play(m hosttest.Mode, tasks ...host.Task) ([]host.Result, error) {
	return hosttest.Run(m, host.Play{Tasks: tasks})
}

func TestEveryModuleDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/modules.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range builtins.Names() {
		if !regexp.MustCompile("(?m)^## .*\\b" + regexp.QuoteMeta(n) + "\\b").Match(b) {
			t.Errorf("docs/modules.md has no heading for %s", n)
		}
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	_, env := newHost(t)
	for _, n := range builtins.Names() {
		refuse(t, env, n, map[string]any{"no_such_key": 1}, `unknown key "no_such_key"`)
	}
}

func TestBuiltinsAreSeparate(t *testing.T) {
	a, b := builtin.Registry(), builtin.Registry()
	a.Register("private", func(module.Env, map[string]any) (host.Resource, error) { return nil, nil })
	if _, ok := b.Lookup("private"); ok {
		t.Fatal("a module registered in one registry is in another")
	}
	if _, ok := b.Lookup("file"); !ok {
		t.Fatal("no file module")
	}
	r := module.NewRegistry()
	builtin.Register(r)
	if strings.Join(r.Names(), " ") != strings.Join(b.Names(), " ") {
		t.Fatal("Register and Builtins differ")
	}
}
