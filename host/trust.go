package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Writable is a file, or a directory on the way to it, that a user other
// than root (and the trusted ones) can change: whoever can, can replace
// what a service reads or runs from there.
type Writable struct {
	Path string `json:"path"`
	// Why is how: "owned by uid 1000", "writable by group 1000",
	// "writable by everyone".
	Why string `json:"why"`
	// File is whether Path is the file itself, whose owner and mode can be
	// put right; else a directory above it, which only moving the file
	// out of puts right.
	File bool `json:"file,omitempty"`
}

func (w *Writable) Error() string { return w.Path + " is " + w.Why }

// Writers is the first of path and the directories above it that a user
// other than root and the trusted can change: one owned by a uid other
// than root's and theirs, writable by a group other than root's and
// theirs, or by everyone. A link on
// the way is checked where it is and where it leads. A path that does not
// exist yet is checked from the nearest directory that does. nil when
// only root and the trusted can change it, and where files have no
// numeric owner.
func Writers(path string, trusted ...Owner) (*Writable, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s: want an absolute path", path)
	}
	file := true
	for {
		_, err := os.Lstat(path)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil, err
		}
		path, file = parent, false
	}
	// Where it leads, then where it is: a link's own directory decides
	// who can swap the link.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if w, err := walkWriters(real, file, trusted); w != nil || err != nil || real == path {
		return w, err
	}
	return walkWriters(path, file, trusted)
}

// walkWriters checks path and each directory above it, as they are (a
// link is not followed).
func walkWriters(path string, file bool, trusted []Owner) (*Writable, error) {
	for {
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		o, ok := FileOwner(fi)
		if !ok {
			return nil, nil
		}
		link := fi.Mode()&fs.ModeSymlink != 0
		if why := writableBy(o, fi.Mode(), link, trusted); why != "" {
			return &Writable{Path: path, Why: why, File: file && !link}, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil, nil
		}
		path, file = parent, false
	}
}

// writableBy is who other than root and the trusted (a trusted uid, and
// its group: a user's own primary group) can change a path with owner o
// and mode m; "" when no one. A link's own mode means nothing: only its
// owner counts.
func writableBy(o Owner, m fs.FileMode, link bool, trusted []Owner) string {
	switch {
	case o.UID != 0 && !slices.ContainsFunc(trusted, func(t Owner) bool { return t.UID == o.UID }):
		return fmt.Sprintf("owned by uid %d", o.UID)
	case link:
		return ""
	case m.Perm()&0o002 != 0:
		return "writable by everyone"
	case m.Perm()&0o020 != 0 && o.GID != 0 && !slices.ContainsFunc(trusted, func(t Owner) bool { return t.GID == o.GID }):
		return fmt.Sprintf("writable by group %d", o.GID)
	}
	return ""
}
