package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// checkFails is a resource whose Check fails.
type checkFails struct{ err error }

func (c *checkFails) ID() string                                   { return "check fails" }
func (c *checkFails) Check(context.Context) (State, error)         { return State{}, c.err }
func (c *checkFails) Apply(context.Context, State) (Result, error) { panic("applied") }

// Plan checks every task, applies none, plans the notified handlers, and
// reports an action as one, through Lazy too.
func TestRunner_PlanChangesNothing(t *testing.T) {
	var log []string
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")
	restart := &Handler{Name: "restart", Resource: &Restart{Exec: &fakeExec{}, Name: "x"}}
	ex := &fakeExec{}
	p := Play{
		Tasks: []Task{
			{Resource: &File{Path: path, Content: []byte("x")}, Notify: []*Handler{restart}},
			{Resource: &stub{id: "in place", inPlace: true, log: &log}},
			{Resource: &checkFails{errors.New("no such unit")}},
			{Resource: &Lazy{Name: "lazy cmd", Make: func(context.Context) (Resource, error) {
				return &Command{Exec: ex, Cmd: Cmd{Name: "true"}}, nil
			}}},
			{Resource: &stub{id: "after", log: &log}},
		},
		Handlers: []*Handler{restart},
	}
	got, err := runMode((*Runner).Plan, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 || len(ex.calls) != 0 {
		t.Fatalf("applied %v, ran %v", log, ex.calls)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("plan wrote the file")
	}
	want := "file " + path + "=changed in place=ok check fails=unknown lazy cmd=changed after=changed restart=changed"
	if s := statuses(got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if !got[3].Action || got[0].Action || got[0].Msg != "create" {
		t.Fatalf("lazy action %v, file %+v", got[3].Action, got[0])
	}
	if h := got[5]; !h.Handler || !h.Action || h.Msg != "restart" {
		t.Fatalf("handler %+v", h)
	}
}

// A plan's Unknown is Blocked when no change planned before it explains
// it: before any change, or a Refusal anywhere. A task that ignores errors
// never blocks: its failed Check is Ignored, as Run goes on past it. Run
// and Verify never set it.
func TestRunner_PlanBlocked(t *testing.T) {
	p := Play{Tasks: []Task{
		{Resource: &checkFails{errors.New("no user")}, IgnoreErrors: true},
		{Resource: &checkFails{errors.New("no docker")}},
		{Resource: &stub{id: "change"}},
		{Name: "owner", Resource: &checkFails{errors.New("no user app")}},
		{Name: "fail", Resource: &checkFails{Refusal(errors.New("DOMAIN is empty"))}},
		{Name: "ignored", Resource: &checkFails{Refusal(errors.New("PORT is empty"))}, IgnoreErrors: true},
	}}
	got, err := runMode((*Runner).Plan, p)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []bool
	for _, r := range got {
		blocked = append(blocked, r.Blocked)
	}
	if !slices.Equal(blocked, []bool{false, true, false, false, true, false}) {
		t.Fatalf("blocked %v: %s", blocked, statuses(got))
	}
	if got[0].Status != Ignored || got[5].Status != Ignored {
		t.Fatalf("ignore_errors: %s", statuses(got))
	}
	got, _ = runMode((*Runner).Verify, p)
	if slices.ContainsFunc(got, func(r Result) bool { return r.Blocked }) {
		t.Fatalf("verify set Blocked: %+v", got)
	}
}

// Verify reports drift, runs no action and no handler, reads every task,
// and fails when anything drifted or is unknown.
func TestRunner_Verify(t *testing.T) {
	var log []string
	ex := &fakeExec{}
	h := &Handler{Name: "h", Resource: &stub{id: "h", log: &log}}
	p := Play{Tasks: []Task{
		{Resource: &stub{id: "drifted", log: &log}, Notify: []*Handler{h}},
		{Resource: &Command{Exec: ex, Cmd: Cmd{Name: "touch", Args: []string{"x"}}}},
		{Resource: &checkFails{errors.New("cannot read")}},
		{Resource: &stub{id: "ok", inPlace: true, log: &log}},
	}}
	got, err := runMode((*Runner).Verify, p)
	if err == nil || !strings.Contains(err.Error(), "1 not in place, 0 failed, 1 unknown") {
		t.Fatalf("err %v", err)
	}
	if len(log) != 0 || len(ex.calls) != 0 {
		t.Fatalf("applied %v, ran %v", log, ex.calls)
	}
	if s := statuses(got); s != "drifted=changed command touch x=ok check fails=unknown ok=ok" {
		t.Fatal(s)
	}
	if !got[1].Action {
		t.Fatal("the command is not marked an action")
	}
	got, err = runMode((*Runner).Verify, Play{Tasks: []Task{{Resource: &stub{id: "ok", inPlace: true}}}})
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
}

// A Check that read the host and found it wrong (a Finding: an Assert that
// does not hold, a Probe with no fix that fails; a Refusal) is Failed in
// Verify, and Verify reads on. One that could not read is Unknown. Either
// is Ignored in a task that ignores errors, and fails no Verify. Plan
// reports a Finding Unknown, Blocked by the rules of any other.
func TestRunner_VerifyFinding(t *testing.T) {
	ex := &fakeExec{respond: func(string) Output { return Output{Code: 1, Err: errors.New("exit 1")} }}
	no := func(context.Context) error { return errors.New("2, above 1") }
	p := Play{Tasks: []Task{
		{Name: "assert", Resource: &Assert{Name: "assert", Holds: no}},
		{Name: "probe", Resource: &Probe{Exec: ex, Test: Cmd{Name: "check"}}},
		{Name: "refusal", Resource: &checkFails{Refusal(errors.New("DOMAIN is empty"))}},
		{Name: "finding", Resource: &checkFails{Finding(errors.New("wrong"))}},
		{Name: "ignored", Resource: &Assert{Name: "ignored", Holds: no}, IgnoreErrors: true},
		{Name: "unread", Resource: &checkFails{errors.New("no ufw")}, IgnoreErrors: true},
		{Name: "missing", Resource: &checkFails{errors.New("no user deploy")}},
		{Name: "drifted", Resource: &stub{id: "drifted"}},
	}}
	got, err := runMode((*Runner).Verify, p)
	want := "assert=failed probe=failed refusal=failed finding=failed ignored=ignored unread=ignored missing=unknown drifted=changed"
	if s := statuses(got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if err == nil || !strings.Contains(err.Error(), "1 not in place, 4 failed, 1 unknown") {
		t.Fatalf("err %v", err)
	}
	if got[0].Msg != "2, above 1" {
		t.Fatalf("msg %q", got[0].Msg)
	}
	got, err = runMode((*Runner).Plan, Play{Tasks: []Task{
		{Name: "before", Resource: &Assert{Name: "before", Holds: no}},
		{Name: "change", Resource: &stub{id: "change"}},
		{Name: "after", Resource: &Assert{Name: "after", Holds: no}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(got); s != "before=unknown change=changed after=unknown" || !got[0].Blocked || got[2].Blocked {
		t.Fatalf("plan %s, blocked %v %v", s, got[0].Blocked, got[2].Blocked)
	}
}

// A Warning from Check or Apply is Warn, and the play goes on without
// notifying.
func TestRunner_Warning(t *testing.T) {
	var log []string
	h := &Handler{Name: "h", Resource: &stub{id: "h", log: &log}}
	p := Play{Tasks: []Task{
		{Resource: &checkFails{Warning(errors.New("swap is on"))}},
		{Resource: &stub{id: "w", log: &log, fail: Warning(errors.New("slow disk"))}, Notify: []*Handler{h}},
		{Resource: &stub{id: "next", log: &log}},
		{Resource: &Assert{Name: "assert", Warn: true, Holds: func(context.Context) error { return errors.New("no") }}},
	}}
	got, err := runMode((*Runner).Run, p)
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(got); s != "check fails=warn w=warn next=changed assert=warn" {
		t.Fatal(s)
	}
	if !slices.Equal(log, []string{"w", "next"}) {
		t.Fatal(log)
	}
	if got, _ := runMode((*Runner).Plan, p); got[0].Status != Warn {
		t.Fatal("plan does not keep a warning", statuses(got))
	}
}

// A handler whose need failed does not run; one notified need runs first
// whatever the play's order.
func TestRunner_HandlerNeeds(t *testing.T) {
	var log []string
	validate := &Handler{Name: "validate", Resource: &stub{id: "validate", log: &log, fail: errors.New("bad config")}}
	reload := &Handler{Name: "reload", Resource: &stub{id: "reload", log: &log}, Needs: []*Handler{validate}}
	after := &Handler{Name: "after", Resource: &stub{id: "after", log: &log}, Needs: []*Handler{reload}}
	other := &Handler{Name: "other", Resource: &stub{id: "other", log: &log}}
	p := Play{
		Tasks:    []Task{{Resource: &stub{id: "t", log: &log}, Notify: []*Handler{after, reload, validate, other}}},
		Handlers: []*Handler{after, reload, validate, other},
	}
	got, err := runMode((*Runner).Run, p)
	if err == nil || !strings.Contains(err.Error(), `not run: handler "validate" failed`) {
		t.Fatalf("err %v", err)
	}
	if !slices.Equal(log, []string{"t", "validate", "other"}) {
		t.Fatalf("applied %v", log)
	}
	if s := statuses(got); s != "t=changed validate=failed reload=failed after=failed other=changed" {
		t.Fatal(s)
	}
	// A need that passes runs first.
	log = nil
	validate.Resource = &stub{id: "validate", log: &log}
	p.Tasks[0].Resource = &stub{id: "t", log: &log}
	other.Resource = &stub{id: "other", log: &log}
	if _, err := runMode((*Runner).Run, p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"t", "validate", "reload", "after", "other"}) {
		t.Fatalf("applied %v", log)
	}
}

// A failed validation puts back the file the run changed, removes the one
// it created, and keeps the reload from running.
func TestValidated_PutsBack(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "sshd_config")
	dropin := filepath.Join(dir, "sshd_config.d", "10-x.conf")
	os.WriteFile(conf, []byte("old\n"), 0o600)
	valid := false
	ex := &fakeExec{respond: func(line string) Output {
		if strings.HasPrefix(line, "sshd -t") && !valid {
			return Output{Code: 255, Out: "bad option"}
		}
		return Output{}
	}}
	validate := &Handler{Name: "validate", Resource: &Validated{Exec: ex, Validate: Cmd{Name: "sshd", Args: []string{"-t"}}, Restore: []string{conf, dropin, filepath.Join(dir, "untouched")}}}
	reload := &Handler{Name: "reload", Resource: &Restart{Exec: ex, Name: "ssh"}, Needs: []*Handler{validate}}
	p := Play{
		Tasks: []Task{
			{Resource: &Line{Path: conf, Line: "new"}, Notify: []*Handler{validate, reload}},
			{Resource: &File{Path: dropin, Content: []byte("x\n"), Parents: true}, Notify: []*Handler{validate, reload}},
		},
		Handlers: []*Handler{validate, reload},
	}
	got, err := runMode((*Runner).Run, p)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(got[2].Msg, "put back "+conf) || !strings.Contains(got[2].Msg, "removed "+dropin) {
		t.Fatalf("validate: %s", got[2].Msg)
	}
	if b, _ := os.ReadFile(conf); string(b) != "old\n" {
		t.Fatalf("conf %q", b)
	}
	if fi, _ := os.Stat(conf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := os.Stat(dropin); err == nil {
		t.Fatal("the new drop-in is still there")
	}
	if len(ex.ran("systemctl")) != 0 {
		t.Fatal("reloaded after a failed validation")
	}

	valid = true
	got, err = runMode((*Runner).Run, p)
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(got); !strings.HasSuffix(s, "validate=changed reload=changed") {
		t.Fatal(s)
	}
	if b, _ := os.ReadFile(conf); string(b) != "old\nnew\n" {
		t.Fatalf("conf %q", b)
	}
}

func TestProbe(t *testing.T) {
	on := false
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "sysctl -n net.ipv4.ip_forward":
			if on {
				return Output{Out: "1\n"}
			}
			return Output{Out: "0\n"}
		case "sysctl -w net.ipv4.ip_forward=1":
			on = true
		}
		return Output{}
	}}
	p := &Probe{Name: "forwarding", Exec: ex, Test: Cmd{Name: "sysctl", Args: []string{"-n", "net.ipv4.ip_forward"}},
		OK:    func(o Output) bool { return strings.TrimSpace(o.Out) == "1" },
		Drift: "forwarding off", Fix: []Cmd{{Name: "sysctl", Args: []string{"-w", "net.ipv4.ip_forward=1"}}}}
	if r := converge(t, p, Changed); r.Msg != "forwarding off" {
		t.Fatal(r.Msg)
	}

	// A fix that does not take is an error.
	on = false
	stuck := *p
	stuck.Fix = []Cmd{{Name: "true"}}
	if _, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: &stuck}}}); err == nil || !strings.Contains(err.Error(), "ran true, and still forwarding off") {
		t.Fatal(err)
	}
	// No fix: a check.
	check := *p
	check.Fix = nil
	if _, err := check.Check(context.Background()); err == nil || err.Error() != "forwarding off" {
		t.Fatal(err)
	}
}

