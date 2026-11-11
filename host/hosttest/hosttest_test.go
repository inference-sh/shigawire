package hosttest_test

import (
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

func TestExec(t *testing.T) {
	x := (&hosttest.Exec{Respond: func(line string) host.Output { return hosttest.Out("default") }}).
		On("systemctl is-active", hosttest.Exit(3)).
		On("systemctl", hosttest.Out("ActiveState=active"))
	if o := x.Run(t.Context(), host.Cmd{Name: "systemctl", Args: []string{"is-active", "x"}}); o.OK() || o.Code != 3 || !strings.Contains(o.Err.Error(), "exit 3") {
		t.Fatalf("%+v", o)
	}
	if o := x.Run(t.Context(), host.Cmd{Name: "systemctl", Args: []string{"show", "x"}}); o.Out != "ActiveState=active" {
		t.Fatalf("%+v", o)
	}
	if o := x.Run(t.Context(), host.Cmd{Name: "true"}); o.Out != "default" {
		t.Fatalf("%+v", o)
	}
	if got := x.Ran("systemctl"); len(got) != 2 {
		t.Fatal(got)
	}
	x.Reset()
	if len(x.Calls()) != 0 {
		t.Fatal("Reset kept the calls")
	}
}

// A play run in each mode against a Host.
func TestHostModes(t *testing.T) {
	h := hosttest.New(t)
	h.Write("/etc/app.conf", "old\n")
	restart := &host.Handler{Name: "restart", Resource: &host.Restart{Exec: h.Exec, Name: "app"}}
	p := host.Play{
		Tasks:    []host.Task{{Name: "conf", Resource: &host.File{Path: h.Path("/etc/app.conf"), Content: []byte("new\n")}, Notify: []*host.Handler{restart}}},
		Handlers: []*host.Handler{restart},
	}
	got, err := hosttest.Run(hosttest.Plan, p)
	if err != nil || hosttest.Statuses(got) != "conf=changed restart=changed" || len(h.Exec.Calls()) != 0 {
		t.Fatal(err, hosttest.Statuses(got), h.Exec.Calls())
	}
	if _, err := hosttest.Run(hosttest.Verify, p); err == nil {
		t.Fatal("verify passed with drift")
	}
	if got, err = hosttest.Run(hosttest.Apply, p); err != nil || hosttest.Statuses(got) != "conf=changed restart=changed" {
		t.Fatal(err, hosttest.Statuses(got))
	}
	if h.Read("/etc/app.conf") != "new\n" || len(h.Exec.Ran("systemctl restart app")) != 1 {
		t.Fatal(h.Read("/etc/app.conf"), h.Exec.Calls())
	}
	if _, err := hosttest.Run(hosttest.Verify, p); err != nil {
		t.Fatal(err)
	}
	if !h.Exists("/etc/app.conf") || h.Exists("/etc/none") {
		t.Fatal("Exists")
	}
}
