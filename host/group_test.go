package host

import "testing"

func TestGroupMember(t *testing.T) {
	groups := "ok adm"
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "id -nG ok":
			return Output{Out: groups + "\n"}
		case "usermod -a -G docker ok":
			groups += " docker"
		}
		return Output{}
	}}
	converge(t, &GroupMember{Exec: ex, User: "ok", Group: "docker"}, Changed)
	if len(ex.ran("usermod")) != 1 {
		t.Fatalf("ran %v", ex.calls)
	}
	// A group whose name another one contains is not it.
	groups = "ok dockerd"
	if r := apply(t, &GroupMember{Exec: ex, User: "ok", Group: "docker"}); r.Status != Changed {
		t.Fatalf("%s", r.Status)
	}
}
