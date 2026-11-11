package host_test

import (
	"context"
	"slices"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
)

func TestService(t *testing.T) {
	h := sim.New(t)
	s := &host.Service{Exec: h.Exec, Name: "docker", Enabled: true, Running: true}
	if r := hosttest.Converge(t, s, host.Changed); r.Msg != "enable and start" || len(h.Exec.Ran("systemctl enable --now")) != 1 {
		t.Fatalf("msg %q, ran %v", r.Msg, h.Exec.Calls())
	}
	// Running only: an enabled unit that stopped is started, nothing else.
	h.Units["docker"].Active = false
	h.Exec.Reset()
	hosttest.Converge(t, &host.Service{Exec: h.Exec, Name: "docker", Running: true}, host.Changed)
	if len(h.Exec.Ran("systemctl enable")) != 0 || len(h.Exec.Ran("systemctl start")) != 1 {
		t.Fatalf("ran %v", h.Exec.Calls())
	}
	// A static unit cannot be enabled, and need not be.
	h.Exec.On("systemctl show", hosttest.Out("UnitFileState=static\nActiveState=active\n"))
	st, err := (&host.Service{Exec: h.Exec, Name: "x", Enabled: true, Running: true}).Check(context.Background())
	if err != nil || !st.InPlace {
		t.Fatalf("a static unit: %+v %v", st, err)
	}
}

func TestRestartAndDaemonReload(t *testing.T) {
	h := sim.New(t)
	hosttest.ApplyOne(t, &host.Restart{Exec: h.Exec, Name: "infsh-boxd"})
	hosttest.ApplyOne(t, &host.DaemonReload{Exec: h.Exec})
	if want := []string{"systemctl restart infsh-boxd", "systemctl daemon-reload"}; !slices.Equal(h.Exec.Calls(), want) {
		t.Fatalf("ran %v", h.Exec.Calls())
	}
	if h.Units["infsh-boxd"].Restarts != 1 || h.DaemonReloads != 1 {
		t.Fatalf("%+v, %d daemon-reloads", *h.Units["infsh-boxd"], h.DaemonReloads)
	}
	h.Exec.On("systemctl restart", hosttest.Exit(1))
	res, err := hosttest.Run(hosttest.Apply, host.Play{Tasks: []host.Task{{Resource: &host.Restart{Exec: h.Exec, Name: "x"}}}})
	if err == nil || res[0].Status != host.Failed || res[0].Msg != "systemctl: exit 1" {
		t.Fatalf("a failed restart: %v %+v", err, res)
	}
}
