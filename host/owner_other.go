//go:build !unix

package host

import "io/fs"

// FileOwner: files have no numeric owner here.
func FileOwner(fs.FileInfo) (Owner, bool) { return Owner{}, false }

// oNoFollow: no O_NOFOLLOW here.
const oNoFollow = 0
