//go:build unix

package store

import (
	"io/fs"
	"syscall"
)

// owner extracts a directory's uid and gid. Split out by build tag because
// FileInfo.Sys() is platform-specific; the deployment targets are Linux, and
// development happens on macOS.
func owner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
