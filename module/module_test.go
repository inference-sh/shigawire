package module_test

import (
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/module"
)

// fileArgs are the test module's arguments.
type fileArgs struct {
	Path    string      `yaml:"path"`
	Content string      `yaml:"content"`
	Mode    fs.FileMode `yaml:"mode"`
}

func testRegistry() *module.Registry {
	r := module.NewRegistry()
	r.Register("test.file", module.Typed(func(_ module.Env, a fileArgs) (host.Resource, error) {
		return &host.File{Path: a.Path, Content: []byte(a.Content), Mode: a.Mode, Parents: true}, nil
	}))
	return r
}

func TestRegistry(t *testing.T) {
	h := hosttest.New(t)
	reg := testRegistry()
	ctor, ok := reg.Lookup("test.file")
	if !ok {
		t.Fatal("test.file is not registered")
	}
	r, err := ctor(module.Env{Exec: h.Exec}, map[string]any{"path": h.Path("/etc/motd"), "content": "hi\n", "mode": "0600"})
	if err != nil {
		t.Fatal(err)
	}
	hosttest.Converge(t, r, host.Changed)
	if h.Read("/etc/motd") != "hi\n" {
		t.Fatal(h.Read("/etc/motd"))
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Fatal("found an unregistered module")
	}
	if _, ok := module.NewRegistry().Lookup("test.file"); ok {
		t.Fatal("a new registry shares another's modules")
	}
	if names := reg.Names(); !slices.Equal(names, []string{"test.file"}) {
		t.Fatal(names)
	}
	_, err = ctor(module.Env{}, map[string]any{"path": "/x", "contnet": "typo"})
	if err == nil || err.Error() != `unknown key "contnet"` {
		t.Fatalf("err %v", err)
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	reg := testRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	reg.Register("test.file", func(module.Env, map[string]any) (host.Resource, error) { return nil, nil })
}

func TestRegisterEmptyPanics(t *testing.T) {
	for name, ctor := range map[string]module.Ctor{
		"":  func(module.Env, map[string]any) (host.Resource, error) { return nil, nil },
		"x": nil,
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q, ctor nil=%v): no panic", name, ctor == nil)
				}
			}()
			module.NewRegistry().Register(name, ctor)
		}()
	}
}

type inner struct {
	Name string `yaml:"name"`
	Port uint16 `yaml:"port"`
}

type Embedded struct {
	Shared string `yaml:"shared"`
}

type allArgs struct {
	Embedded
	Str      string `yaml:"str"`
	Untagged int
	Skip     string            `yaml:"-"`
	Bool     bool              `yaml:"bool"`
	Int      int8              `yaml:"int"`
	Float    float64           `yaml:"float"`
	Dur      time.Duration     `yaml:"dur"`
	Mode     fs.FileMode       `yaml:"mode"`
	List     []string          `yaml:"list"`
	Items    []inner           `yaml:"items"`
	Map      map[string]int    `yaml:"map"`
	Ptr      *inner            `yaml:"ptr"`
	Any      any               `yaml:"any"`
	Env      map[string]string `yaml:"env"`
}

func TestDecode(t *testing.T) {
	var a allArgs
	err := module.Decode(map[string]any{
		"shared": "s", "str": "x", "untagged": 3, "bool": true, "int": 100, "float": 2,
		"dur": "1m30s", "mode": "0644", "list": []any{"a", "b"},
		"items": []any{map[string]any{"name": "n", "port": 80}},
		"map":   map[any]any{"k": 1.0}, "ptr": map[string]any{"name": "p"}, "any": []any{1},
		"env": nil,
	}, &a)
	if err != nil {
		t.Fatal(err)
	}
	if a.Shared != "s" || a.Str != "x" || a.Untagged != 3 || !a.Bool || a.Int != 100 || a.Float != 2 ||
		a.Dur != 90*time.Second || a.Mode != 0o644 || len(a.List) != 2 || a.Items[0].Port != 80 ||
		a.Map["k"] != 1 || a.Ptr.Name != "p" || a.Env != nil {
		t.Fatalf("%+v", a)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"skip": "x"}, `unknown key "skip"`},
		{map[string]any{"items": []any{map[string]any{"name": "n", "prot": 1}}}, `unknown key "items[0].prot"`},
		{map[string]any{"ptr": map[string]any{"nme": 1}}, `unknown key "ptr.nme"`},
		{map[string]any{"int": 300}, "int: want a whole number in range, got a number"},
		{map[string]any{"int": 1.5}, "int: want a whole number in range"},
		{map[string]any{"str": 1.5}, "str: want a string, got a number"},
		{map[string]any{"bool": "yes"}, "bool: want true or false, got a string"},
		{map[string]any{"mode": "rw"}, `mode: "rw" is not an octal mode`},
		{map[string]any{"mode": 644}, `mode: 644 is a number`},
		{map[string]any{"mode": 420}, `mode: 420 is a number`},
		{map[string]any{"dur": 5}, "dur: want a duration"},
		{map[string]any{"items": "x"}, "items: want a list, got a string"},
		{map[string]any{"items": []any{map[string]any{"port": -1}}}, "items[0].port: want a whole number in range"},
	} {
		var a allArgs
		if err := module.Decode(c.args, &a); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err %v, want %q", c.args, err, c.want)
		}
	}
	if err := module.Decode(nil, a); err == nil {
		t.Fatal("decoded into a non-pointer")
	}
}

type docArgs struct {
	Path  string        `yaml:"path" doc:"where the file goes"`
	Mode  fs.FileMode   `yaml:"mode"`
	Wait  time.Duration `yaml:"wait"`
	Items []inner       `yaml:"items"`
	Ptr   *inner        `yaml:"ptr"`
	Env   map[string]string
	Embedded
}

func TestArgs(t *testing.T) {
	reg := module.NewRegistry()
	reg.Register("test.doc", module.Typed(func(module.Env, docArgs) (host.Resource, error) { return nil, nil }))
	reg.Register("test.raw", func(module.Env, map[string]any) (host.Resource, error) { return nil, nil })
	args, ok := reg.Args("test.doc")
	if !ok {
		t.Fatal("no args")
	}
	var got []string
	for _, a := range args {
		got = append(got, a.Key+":"+a.Type+":"+a.Doc)
	}
	want := "path:string:where the file goes mode:mode: wait:duration: items:list of object: items[].name:string: items[].port:int: " +
		"ptr:object: ptr.name:string: ptr.port:int: env:map of string: shared:string:"
	if s := strings.Join(got, " "); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if _, ok := reg.Args("test.raw"); ok {
		t.Fatal("args of an untyped module")
	}
	if _, ok := reg.Args("nope"); ok {
		t.Fatal("args of no module")
	}
}

func TestEnvPath(t *testing.T) {
	if p := (module.Env{}).Path("/etc/x"); p != "/etc/x" {
		t.Fatal(p)
	}
	if p := (module.Env{Root: "/tmp/r"}).Path("/etc/x"); p != "/tmp/r/etc/x" {
		t.Fatal(p)
	}
}

// A whole number given for a string is its decimal text (port: 22).
func TestDecode_WholeNumberAsString(t *testing.T) {
	var a struct {
		Port string `yaml:"port"`
		Big  string `yaml:"big"`
	}
	if err := module.Decode(map[string]any{"port": 22, "big": uint64(1 << 40)}, &a); err != nil {
		t.Fatal(err)
	}
	if a.Port != "22" || a.Big != "1099511627776" {
		t.Fatalf("%+v", a)
	}
}
