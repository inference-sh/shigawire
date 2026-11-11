//go:build unix

package host

import (
	"io/fs"
	"syscall"
)

// FileOwner is fi's owner, where the platform has one.
func FileOwner(fi fs.FileInfo) (Owner, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Owner{}, false
	}
	return Owner{UID: int(st.Uid), GID: int(st.Gid)}, true
}

// oNoFollow opens a path that is not a link, and fails on one.
const oNoFollow = syscall.O_NOFOLLOW
