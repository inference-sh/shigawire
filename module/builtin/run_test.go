package builtin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

func TestDownload(t *testing.T) {
	h, env := newHost(t)
	body := "#!/bin/sh\necho hi\n"
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	h.Write("/usr/local/bin/.keep", "")
	hosttest.Converge(t, build(t, env, "get_url", map[string]any{"url": srv.URL + "/hi.sh", "dest": "/usr/local/bin/hi",
		"sha256": hex.EncodeToString(sum[:]), "mode": "0755"}), host.Changed)
	if h.Read("/usr/local/bin/hi") != body {
		t.Fatal("content")
	}
	// A sha256 the download does not have: failed, the file not written.
	got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "download", map[string]any{"url": srv.URL, "dest": "/usr/local/bin/other",
		"sha256": strings.Repeat("0", 64)})})
	if err == nil || h.Exists("/usr/local/bin/other") {
		t.Fatal(hosttest.Statuses(got), err)
	}
	refuse(t, env, "download", map[string]any{"url": "ftp://x/y", "dest": "/x"}, "http or https")
	refuse(t, env, "download", map[string]any{"url": "https://x/y", "dest": "/x", "sha256": "abc"}, "64 hex digits")
	refuse(t, env, "download", map[string]any{"url": "https://x/y"}, "dest is required")
}

// recorder is an Exec that keeps every Cmd whole (hosttest.Exec keeps the
// line only).
type recorder struct {
	*hosttest.Exec
	cmds []host.Cmd
}

func (r *recorder) Run(ctx context.Context, c host.Cmd) host.Output {
	r.cmds = append(r.cmds, c)
	return r.Exec.Run(ctx, c)
}

func TestCommand(t *testing.T) {
	h, env := newHost(t)
	rec := &recorder{Exec: h.Exec}
	env.Exec = rec
	h.Exec.Respond = func(line string) host.Output {
		if line == "/bin/sh -c ./install.sh" {
			h.Write("/opt/tool/bin/tool", "")
		}
		return host.Output{}
	}
	r := build(t, env, "command", map[string]any{"cmd": "./install.sh", "creates": "/opt/tool/bin/tool",
		"chdir": "/opt/src", "env": map[string]any{"PREFIX": "/opt/tool", "A": "1"}})
	hosttest.Converge(t, r, host.Changed)
	if len(rec.cmds) != 1 || rec.cmds[0].Dir != "/opt/src" || strings.Join(rec.cmds[0].Env, " ") != "A=1 PREFIX=/opt/tool" {
		t.Fatalf("%+v", rec.cmds)
	}
	// unless: in place when it exits 0.
	h.Exec.Respond = func(line string) host.Output {
		if strings.HasPrefix(line, "systemctl is-active") {
			return hosttest.Exit(3)
		}
		return host.Output{}
	}
	first := build(t, env, "shell", map[string]any{"cmd": []any{"systemctl", "start", "app"}, "unless": []any{"systemctl", "is-active", "app"}})
	if got := hosttest.ApplyOne(t, first); got.Status != host.Changed {
		t.Fatal(got.Status)
	}
	h.Exec.Respond = nil
	if got := hosttest.ApplyOne(t, first); got.Status != host.OK {
		t.Fatal(got.Status)
	}
	// No guard: an action.
	if got := hosttest.ApplyOne(t, build(t, env, "command", map[string]any{"cmd": "true"})); got.Status != host.Changed || !got.Action {
		t.Fatal(got.Status, got.Action)
	}
	refuse(t, env, "command", map[string]any{}, "cmd: an empty command")
	refuse(t, env, "command", map[string]any{"cmd": "  "}, "an empty command")
	refuse(t, env, "command", map[string]any{"cmd": "x", "creates": "rel"}, "absolute")
	refuse(t, env, "command", map[string]any{"cmd": "x", "env": map[string]any{"A B": "1"}}, "not a variable name")
	refuse(t, env, "command", map[string]any{"cmd": "x", "timeout": 10}, "duration")
	// A negative timeout is refused, not taken as none or as the default.
	for _, m := range []string{"command", "query", "assert"} {
		refuse(t, env, m, map[string]any{"cmd": "x", "timeout": "-1m"}, "want a positive duration")
	}
	refuse(t, env, "query", map[string]any{"cmd": "x", "chdir": "rel"}, "absolute")
}

