package builtin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

func registerSystem(r *module.Registry) {
	register(r, module.Typed(newSystemd), "systemd", "service")
	r.Register("user", module.Typed(newUser))
	r.Register("group", module.Typed(newGroup))
	r.Register("sysctl", module.Typed(newSysctl))
	r.Register("restart", module.Typed(newRestart))
	r.Register("reload", module.Typed(newReload))
	r.Register("daemon_reload", module.Typed(newDaemonReload))
	r.Register("validate_reload", module.Typed(newValidateReload))
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+$`)

func checkUnit(name string) error {
	if err := required("name", name); err != nil {
		return err
	}
	if !unitName.MatchString(name) {
		return fmt.Errorf("name %q: not a unit name", name)
	}
	return nil
}

type systemdArgs struct {
	Name string `yaml:"name"`
	// Enabled: true enables the unit at boot, false disables it; unset
	// leaves it.
	Enabled *bool `yaml:"enabled"`
	// State is started or stopped; unset leaves it.
	State string `yaml:"state"`
	// Unit is the unit file's content; unset leaves the file.
	Unit *string `yaml:"unit"`
	// UnitPath is where the unit file goes; unset is
	// /etc/systemd/system/<name>.
	UnitPath string `yaml:"unit_path"`
}

func newSystemd(env module.Env, a systemdArgs) (host.Resource, error) {
	if err := checkUnit(a.Name); err != nil {
		return nil, err
	}
	if a.State != "" {
		if err := oneOf("state", a.State, "started", "stopped"); err != nil {
			return nil, fmt.Errorf("%w (restarted and reloaded are the restart and reload modules, for handlers)", err)
		}
	}
	if a.UnitPath != "" {
		if err := absPath("unit_path", a.UnitPath); err != nil {
			return nil, err
		}
		if a.Unit == nil {
			return nil, errors.New("unit_path without unit")
		}
	}
	if a.Enabled == nil && a.State == "" && a.Unit == nil {
		return nil, errors.New("enabled, state or unit is required")
	}
	s := &host.Service{Exec: env.Exec, Name: a.Name, UnitPath: a.UnitPath, Root: env.Root,
		Running: a.State == "started", Stopped: a.State == "stopped"}
	if a.Enabled != nil {
		s.Enabled, s.Disabled = *a.Enabled, !*a.Enabled
	}
	if a.Unit != nil {
		s.Unit = []byte(*a.Unit)
	}
	return s, nil
}

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

type userArgs struct {
	Name string `yaml:"name"`
	// UID is the user's id; unset leaves it to useradd.
	UID int `yaml:"uid"`
	// Group is the primary group, by name or id.
	Group string `yaml:"group"`
	// Groups are supplementary groups the user is added to (never removed
	// from others).
	Groups  List   `yaml:"groups"`
	Home    string `yaml:"home"`
	Shell   string `yaml:"shell"`
	Comment string `yaml:"comment"`
	System  bool   `yaml:"system"`
	// CreateHome: a new user's home is made; unset is true.
	CreateHome *bool `yaml:"create_home"`
	// NoPassword: no password and not locked (shadow "*"), for key login.
	NoPassword bool `yaml:"no_password"`
	// State is present (the default) or absent.
	State string `yaml:"state"`
}

func newUser(env module.Env, a userArgs) (host.Resource, error) {
	if err := required("name", a.Name); err != nil {
		return nil, err
	}
	if !accountName.MatchString(a.Name) {
		return nil, fmt.Errorf("name %q: not a user name", a.Name)
	}
	state := a.State
	if state == "" {
		state = "present"
	}
	if err := oneOf("state", state, "present", "absent"); err != nil {
		return nil, err
	}
	if a.UID < 0 {
		return nil, errors.New("uid: want a positive number")
	}
	if a.Home != "" {
		if err := absPath("home", a.Home); err != nil {
			return nil, err
		}
	}
	for _, s := range []string{a.Comment, a.Home, a.Shell} {
		if strings.ContainsAny(s, ":\n") {
			return nil, errors.New("home, shell and comment: no colon or newline")
		}
	}
	u := &host.User{Exec: env.Exec, Name: a.Name, UID: a.UID, Group: a.Group, Groups: a.Groups,
		Home: a.Home, Shell: a.Shell, Comment: a.Comment, System: a.System,
		CreateHome: a.CreateHome == nil || *a.CreateHome, NoPassword: a.NoPassword, Absent: state == "absent"}
	return u, nil
}

type groupArgs struct {
	Name string `yaml:"name"`
	// GID is the group's id; unset leaves it to groupadd.
	GID    int    `yaml:"gid"`
	System bool   `yaml:"system"`
	State  string `yaml:"state"`
}

func newGroup(env module.Env, a groupArgs) (host.Resource, error) {
	if err := required("name", a.Name); err != nil {
		return nil, err
	}
	if !accountName.MatchString(a.Name) {
		return nil, fmt.Errorf("name %q: not a group name", a.Name)
	}
	state := a.State
	if state == "" {
		state = "present"
	}
	if err := oneOf("state", state, "present", "absent"); err != nil {
		return nil, err
	}
	if a.GID < 0 {
		return nil, errors.New("gid: want a positive number")
	}
	return &host.Group{Exec: env.Exec, Name: a.Name, GID: a.GID, System: a.System, Absent: state == "absent"}, nil
}

// defaultSysctlFile is sysctl's drop-in when a task names none.
const defaultSysctlFile = "/etc/sysctl.d/99-shigawire.conf"

var sysctlKey = regexp.MustCompile(`^[a-z0-9_]+([./][A-Za-z0-9_*-]+)+$`)

type sysctlArgs struct {
	// Name is the key (net.ipv4.ip_forward).
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
	// File is the drop-in the key goes in; unset is
	// /etc/sysctl.d/99-shigawire.conf.
	File string `yaml:"file"`
}

func newSysctl(env module.Env, a sysctlArgs) (host.Resource, error) {
	if err := required("name", a.Name, "value", a.Value); err != nil {
		return nil, err
	}
	if !sysctlKey.MatchString(a.Name) {
		return nil, fmt.Errorf("name %q: not a sysctl key", a.Name)
	}
	if strings.ContainsAny(a.Value, "\n") {
		return nil, errors.New("value: no newline")
	}
	file := a.File
	if file == "" {
		file = defaultSysctlFile
	}
	if err := absPath("file", file); err != nil {
		return nil, err
	}
	return &host.Sysctl{Exec: env.Exec, Path: file, Key: a.Name, Value: a.Value, Root: env.Root}, nil
}

type unitArgs struct {
	Name string `yaml:"name"`
}

func newRestart(env module.Env, a unitArgs) (host.Resource, error) {
	if err := checkUnit(a.Name); err != nil {
		return nil, err
	}
	return &host.Restart{Exec: env.Exec, Name: a.Name}, nil
}

func newReload(env module.Env, a unitArgs) (host.Resource, error) {
	if err := checkUnit(a.Name); err != nil {
		return nil, err
	}
	return &host.Reload{Exec: env.Exec, Name: a.Name}, nil
}

type noArgs struct{}

func newDaemonReload(env module.Env, _ noArgs) (host.Resource, error) {
	return &host.DaemonReload{Exec: env.Exec}, nil
}

type validateReloadArgs struct {
	// Validate checks the config (sshd -t, nginx -t).
	Validate Cmd `yaml:"validate"`
	// Service is the unit reloaded when the config is valid; Then are
	// commands run instead. With neither, the handler only validates (and
	// restores): a gate other handlers name in needs.
	Service string `yaml:"service"`
	Then    []Cmd  `yaml:"then"`
	// Restore are files put back as they were before the run when the
	// config is not valid (those a file, line or block task wrote).
	Restore []string `yaml:"restore"`
}

func newValidateReload(env module.Env, a validateReloadArgs) (host.Resource, error) {
	v, err := a.Validate.cmd("validate")
	if err != nil {
		return nil, err
	}
	out := &host.Validated{Exec: env.Exec, Validate: v}
	switch {
	case a.Service != "" && len(a.Then) > 0:
		return nil, errors.New("service and then: give one")
	case a.Service != "":
		if err := checkUnit(a.Service); err != nil {
			return nil, fmt.Errorf("service: %w", err)
		}
		out.Then = []host.Cmd{host.Systemctl("reload", a.Service)}
		out.Name = "validate and reload " + a.Service
	case len(a.Then) > 0:
		for i, c := range a.Then {
			rc, err := c.cmd(fmt.Sprintf("then[%d]", i))
			if err != nil {
				return nil, err
			}
			out.Then = append(out.Then, rc)
		}
	}
	for _, p := range a.Restore {
		if err := absPath("restore", p); err != nil {
			return nil, err
		}
		out.Restore = append(out.Restore, env.Path(p))
	}
	return out, nil
}