func TestWhen(t *testing.T) {
	var log []string
	cond := false
	w := &When{Cond: func(context.Context) (bool, error) { return cond, nil }, Resource: &stub{id: "docker part", log: &log}, Why: "no docker"}
	if r := apply(t, w); r.Status != Skipped || r.Msg != "no docker" || len(log) != 0 {
		t.Fatal(r, log)
	}
	for _, m := range []func(*Runner, context.Context, Play) error{(*Runner).Plan, (*Runner).Verify} {
		got, err := runMode(m, Play{Tasks: []Task{{Resource: w}}})
		if err != nil || got[0].Status != Skipped {
			t.Fatal(got, err)
		}
	}
	cond = true
	if r := converge(t, w, Changed); r.Name != "docker part" {
		t.Fatal(r.Name)
	}
	// An action stays one through When.
	w.Resource = &Restart{Exec: &fakeExec{}, Name: "x"}
	if st, _ := w.Check(context.Background()); !st.Action {
		t.Fatal("When hides the action")
	}
}

func TestSymlink_Force(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "cert.pem")
	os.WriteFile(link, []byte("placeholder"), 0o644)
	if _, err := (&Symlink{Path: link, Target: "/x"}).Check(context.Background()); err == nil {
		t.Fatal("replaced a file without Force")
	}
	converge(t, &Symlink{Path: link, Target: "/etc/letsencrypt/live/x/fullchain.pem", Force: true}, Changed)
	if got, _ := os.Readlink(link); got != "/etc/letsencrypt/live/x/fullchain.pem" {
		t.Fatal(got)
	}
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	if _, err := (&Symlink{Path: filepath.Join(dir, "d"), Target: "/x", Force: true}).Check(context.Background()); err == nil {
		t.Fatal("Force replaced a directory")
	}
}

func TestBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysctl.conf")
	os.WriteFile(path, []byte("a=1\n"), 0o640)
	b := &Block{Path: path, Content: "x=1\ny=2\n"}
	if r := converge(t, b, Changed); r.Msg != "add the block" {
		t.Fatal(r.Msg)
	}
	want := "a=1\n# BEGIN MANAGED BLOCK\nx=1\ny=2\n# END MANAGED BLOCK\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Fatalf("%q", got)
	}
	os.WriteFile(path, []byte(want+"z=3\n"), 0o640)
	b.Content = "x=2"
	if r := converge(t, b, Changed); r.Msg != "set the block" {
		t.Fatal(r.Msg)
	}
	if got, _ := os.ReadFile(path); string(got) != "a=1\n# BEGIN MANAGED BLOCK\nx=2\n# END MANAGED BLOCK\nz=3\n" {
		t.Fatalf("%q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatal(fi.Mode())
	}
	converge(t, &Block{Path: path, Absent: true}, Changed)
	if got, _ := os.ReadFile(path); string(got) != "a=1\nz=3\n" {
		t.Fatalf("%q", got)
	}
	// A custom marker, a new file.
	other := filepath.Join(dir, "new")
	converge(t, &Block{Path: other, Marker: "// {mark} shiga", Content: "k"}, Changed)
	if got, _ := os.ReadFile(other); string(got) != "// BEGIN shiga\nk\n// END shiga\n" {
		t.Fatalf("%q", got)
	}
	if r := apply(t, &Block{Path: filepath.Join(dir, "none"), Absent: true}); r.Status != OK {
		t.Fatal(r.Status)
	}
	// No block and no trailing newline: nothing to remove.
	bare := filepath.Join(dir, "bare")
	os.WriteFile(bare, []byte("x"), 0o644)
	if r := apply(t, &Block{Path: bare, Absent: true}); r.Status != OK {
		t.Fatal(r.Status, r.Msg)
	}
	if got, _ := os.ReadFile(bare); string(got) != "x" {
		t.Fatalf("%q", got)
	}
	os.WriteFile(path, []byte("# BEGIN MANAGED BLOCK\nx\n"), 0o644)
	if _, err := b.Check(context.Background()); err == nil {
		t.Fatal("a begin marker with no end is not an error")
	}
}