func TestCompose(t *testing.T) {
	h, env := newHost(t)
	up := false
	h.Exec.Respond = func(line string) host.Output {
		switch {
		case strings.Contains(line, " ps "):
			if !up {
				return hosttest.Out("")
			}
			return hosttest.Out(`{"Service":"web","State":"running","ExitCode":0,"Labels":"com.docker.compose.project=app,com.docker.compose.config-hash=abc"}` + "\n" +
				`{"Service":"migrate","State":"exited","ExitCode":0,"Labels":"com.docker.compose.config-hash=def"}` + "\n")
		case strings.Contains(line, " config --hash"):
			return hosttest.Out("web abc\nmigrate def\n")
		case strings.Contains(line, " up "):
			up = true
		case strings.Contains(line, " down "):
			up = false
		}
		return host.Output{}
	}
	hosttest.Converge(t, build(t, env, "docker_compose", map[string]any{"project_src": "/srv/app", "files": "compose.yaml", "project_name": "app"}), host.Changed)
	if got := h.Exec.Ran("docker compose -f compose.yaml -p app up"); len(got) != 1 {
		t.Fatal(h.Exec.Calls())
	}
	hosttest.Converge(t, build(t, env, "compose", map[string]any{"project_src": "/srv/app", "state": "absent"}), host.Changed)
	refuse(t, env, "compose", map[string]any{"project_src": "srv"}, "absolute")
	refuse(t, env, "compose", map[string]any{"project_src": "/srv", "state": "restarted"}, "want present, absent")
}

func TestSwarm(t *testing.T) {
	h, env := newHost(t)
	state := "inactive"
	h.Exec.Respond = func(line string) host.Output {
		switch {
		case strings.HasPrefix(line, "docker info"):
			return hosttest.Out(state + "\n")
		case strings.HasPrefix(line, "docker swarm init"), strings.HasPrefix(line, "docker swarm join"):
			state = "active"
		}
		return host.Output{}
	}
	if r := hosttest.Converge(t, build(t, env, "docker_swarm", map[string]any{"advertise_addr": "10.0.0.1"}), host.Changed); r.Msg != "swarm inactive: init" {
		t.Fatalf("%q: want what is wrong and what apply does", r.Msg)
	}
	if got := h.Exec.Ran("docker swarm"); len(got) != 1 || got[0] != "docker swarm init --advertise-addr 10.0.0.1" {
		t.Fatal(got)
	}
	state = "inactive"
	join := build(t, env, "docker_swarm", map[string]any{"state": "join", "join_token": "SWMTKN-1-secret", "remote_addrs": "10.0.0.1:2377"})
	hosttest.Converge(t, join, host.Changed)
	for _, st := range []string{"pending", "error", "locked"} {
		state = st
		got, err := play(hosttest.Apply, host.Task{Resource: join})
		if err == nil || got[0].Status != host.Failed || !strings.Contains(got[0].Msg, st) {
			t.Fatal(st, hosttest.Statuses(got), err)
		}
		// No task before it unlocks a swarm: plan stops on it.
		if got, _ := play(hosttest.Plan, host.Task{Resource: join}); got[0].Status != host.Unknown || !host.IsRefusal(got[0].Err) {
			t.Fatal(st, hosttest.Statuses(got), got[0].Err)
		}
	}
	// A join that fails says so without the token.
	state = "inactive"
	h.Exec.Respond = func(line string) host.Output {
		if strings.HasPrefix(line, "docker info") {
			return hosttest.Out("inactive")
		}
		return host.Output{Code: 1, Out: "Error response from daemon: timeout"}
	}
	got, err := play(hosttest.Apply, host.Task{Resource: join})
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "timeout") {
		t.Fatal(hosttest.Statuses(got), err)
	}
	refuse(t, env, "docker_swarm", map[string]any{"state": "join"}, "needs a join token and a manager's address")
	refuse(t, env, "docker_swarm", map[string]any{"join_token": "x"}, "are for docker swarm join")
	refuse(t, env, "docker_swarm", map[string]any{"state": "leave"}, "want init, join")
}

