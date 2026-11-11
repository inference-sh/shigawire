package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SystemUser is a system user of setup's own, made with useradd when
// missing: its own group, a home made for it, no login shell. One of that
// name whose Comment is another is someone else's, and an error: it is
// never taken over.
type SystemUser struct {
	Exec                       Exec
	Name, Home, Comment, Shell string
	// HomeMode is the home's mode; the user owns it.
	HomeMode fs.FileMode
	// Root is where the host's files are ("" is /): the home is under it.
	Root string
}

func (u *SystemUser) ID() string { return "user " + u.Name }

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
	o := x.Run(ctx, Cmd{Name: "getent", Args: []string{"passwd", name}, Timeout: time.Minute})
	if o.Code == 2 { // getent: key not found
		return nil, nil
	}
	if o.Err != nil {
		return nil, fmt.Errorf("getent passwd %s: %w", name, o.Err)
	}
	return parsePasswd(o.Out)
}

func parsePasswd(line string) (*Passwd, error) {
	f := strings.Split(strings.TrimSpace(line), ":")
	if len(f) != 7 {
		return nil, fmt.Errorf("getent passwd: %q is not a passwd entry", strings.TrimSpace(line))
	}
	uid, err1 := strconv.Atoi(f[2])
	gid, err2 := strconv.Atoi(f[3])
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("getent passwd %s: %w", f[0], err)
	}
	return &Passwd{Name: f[0], UID: uid, GID: gid, Comment: f[4], Home: f[5], Shell: f[6]}, nil
}

func (u *SystemUser) home() string {
	if u.Root == "" {
		return u.Home
	}
	return filepath.Join(u.Root, u.Home)
}

// passwd is u's entry, an error for one that is not setup's.
func (u *SystemUser) passwd(ctx context.Context) (*Passwd, error) {
	p, err := LookupPasswd(ctx, u.Exec, u.Name)
	if err != nil || p == nil {
		return p, err
	}
	if p.Comment != u.Comment {
		return nil, fmt.Errorf("a user %s exists that setup did not make (its comment is %q, not %q): setup does not take it over; name another engine user with --user", u.Name, p.Comment, u.Comment)
	}
	if p.Home != u.Home {
		return nil, fmt.Errorf("user %s has the home %s, not %s", u.Name, p.Home, u.Home)
	}
	return p, nil
}

func (u *SystemUser) Check(ctx context.Context) (State, error) {
	p, err := u.passwd(ctx)
	if err != nil {
		return State{}, err
	}
	if p == nil {
		return driftData(userDrift{}, "create")
	}
	fi, err := os.Lstat(u.home())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return driftData(userDrift{p}, "make its home %s", u.Home)
	case err != nil:
		return State{}, err
	case !fi.IsDir():
		return State{}, fmt.Errorf("the home of %s, %s, is %s, not a directory", u.Name, u.Home, kind(fi))
	}
	if d := (attrs{u.HomeMode, &Owner{p.UID, p.GID}}).drift(fi); len(d) > 0 {
		return driftData(userDrift{p}, "home %s", strings.Join(d, "; "))
	}
	return inPlace()
}

// userDrift is what Check found: the user's entry, nil when it is to be
// made.
type userDrift struct{ p *Passwd }

func (u *SystemUser) Apply(ctx context.Context, st State) (Result, error) {
	d, ok := st.Data.(userDrift)
	p := d.p
	if !ok {
		var err error
		if p, err = u.passwd(ctx); err != nil {
			return Result{}, err
		}
	}
	if p == nil {
		var err error
		if _, err = run(ctx, u.Exec, Cmd{Name: "useradd", Args: []string{"--system", "--user-group", "--create-home",
			"--home-dir", u.Home, "--shell", u.Shell, "--comment", u.Comment, u.Name}, Timeout: time.Minute}); err != nil {
			return Result{}, err
		}
		if p, err = u.passwd(ctx); err != nil {
			return Result{}, err
		}
		if p == nil {
			return Result{}, fmt.Errorf("useradd ran, and there is no user %s", u.Name)
		}
	}
	// The home useradd made (or one it found there: --create-home leaves
	// an existing directory as it is), put right.
	home := u.home()
	if fi, err := os.Lstat(home); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.Mkdir(home, u.HomeMode); err != nil {
			return Result{}, err
		}
	} else if err != nil {
		return Result{}, err
	} else if !fi.IsDir() {
		return Result{}, fmt.Errorf("the home of %s, %s, is %s, not a directory", u.Name, u.Home, kind(fi))
	}
	if err := (attrs{u.HomeMode, &Owner{p.UID, p.GID}}).set(home); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}