func TestFile_Parents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "f")
	if _, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: &File{Path: path, Content: []byte("x")}}}}); err == nil {
		t.Fatal("made the parents without Parents")
	}
	converge(t, &File{Path: path, Content: []byte("x"), Parents: true}, Changed)
}

func TestApt_Installed(t *testing.T) {
	d := newFakeDpkg(map[string]string{"curl": "ii \t8.0", "old": "rc \t1"})
	a := &Apt{Exec: d.ex}
	for n, want := range map[string]bool{"curl": true, "curl=8.0": true, "curl=7": false, "curl:amd64": true, "old": false, "none": false} {
		if got, err := a.Installed(context.Background(), n); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", n, got, err, want)
		}
	}
	// A Command that installs packages makes Apt read them again.
	cmd := &Command{Exec: d.ex, Cmd: Cmd{Name: "sh", Args: []string{"get-docker.sh"}}, Apt: a}
	d.pkgs["docker-ce"] = "ii \t27"
	if ok, _ := a.Installed(context.Background(), "docker-ce"); ok {
		t.Fatal("the read was not kept")
	}
	apply(t, cmd)
	if ok, _ := a.Installed(context.Background(), "docker-ce"); !ok {
		t.Fatal("the command did not invalidate the read")
	}
}

func TestApt_RepoKeyringDir(t *testing.T) {
	root := t.TempDir()
	a := &Apt{Exec: &fakeExec{}, Root: root}
	apply(t, a.Repo(Repo{Name: "x", URIs: []string{"https://x"}, Suites: []string{"stable"}, Key: []byte(testKey), KeyringDir: "/usr/share/keyrings"}))
	if _, err := os.Stat(filepath.Join(root, "usr/share/keyrings/x.asc")); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(root, "etc/apt/sources.list.d/x.sources"))
	if !strings.Contains(string(src), "Signed-By: /usr/share/keyrings/x.asc") {
		t.Fatalf("%s", src)
	}
}

