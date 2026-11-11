package host

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
)

// Owner is a file's numeric owner and group. A resource's GID -1 leaves
// the group as it is (chown with an owner and no group): a new path gets
// the writer's.
type Owner struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

func (o Owner) String() string {
	if o.GID < 0 {
		return strconv.Itoa(o.UID)
	}
	return fmt.Sprintf("%d:%d", o.UID, o.GID)
}

// has reports whether got, a path's owner, is o's: the group too, unless
// o leaves it (GID -1).
func (o Owner) has(got Owner) bool {
	return got.UID == o.UID && (o.GID < 0 || got.GID == o.GID)
}

// UserOwner is u and u's primary group.
func UserOwner(u *user.User) (Owner, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Owner{}, fmt.Errorf("user %s: uid %q", u.Username, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Owner{}, fmt.Errorf("user %s: gid %q", u.Username, u.Gid)
	}
	return Owner{UID: uid, GID: gid}, nil
}

// attrs are a path's mode and owner as a resource wants them, by one rule
// for every resource: mode 0 leaves an existing path's (a new one gets its
// kind's default), owner nil leaves its owner (a new one gets the
// writer's).
type attrs struct {
	mode  fs.FileMode
	owner *Owner
}

// drift is how fi differs.
func (a attrs) drift(fi fs.FileInfo) []string {
	var d []string
	if got := fi.Mode().Perm(); a.mode != 0 && got != a.mode.Perm() {
		d = append(d, fmt.Sprintf("mode %04o, want %04o", got, a.mode.Perm()))
	}
	if got, ok := FileOwner(fi); a.owner != nil && ok && !a.owner.has(got) {
		if a.owner.GID < 0 {
			got.GID = -1
		}
		d = append(d, "owner "+got.String()+", want "+a.owner.String())
	}
	return d
}

// set puts the mode and owner on path, in place; a link's own owner (a
// mode is never set on a link: chmod follows it).
func (a attrs) set(path string) error {
	if a.mode != 0 {
		if err := os.Chmod(path, a.mode); err != nil {
			return err
		}
	}
	if a.owner != nil {
		return os.Lchown(path, a.owner.UID, a.owner.GID)
	}
	return nil
}

// forNew is the mode and owner a file replacing fi (nil: none there) is
// written with: the wanted ones, else fi's, else def and the writer's.
func (a attrs) forNew(fi fs.FileInfo, def fs.FileMode) (fs.FileMode, *Owner) {
	mode, owner := a.mode, a.owner
	if fi != nil && fi.Mode().IsRegular() {
		if mode == 0 {
			mode = fi.Mode().Perm()
		}
		if o, ok := FileOwner(fi); ok && owner == nil {
			owner = &o
		} else if ok && owner.GID < 0 {
			owner = &Owner{UID: owner.UID, GID: o.GID}
		}
	}
	if mode == 0 {
		mode = def
	}
	return mode, owner
}
