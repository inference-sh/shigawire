package host

import (
	"context"
	"errors"
	"testing"
)

func TestQuery(t *testing.T) {
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "grep -q x /etc/f":
			return Output{Code: 1}
		case "missing":
			return Output{Code: -1, Err: errors.New("not found")}
		}
		return Output{}
	}}
	q := &Query{Exec: ex, Cmd: Cmd{Name: "grep", Args: []string{"-q", "x", "/etc/f"}}}
	for _, m := range []func(*Runner, context.Context, Play) error{(*Runner).Run, (*Runner).Plan, (*Runner).Verify} {
		if got, err := runMode(m, Play{Tasks: []Task{{Resource: q}}}); err != nil || got[0].Status != OK {
			t.Fatalf("%v %v", got, err)
		}
	}
	if n := len(ex.calls); n != 3 {
		t.Fatalf("ran %d times, want once a mode", n)
	}
	if _, err := (&Query{Exec: ex, Cmd: Cmd{Name: "missing"}}).Check(context.Background()); err == nil {
		t.Fatal("a command that did not run is not an error")
	}
}
