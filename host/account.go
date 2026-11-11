package host

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GroupEntry is a group's entry in the system's group database.
type GroupEntry struct {
	Name    string
	GID     int
	Members []string
}

// LookupGroup is name's entry as getent group has it (NSS: /etc/group,
// LDAP), nil when there is none. A number names a group by its GID.
func LookupGroup(ctx context.Context, x Exec, name string) (*GroupEntry, error) {
	o := x.Run(ctx, Cmd{Name: "getent", Args: []string{"group", name}, Timeout: time.Minute})
	if o.Code == 2 { // getent: key not found
		return nil, nil
	}
	if o.Err != nil {
		return nil, fmt.Errorf("getent group %s: %w", name, o.Err)
	}
	f := strings.Split(strings.TrimSpace(o.Out), ":")
	if len(f) != 4 {
		return nil, fmt.Errorf("getent group: %q is not a group entry", strings.TrimSpace(o.Out))
	}
	gid, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, fmt.Errorf("getent group %s: gid %q", f[0], f[2])
	}
	g := &GroupEntry{Name: f[0], GID: gid}
	if f[3] != "" {
		g.Members = strings.Split(f[3], ",")
	}
	return g, nil
}

func account(name string, args ...string) Cmd {
	return Cmd{Name: name, Args: args, Timeout: time.Minute}
}

// Group is a group in the system's group database, made with groupadd
// when missing, or not there (Absent: removed with groupdel).
type Group struct {
	Exec Exec
	Name string
	// GID is the group's id; 0 leaves it to groupadd, and an existing
	// group's as it is. One that differs is set with groupmod.
	GID int
	// System: a new group gets a system GID (groupadd --system).
	System bool
	Absent bool
}

func (g *Group) ID() string {
	if g.Absent {
		return "no group " + g.Name
	}
	return "group " + g.Name
}

// Check's Data is the command that puts it in place.
func (g *Group) Check(ctx context.Context) (State, error) {
	e, err := LookupGroup(ctx, g.Exec, g.Name)
	switch {
	case err != nil:
		return State{}, err
	case e == nil && g.Absent:
		return inPlace()
	case g.Absent:
		return driftData(account("groupdel", g.Name), "remove")
	case e == nil:
		args := []string{}
		if g.System {
			args = append(args, "--system")
		}
		if g.GID != 0 {
			args = append(args, "--gid", strconv.Itoa(g.GID))
		}
		return driftData(account("groupadd", append(args, g.Name)...), "create")
	case g.GID != 0 && e.GID != g.GID:
		return driftData(account("groupmod", "--gid", strconv.Itoa(g.GID), g.Name), "gid %d, want %d", e.GID, g.GID)
	}
	return inPlace()
}

