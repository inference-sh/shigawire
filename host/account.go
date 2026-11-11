package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Passwd is a user's entry in the system's user database.
type Passwd struct {
	Name     string
	UID, GID int
	Comment  string
	Home     string
	Shell    string
}

// LookupPasswd is name's entry as getent passwd has it (NSS: /etc/passwd,
// LDAP), nil when there is none.
func LookupPasswd(ctx context.Context, x Exec, name string) (*Passwd, error) {
	f, err := getent(ctx, x, "passwd", name, 7)
	if f == nil || err != nil {
		return nil, err
	}
	uid, err1 := strconv.Atoi(f[2])
	gid, err2 := strconv.Atoi(f[3])
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("getent passwd %s: %w", f[0], err)
	}
	return &Passwd{Name: f[0], UID: uid, GID: gid, Comment: f[4], Home: f[5], Shell: f[6]}, nil
}

// LookupOwner is the owner and group a path gets for owner and group, by
// name or number, as the user database (getent) has them. An owner with
// no group gives the owner's primary group. A number with no entry is
// that uid or gid as it is (chown's rule: a container's uid has none),
// and an owner number with no entry and no group leaves the group as it
// is (GID -1). A name with no entry is an error. Owner "" is nil; a
// group with no owner is an error.
func LookupOwner(ctx context.Context, x Exec, owner, group string) (*Owner, error) {
	switch {
	case owner == "" && group != "":
		return nil, errors.New("group needs owner")
	case owner == "":
		return nil, nil
	}
	p, err := LookupPasswd(ctx, x, owner)
	if err != nil {
		return nil, err
	}
	own := &Owner{GID: -1}
	switch id, ok := numericID(owner); {
	case p != nil:
		own.UID, own.GID = p.UID, p.GID
	case ok:
		own.UID = id
	default:
		return nil, fmt.Errorf("there is no user %s", owner)
	}
	if group == "" {
		return own, nil
	}
	g, err := LookupGroup(ctx, x, group)
	if err != nil {
		return nil, err
	}
	switch id, ok := numericID(group); {
	case g != nil:
		own.GID = g.GID
	case ok:
		own.GID = id
	default:
		return nil, fmt.Errorf("there is no group %s", group)
	}
	return own, nil
}

// numericID is s as a uid or gid: digits only, below 2^32-1 (-1 as a
// 32-bit id means none).
func numericID(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 1<<32-1 {
		return 0, false
	}
	return int(n), true
}

// getent is key's entry in the database db (passwd, group, shadow) as
// getent has it, split at its colons: an entry without that many fields
// is an error. It is nil when there is none.
func getent(ctx context.Context, x Exec, db, key string, fields int) ([]string, error) {
	o := x.Run(ctx, short("getent", db, key))
	if o.Code == 2 { // getent: key not found
		return nil, nil
	}
	if o.Err != nil {
		return nil, fmt.Errorf("getent %s %s: %w", db, key, o.Err)
	}
	line := strings.TrimSpace(o.Out)
	f := strings.Split(line, ":")
	if len(f) != fields {
		return nil, fmt.Errorf("getent %s: %q is not a %s entry", db, line, db)
	}
	return f, nil
}

// GroupEntry is a group's entry in the system's group database.
type GroupEntry struct {
	Name    string
	GID     int
	Members []string
}

// LookupGroup is name's entry as getent group has it (NSS: /etc/group,
// LDAP), nil when there is none. A number names a group by its GID.
func LookupGroup(ctx context.Context, x Exec, name string) (*GroupEntry, error) {
	f, err := getent(ctx, x, "group", name, 4)
	if f == nil || err != nil {
		return nil, err
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
		return driftData([]Cmd{short("groupdel", g.Name)}, "remove")
	case e == nil:
		args := []string{}
		if g.System {
			args = append(args, "--system")
		}
		if g.GID != 0 {
			args = append(args, "--gid", strconv.Itoa(g.GID))
		}
		return driftData([]Cmd{short("groupadd", append(args, g.Name)...)}, "create")
	case g.GID != 0 && e.GID != g.GID:
		return driftData([]Cmd{short("groupmod", "--gid", strconv.Itoa(g.GID), g.Name)}, "gid %d, want %d", e.GID, g.GID)
	}
	return inPlace()
}

func (g *Group) Apply(ctx context.Context, st State) (Result, error) { return runCmds(ctx, g.Exec, st) }

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
		return driftData([]Cmd{short("userdel", u.Name)}, "remove")
	case p == nil:
		return driftData([]Cmd{short("useradd", u.addArgs()...)}, "create")
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
		o, err := run(ctx, u.Exec, short("id", "-nG", u.Name))
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
	return driftData([]Cmd{short("usermod", append(args, u.Name)...)}, "%s", strings.Join(d, "; "))
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
	f, err := getent(ctx, x, "shadow", name, 9)
	switch {
	case err != nil:
		return "", err
	case f == nil:
		return "", fmt.Errorf("there is no shadow entry for %s", name)
	}
	return f[1], nil
}

func (u *User) Apply(ctx context.Context, st State) (Result, error) { return runCmds(ctx, u.Exec, st) }
