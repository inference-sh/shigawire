package host

import (
	"context"
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