func (g *Group) Apply(ctx context.Context, st State) (Result, error) {
	if _, err := run(ctx, g.Exec, st.Data.(Cmd)); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// User is a user in the system's user database, made with useradd when
// missing and put right with usermod, or not there (Absent: removed with
// userdel, its home left). A field left empty is left to useradd for a
// new user and as it is for an existing one.
type User struct {
	Exec Exec
	Name string
	// UID is the user's id; 0 leaves it.
	UID int
	// Group is the primary group, by name or GID.
	Group string
	// Groups are supplementary groups the user is added to; it is not
	// removed from others.
	Groups  []string
	Home    string
	Shell   string
	Comment string
	// System: a new user gets a system UID (useradd --system).
	System bool
	// CreateHome: a new user's home is made (useradd --create-home);
	// else useradd is told not to (--no-create-home).
	CreateHome bool
	// NoPassword: the account has no password and is not locked: its
	// shadow password is "*", as adduser --disabled-password leaves it.
	// useradd leaves "!", a locked account, and sshd refuses a locked
	// account even a key login when UsePAM is no. A locked password ("!"
	// and a hash) is an error: it is not thrown away.
	NoPassword bool
	Absent     bool
}

func (u *User) ID() string {
	if u.Absent {
		return "no user " + u.Name
	}
	return "user " + u.Name
}

// Check's Data is the command that puts it in place.
func (u *User) Check(ctx context.Context) (State, error) {
	p, err := LookupPasswd(ctx, u.Exec, u.Name)
	switch {
	case err != nil:
		return State{}, err
	case p == nil && u.Absent:
		return inPlace()
	case u.Absent:
		return driftData(account("userdel", u.Name), "remove")
	case p == nil:
		return driftData(account("useradd", u.addArgs()...), "create")
	}
	var args, d []string
	if u.UID != 0 && p.UID != u.UID {
		args, d = append(args, "--uid", strconv.Itoa(u.UID)), append(d, fmt.Sprintf("uid %d, want %d", p.UID, u.UID))
	}
	if u.Group != "" {
		g, err := LookupGroup(ctx, u.Exec, u.Group)
		if err != nil {
			return State{}, err
		}
		if g == nil {
			return State{}, fmt.Errorf("user %s: there is no group %s", u.Name, u.Group)
		}
		if p.GID != g.GID {
			args, d = append(args, "--gid", u.Group), append(d, fmt.Sprintf("group %d, want %s", p.GID, u.Group))
		}
	}
	for _, f := range []struct{ got, want, flag, what string }{
		{p.Home, u.Home, "--home", "home"},
		{p.Shell, u.Shell, "--shell", "shell"},
		{p.Comment, u.Comment, "--comment", "comment"},
	} {
		if f.want != "" && f.got != f.want {
			args, d = append(args, f.flag, f.want), append(d, fmt.Sprintf("%s %q, want %q", f.what, f.got, f.want))
		}
	}
	if u.NoPassword {
		pw, err := shadowPassword(ctx, u.Exec, u.Name)
		if err != nil {
			return State{}, err
		}
		if strings.HasPrefix(pw, "!") {
			if rest := strings.TrimLeft(pw, "!"); rest != "" && rest != "*" {
				return State{}, fmt.Errorf("user %s: its password is locked with a hash in it; clear it (usermod -p '*') or unlock it by hand", u.Name)
			}
			args, d = append(args, "--password", "*"), append(d, fmt.Sprintf("password %q (locked), want \"*\"", pw))
		}
	}
	if len(u.Groups) > 0 {
		o, err := run(ctx, u.Exec, account("id", "-nG", u.Name))
		if err != nil {
			return State{}, fmt.Errorf("the groups of %s: %w", u.Name, err)
		}
		have := strings.Fields(o.Out)
		var missing []string
		for _, g := range u.Groups {
			if !slices.Contains(have, g) {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			args = append(args, "--append", "--groups", strings.Join(missing, ","))
			d = append(d, "add to "+strings.Join(missing, ", "))
		}
	}
	if len(args) == 0 {
		return inPlace()
	}
	return driftData(account("usermod", append(args, u.Name)...), "%s", strings.Join(d, "; "))
}

func (u *User) addArgs() []string {
	var args []string
	if u.System {
		args = append(args, "--system")
	}
	if u.CreateHome {
		args = append(args, "--create-home")
	} else {
		args = append(args, "--no-create-home")
	}
	if u.UID != 0 {
		args = append(args, "--uid", strconv.Itoa(u.UID))
	}
	if u.Group != "" {
		args = append(args, "--gid", u.Group)
	}
	if len(u.Groups) > 0 {
		args = append(args, "--groups", strings.Join(u.Groups, ","))
	}
	if u.NoPassword {
		args = append(args, "--password", "*")
	}
	for _, f := range [][2]string{{"--home-dir", u.Home}, {"--shell", u.Shell}, {"--comment", u.Comment}} {
		if f[1] != "" {
			args = append(args, f[0], f[1])
		}
	}
	return append(args, u.Name)
}

// shadowPassword is the password field of name's shadow entry.
func shadowPassword(ctx context.Context, x Exec, name string) (string, error) {
	o, err := run(ctx, x, Cmd{Name: "getent", Args: []string{"shadow", name}, Timeout: time.Minute})
	if err != nil {
		return "", fmt.Errorf("the shadow entry of %s: %w", name, err)
	}
	f := strings.Split(strings.TrimSpace(o.Out), ":")
	if len(f) < 2 || f[0] != name {
		return "", fmt.Errorf("getent shadow: %q is not a shadow entry", strings.TrimSpace(o.Out))
	}
	return f[1], nil
}

func (u *User) Apply(ctx context.Context, st State) (Result, error) {
	if _, err := run(ctx, u.Exec, st.Data.(Cmd)); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}
