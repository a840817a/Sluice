//go:build !unix

package store

import "io/fs"

// owner has no portable implementation off unix; the caller falls back to
// reporting the mode alone.
func owner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
