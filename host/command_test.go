package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommand_RunsEveryTimeWithoutAGuard(t *testing.T) {
	ex := &fakeExec{}
	c := &Command{Exec: ex, Cmd: Cmd{Name: "update-initramfs", Args: []string{"-u"}}}
	for range 2 {
		if r := apply(t, c); r.Status != Changed {
			t.Fatalf("%s", r.Status)
		}
	}
	if len(ex.calls) != 2 {
		t.Fatalf("ran %v", ex.calls)
	}
}

func TestCommand_Guards(t *testing.T) {
	dir := t.TempDir()
	there := filepath.Join(dir, "md0")
	os.WriteFile(there, nil, 0o644)
	gone := filepath.Join(dir, "nvidia-uninstall")
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "grep -q x f":
			return exit(0)
		case "sh swarm":
			return Output{Out: "inactive\nswarm inactive: init\n", Code: 1}
		}
		return exit(1)
	}}
	for name, c := range map[string]*Command{
		"creates": {Creates: there},
		"removes": {Removes: gone},
		"unless":  {Unless: &Cmd{Name: "grep", Args: []string{"-q", "x", "f"}}},
	} {
		c.Exec, c.Cmd = ex, Cmd{Name: "mdadm"}
		if st, err := c.Check(context.Background()); err != nil || !st.InPlace {
			t.Errorf("%s: %+v %v", name, st, err)
		}
	}
	// The drift says what the guard found.
	for want, c := range map[string]*Command{
		"no " + gone:           {Creates: gone},
		there + " exists":      {Removes: there},
		"false exited 1":       {Unless: &Cmd{Name: "false"}},
		"swarm inactive: init": {Unless: &Cmd{Name: "sh", Args: []string{"swarm"}}},
	} {
		c.Exec, c.Cmd = ex, Cmd{Name: "mdadm"}
		if st, err := c.Check(context.Background()); err != nil || st.InPlace || st.Drift != want {
			t.Errorf("%s: %+v %v", want, st, err)
		}
	}
	if len(ex.ran("mdadm")) != 0 {
		t.Fatal("a check ran the command")
	}
}

// nvidia-ctk rewrites daemon.json each run; Docker restarts only when the
// content changed.
func TestCommand_Watch(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "daemon.json")
	next := `{"runtimes":{}}`
	ex := &fakeExec{respond: func(string) Output {
		os.WriteFile(cfg, []byte(next), 0o644)
		return Output{}
	}}
	c := &Command{Exec: ex, Cmd: Cmd{Name: "nvidia-ctk"}, Watch: []string{cfg}}
	if r := apply(t, c); r.Status != Changed || r.Msg != "changed "+cfg {
		t.Fatalf("first: %s %q", r.Status, r.Msg)
	}
	if r := apply(t, c); r.Status != OK {
		t.Fatalf("same content: %s %q", r.Status, r.Msg)
	}
	next = `{"runtimes":{"nvidia":{}}}`
	if r := apply(t, c); r.Status != Changed {
		t.Fatalf("new content: %s", r.Status)
	}
}

func TestCommand_NoChangeAndFailure(t *testing.T) {
	ex := &fakeExec{respond: func(line string) Output {
		if strings.HasPrefix(line, "sh bad") {
			return Output{Out: "line 1\nchecksum mismatch\n", Code: 2}
		}
		return Output{Out: "OK\n"}
	}}
	if r := apply(t, &Command{Exec: ex, Cmd: Cmd{Name: "sh", Args: []string{"nvidia.run", "--check"}}, NoChange: true}); r.Status != OK || r.Msg != "OK" {
		t.Fatalf("verify: %s %q", r.Status, r.Msg)
	}
	_, err := runPlay(context.Background(), Play{Tasks: []Task{{Resource: &Command{Exec: ex, Cmd: Cmd{Name: "sh", Args: []string{"bad"}}}}}})
	if err == nil || !strings.Contains(err.Error(), "exit 2: line 1 | checksum mismatch") {
		t.Fatalf("err %v", err)
	}
}
