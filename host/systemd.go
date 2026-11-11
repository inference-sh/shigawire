package host

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// systemctlTimeout bounds one systemctl call (a unit's start included).
const systemctlTimeout = 5 * time.Minute

// Systemctl is systemctl with args, killed after systemctlTimeout.
func Systemctl(args ...string) Cmd {
	return Cmd{Name: "systemctl", Args: args, Timeout: systemctlTimeout}
}

// Service is a systemd unit enabled at boot or not, running or not, and
// its unit file. A field left false leaves that side of the unit as it
// is: Service never disables or stops a unit unless Disabled or Stopped
// says so.
type Service struct {
	Exec    Exec
	Name    string
	Enabled bool
	Running bool
	// Disabled: not enabled at boot (a static unit is left as it is).
	// Stopped: not running. Each is an error with its opposite.
	Disabled bool
	Stopped  bool
	// Unit, when set, is the unit file's content. A file that differs is
	// written, at UnitPath under Root, and systemd reads its units again
	// (daemon-reload) before the unit is enabled or started. A running
	// unit is not restarted: a task notifies a Restart for that.
	Unit []byte
	// UnitPath is where the unit file goes, as the host knows it; "" is
	// /etc/systemd/system/ and the unit's name (".service" added to a
	// name with no suffix).
	UnitPath string
	// Root is where the host's files are ("" is /).
	Root string
}

func (s *Service) ID() string {
	var want []string
	if s.Unit != nil {
		want = append(want, "unit file")
	}
	for _, w := range []struct {
		on   bool
		what string
	}{{s.Enabled, "enabled"}, {s.Running, "running"}, {s.Disabled, "disabled"}, {s.Stopped, "stopped"}} {
		if w.on {
			want = append(want, w.what)
		}
	}
	return "service " + s.Name + " " + strings.Join(want, ", ")
}

// unitPath is the unit file's path as the host knows it.
func (s *Service) unitPath() string {
	if s.UnitPath != "" {
		return s.UnitPath
	}
	name := s.Name
	if !strings.Contains(name, ".") {
		name += ".service"
	}
	return "/etc/systemd/system/" + name
}

func (s *Service) unitFile() *File {
	return &File{Path: UnderRoot(s.Root, s.unitPath()), Content: s.Unit, Mode: 0o644, Parents: true}
}

// enabledStates are the UnitFileStates is-enabled exits 0 for: enabled,
// or a unit that cannot be (static, alias, indirect, generated).
var enabledStates = []string{"enabled", "enabled-runtime", "static", "alias", "indirect", "generated", "transient"}

// serviceTodo is what Service.Check found to do: the unit file to write
// (nil: none) and the systemctl calls, with what they do.
type serviceTodo struct {
	file   *File
	fileSt State
	cmdPlan
}

func (s *Service) todo(ctx context.Context) (*serviceTodo, error) {
	if (s.Enabled && s.Disabled) || (s.Running && s.Stopped) {
		return nil, fmt.Errorf("service %s: enabled and disabled, or running and stopped", s.Name)
	}
	t := &serviceTodo{}
	if s.Unit != nil {
		f := s.unitFile()
		st, err := f.Check(ctx)
		if err != nil {
			return nil, err
		}
		if !st.InPlace {
			t.file, t.fileSt = f, st
			t.drift = append(t.drift, "unit file: "+st.Drift)
		}
	}
	o, err := run(ctx, s.Exec, Systemctl("show", "-p", "UnitFileState", "-p", "ActiveState", s.Name))
	if err != nil {
		return nil, err
	}
	kv := parseKV([]byte(o.Out))
	ufs, as := kv["UnitFileState"], kv["ActiveState"]
	active := as == "active" || as == "reloading"
	enable := s.Enabled && !slices.Contains(enabledStates, ufs)
	disable := s.Disabled && (ufs == "enabled" || ufs == "enabled-runtime")
	start := s.Running && !active
	stop := s.Stopped && (active || as == "activating")
	switch {
	case enable && start:
		t.add(Systemctl("enable", "--now", s.Name), "enable and start")
	case disable && stop:
		t.add(Systemctl("disable", "--now", s.Name), "disable and stop")
	default:
		for _, c := range []struct {
			do   bool
			verb string
		}{{enable, "enable"}, {disable, "disable"}, {start, "start"}, {stop, "stop"}} {
			if c.do {
				t.add(Systemctl(c.verb, s.Name), c.verb)
			}
		}
	}
	return t, nil
}

// Check's Data is what is to do (serviceTodo).
func (s *Service) Check(ctx context.Context) (State, error) {
	t, err := s.todo(ctx)
	switch {
	case err != nil:
		return State{}, err
	case t.file == nil && len(t.cmds) == 0:
		return inPlace()
	}
	return driftData(t, "%s", strings.Join(t.drift, "; "))
}

func (s *Service) Apply(ctx context.Context, st State) (Result, error) {
	t := st.Data.(*serviceTodo)
	if t.file != nil {
		if _, err := t.file.Apply(ctx, t.fileSt); err != nil {
			return Result{}, err
		}
		if _, err := run(ctx, s.Exec, Systemctl("daemon-reload")); err != nil {
			return Result{}, err
		}
	}
	return runCmds(ctx, s.Exec, State{Drift: st.Drift, Data: t.cmds})
}

// systemctlAction runs systemctl with args, for an action's Apply, and
// says done.
func systemctlAction(ctx context.Context, x Exec, done string, args ...string) (Result, error) {
	if _, err := run(ctx, x, Systemctl(args...)); err != nil {
		return Result{}, err
	}
	return changed("%s", done)
}

// Restart restarts a unit (or starts a stopped one). It is an action, for
// a handler: run because something the unit reads changed.
type Restart struct {
	Exec Exec
	Name string
}

func (r *Restart) ID() string                           { return "restart " + r.Name }
func (r *Restart) Check(context.Context) (State, error) { return action("restart") }
func (r *Restart) Apply(ctx context.Context, _ State) (Result, error) {
	return systemctlAction(ctx, r.Exec, "restarted", "restart", r.Name)
}

// Reload makes a unit read its config again (systemctl reload). It is an
// action, for a handler: run because a config the unit reads changed.
type Reload struct {
	Exec Exec
	Name string
}

func (r *Reload) ID() string                           { return "reload " + r.Name }
func (r *Reload) Check(context.Context) (State, error) { return action("reload") }
func (r *Reload) Apply(ctx context.Context, _ State) (Result, error) {
	return systemctlAction(ctx, r.Exec, "reloaded", "reload", r.Name)
}

// DaemonReload makes systemd read its units (and fstab's) again. It is an
// action, for a handler: run because a unit changed.
type DaemonReload struct {
	Exec Exec
}

func (d *DaemonReload) ID() string                           { return "systemd daemon-reload" }
func (d *DaemonReload) Check(context.Context) (State, error) { return action("reload") }
func (d *DaemonReload) Apply(ctx context.Context, _ State) (Result, error) {
	return systemctlAction(ctx, d.Exec, "reloaded", "daemon-reload")
}
