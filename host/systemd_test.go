package host

import (
	"context"
	"slices"
	"testing"
)

func TestService(t *testing.T) {
	enabled, active := false, false
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "systemctl show -p UnitFileState -p ActiveState docker":
			return Output{Out: "UnitFileState=" + map[bool]string{true: "enabled", false: "disabled"}[enabled] +
				"\nActiveState=" + map[bool]string{true: "active", false: "inactive"}[active] + "\n"}
		case "systemctl enable --now docker":
			enabled, active = true, true
		case "systemctl enable docker":
			enabled = true
		case "systemctl start docker":
			active = true
		}
		return Output{}
	}}
	s := &Service{Exec: ex, Name: "docker", Enabled: true, Running: true}
	if r := converge(t, s, Changed); r.Msg != "enable and start" || len(ex.ran("systemctl enable --now")) != 1 {
		t.Fatalf("msg %q, ran %v", r.Msg, ex.calls)
	}
	// Running only: an enabled unit that stopped is started, nothing else.
	active = false
	ex.calls = nil
	converge(t, &Service{Exec: ex, Name: "docker", Running: true}, Changed)
	if len(ex.ran("systemctl enable")) != 0 || len(ex.ran("systemctl start")) != 1 {
		t.Fatalf("ran %v", ex.calls)
	}
	// A static unit cannot be enabled, and need not be.
	enabled = false
	st, err := (&Service{Exec: &fakeExec{respond: func(string) Output {
		return Output{Out: "UnitFileState=static\nActiveState=active\n"}
	}}, Name: "x", Enabled: true, Running: true}).Check(context.Background())
	if err != nil || !st.InPlace {
		t.Fatalf("a static unit: %+v %v", st, err)
	}
}

func TestRestartAndDaemonReload(t *testing.T) {
	ex := &fakeExec{}
	apply(t, &Restart{Exec: ex, Name: "infsh-boxd"})
	apply(t, &DaemonReload{Exec: ex})
	if want := []string{"systemctl restart infsh-boxd", "systemctl daemon-reload"}; !slices.Equal(ex.calls, want) {
		t.Fatalf("ran %v", ex.calls)
	}
	res, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: &Restart{Exec: &fakeExec{respond: func(string) Output { return exit(1) }}, Name: "x"}}}})
	if err == nil || res[0].Status != Failed || res[0].Msg != "systemctl: exit 1" {
		t.Fatalf("a failed restart: %v %+v", err, res)
	}
}