// A Validated inside a When still puts back; one the Runner cannot see
// before the run (made by a Lazy) refuses instead of failing without
// putting back.
func TestValidated_Wrapped(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "sshd_config")
	os.WriteFile(conf, []byte("old\n"), 0o600)
	ex := &fakeExec{respond: func(line string) Output {
		if strings.HasPrefix(line, "sshd -t") {
			return Output{Code: 1, Out: "bad"}
		}
		return Output{}
	}}
	v := &Validated{Exec: ex, Validate: Cmd{Name: "sshd", Args: []string{"-t"}}, Restore: []string{conf}}
	for _, tc := range []struct {
		name string
		r    Resource
		want string
	}{
		{"when", &When{Cond: func(context.Context) (bool, error) { return true, nil }, Resource: v}, "put back " + conf},
		{"lazy", &Lazy{Name: "lazy", Make: func(context.Context) (Resource, error) { return v, nil }}, "did not set up keeping it"},
	} {
		os.WriteFile(conf, []byte("old\n"), 0o600)
		h := &Handler{Name: "validate", Resource: tc.r}
		p := Play{Tasks: []Task{{Resource: &File{Path: conf, Content: []byte("bad\n")}, Notify: []*Handler{h}}}, Handlers: []*Handler{h}}
		_, err := runMode((*Runner).Run, p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.name == "when" {
			if b, _ := os.ReadFile(conf); string(b) != "old\n" {
				t.Fatalf("%s: conf %q", tc.name, b)
			}
		}
	}
	// Applied outside a Runner, it cannot put back: an error.
	if _, err := v.Apply(context.Background(), State{}); err == nil || !strings.Contains(err.Error(), "did not set up") {
		t.Fatal(err)
	}
}
