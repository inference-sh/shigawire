package host

import (
	"context"
	"path/filepath"
	"testing"
)

func TestComposePSForms(t *testing.T) {
	arr, err := parseComposePS(`[{"Service":"web","State":"running","Labels":"a=b,com.docker.compose.config-hash=h1"}]`)
	if err != nil || len(arr) != 1 || arr[0].label("com.docker.compose.config-hash") != "h1" {
		t.Fatal(arr, err)
	}
	lines, err := parseComposePS("{\"Service\":\"a\"}\n{\"Service\":\"b\"}\n")
	if err != nil || len(lines) != 2 {
		t.Fatal(lines, err)
	}
}

func TestComposeDrift(t *testing.T) {
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "docker compose ps --all --format json":
			return Output{Out: `{"Service":"web","State":"restarting","Labels":"com.docker.compose.config-hash=old"}` + "\n" +
				`{"Service":"worker","State":"exited","ExitCode":1,"Labels":"com.docker.compose.config-hash=w"}` + "\n"}
		case "docker compose config --hash *":
			return Output{Out: "web new\nworker w\ndb d\n"}
		}
		return Output{}
	}}
	st, err := (&Compose{Exec: ex, Dir: "/srv/app"}).Check(context.Background())
	if err != nil || st.Drift != "web config changed; worker exited; db not created" {
		t.Fatalf("%q %v", st.Drift, err)
	}
}

// Check refuses a join without a token or managers, and an init with
// either, before it runs anything.
func TestSwarm_Validate(t *testing.T) {
	for _, s := range []*Swarm{{Join: true, JoinToken: "x"}, {Join: true, RemoteAddrs: []string{"10.0.0.1:2377"}}, {JoinToken: "x"}} {
		ex := &fakeExec{}
		s.Exec = ex
		if _, err := s.Check(context.Background()); err == nil || len(ex.calls) != 0 {
			t.Errorf("%+v: %v, ran %v", s, err, ex.calls)
		}
	}
}

// In Plan, a project directory an earlier task makes is not there yet:
// compose is Unknown, saying so, and not Blocked, since the change
// planned before it explains it. With no such task it is Blocked.
func TestComposePlanNoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	compose := &Compose{Exec: OSExec{}, Dir: dir}
	got, err := runMode((*Runner).Plan, Play{Tasks: []Task{{Resource: &Dir{Path: dir}}, {Resource: compose}}})
	if err != nil || statuses(got) != "dir "+dir+"=changed compose "+dir+"=unknown" {
		t.Fatalf("%s %v", statuses(got), err)
	}
	if r := got[1]; r.Blocked || r.Msg != "project_src "+dir+" does not exist yet" {
		t.Fatalf("%+v", r)
	}
	got, _ = runMode((*Runner).Plan, Play{Tasks: []Task{{Resource: compose}}})
	if !got[0].Blocked {
		t.Fatalf("%+v", got[0])
	}
}