func TestAssert(t *testing.T) {
	h, env := newHost(t)
	use := "42%"
	h.Exec.Respond = func(line string) host.Output {
		if strings.Contains(line, "df") {
			return hosttest.Out("Use%\n " + use + "\n")
		}
		return host.Output{}
	}
	disk := build(t, env, "assert", map[string]any{"name": "disk use of /", "cmd": []any{"df", "--output=pcent", "/"},
		"regex": `(\d+)%`, "warn_max": 80, "max": 90})
	status := func(m hosttest.Mode) host.Status {
		got, _ := play(m, host.Task{Resource: disk})
		return got[0].Status
	}
	if s := status(hosttest.Apply); s != host.OK {
		t.Fatal(s)
	}
	use = "85%"
	if s := status(hosttest.Apply); s != host.Warn {
		t.Fatal(s)
	}
	if s := status(hosttest.Verify); s != host.Warn {
		t.Fatal(s)
	}
	use = "95%"
	if s := status(hosttest.Apply); s != host.Failed {
		t.Fatal(s)
	}
	if got, _ := play(hosttest.Apply, host.Task{Resource: disk}); got[0].Msg != "95, above 90" {
		t.Fatal(got[0].Msg)
	}
	// A check that prints a level and a finding: the finding is the message.
	h.Exec.On("check ufw", hosttest.Out("2 ufw inactive\n"))
	level := build(t, env, "assert", map[string]any{"cmd": []any{"check", "ufw"}, "regex": `^([0-2]) `, "warn_max": 0, "max": 1})
	if got, _ := play(hosttest.Apply, host.Task{Resource: level}); got[0].Status != host.Failed || got[0].Msg != "2, above 1: ufw inactive" {
		t.Fatal(got[0].Status, got[0].Msg)
	}
	// warn: a failure warns.
	soft := build(t, env, "assert", map[string]any{"cmd": "df --output=pcent /", "regex": `(\d+)%`, "max": 90, "warn": true})
	if got, err := play(hosttest.Apply, host.Task{Resource: soft}); err != nil || got[0].Status != host.Warn {
		t.Fatal(hosttest.Statuses(got), err)
	}
	// Only a regex; it changes nothing, and runs only its command.
	h.Exec.Reset()
	ok := build(t, env, "assert", map[string]any{"cmd": "df", "regex": "Use%"})
	hosttest.Converge(t, ok, host.OK)
	if got := h.Exec.Calls(); len(got) != 2 {
		t.Fatal(got)
	}
	refuse(t, env, "assert", map[string]any{"regex": "x"}, "cmd: an empty command")
	refuse(t, env, "assert", map[string]any{"cmd": "x", "regex": "("}, "regex")
	refuse(t, env, "assert", map[string]any{"cmd": "x", "min": 5, "max": 1}, "min above max")
	refuse(t, env, "assert", map[string]any{"cmd": "x", "max": "ninety"}, "want a number")
}

func TestQuery(t *testing.T) {
	h, env := newHost(t)
	h.Exec.On("grep -q x /etc/f", hosttest.Exit(1))
	r := build(t, env, "query", map[string]any{"cmd": []any{"grep", "-q", "x", "/etc/f"}})
	for _, m := range []hosttest.Mode{hosttest.Apply, hosttest.Plan, hosttest.Verify} {
		if got, err := play(m, host.Task{Resource: r}); err != nil || got[0].Status != host.OK {
			t.Fatal(hosttest.Statuses(got), err)
		}
	}
	if n := len(h.Exec.Ran("grep")); n != 3 {
		t.Fatalf("ran %d times", n)
	}
	refuse(t, env, "query", map[string]any{}, "cmd: an empty command")
	refuse(t, env, "query", map[string]any{"cmd": "x", "env": map[string]any{"a b": "1"}}, "not a variable name")
}

func TestProbe(t *testing.T) {
	h, env := newHost(t)
	opts := "rw,nr_inodes=1048576"
	h.Exec.Respond = func(line string) host.Output {
		switch {
		case strings.HasPrefix(line, "findmnt"):
			return hosttest.Out(opts + "\n")
		case strings.HasPrefix(line, "mount -o remount"):
			opts = "rw,nr_inodes=8388608"
		}
		return host.Output{}
	}
	args := map[string]any{"test": []any{"findmnt", "-no", "OPTIONS", "/tmp"}, "regex": `(^|,)nr_inodes=8388608(,|$)`,
		"drift": "remount", "fix": []any{[]any{"mount", "-o", "remount,nr_inodes=8388608", "/tmp"}}}
	if got := hosttest.Converge(t, build(t, env, "probe", args), host.Changed); got.Msg != "remount" {
		t.Fatal(got.Msg)
	}
	// A fix that does not fix is an error.
	opts = "rw"
	h.Exec.Respond = func(line string) host.Output { return hosttest.Out(opts) }
	if got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "probe", args)}); err == nil || got[0].Status != host.Failed || !strings.Contains(got[0].Msg, "still remount") {
		t.Fatal(hosttest.Statuses(got), err)
	}
	// With no fix, a failing test fails the task.
	delete(args, "fix")
	if got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "probe", args)}); err == nil || got[0].Status != host.Failed {
		t.Fatal(hosttest.Statuses(got), err)
	}
	refuse(t, env, "probe", map[string]any{"test": "x", "regex": "("}, "regex")
}

func TestFail(t *testing.T) {
	_, env := newHost(t)
	got, err := play(hosttest.Apply, host.Task{Resource: build(t, env, "fail", map[string]any{"msg": "set HOST_NAME"})})
	if err == nil || got[0].Status != host.Failed || got[0].Msg != "set HOST_NAME" {
		t.Fatal(hosttest.Statuses(got), got[0].Msg, err)
	}
	refuse(t, env, "fail", map[string]any{}, "msg is required")
}
