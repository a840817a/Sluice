package store

import (
	"fmt"
	"os"
)

// CheckWritable reports whether dataDir can actually be written to, creating it
// if it does not exist. It is meant to be called once at startup.
//
// It writes a file rather than consulting permission bits, because the failures
// worth catching are the ones the bits do not show. A container bind mount can
// be mode 0775 and still deny every write: the process's gid may not be the
// directory's group (an image that declares USER without a group gets its
// primary gid from its own /etc/passwd), the mount may be read-only, or SELinux
// may refuse a directory that was never relabelled. All three pass an
// access(2)-style check and then fail on first use.
//
// Creating a file and creating a subdirectory need the same bits on the parent,
// so one probe covers both the channel store's channels.json and the segment
// store's per-channel directories.
//
// Why at startup: nothing else fails early. /healthz never touches the data
// directory, so a gateway that cannot store a single byte still reports healthy
// and passes a container healthcheck; the first symptom is a segment write
// failing minutes or days later, in a log line far from the cause.
func CheckWritable(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("data directory %s cannot be created: %w%s", dataDir, err, ownership(dataDir))
	}

	f, err := os.CreateTemp(dataDir, ".writable-")
	if err != nil {
		return fmt.Errorf("data directory %s is not writable: %w%s", dataDir, err, ownership(dataDir))
	}
	name := f.Name()
	// Remove before reporting a close error: leaving the probe behind would put
	// a stray dotfile in a directory that is served over HTTP.
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("data directory %s: probe file %s could not be removed: %w", dataDir, name, err)
	}
	if closeErr != nil {
		return fmt.Errorf("data directory %s: probe file could not be closed: %w", dataDir, closeErr)
	}
	return nil
}

// ownership renders who owns dataDir and who this process is, as a parenthesised
// suffix for an error message. It is the part an operator cannot reconstruct
// from "permission denied" alone, and the reason this check exists at all: the
// mismatch between the two numbers is the answer nearly every time.
//
// Returns "" if the directory cannot be stat'ed — this decorates an error that
// is already being returned, and must not replace it with its own.
func ownership(dataDir string) string {
	fi, err := os.Stat(dataDir)
	if err != nil {
		return ""
	}
	uid, gid, ok := owner(fi)
	if !ok {
		return fmt.Sprintf(" (directory mode=%04o; process uid=%d gid=%d)",
			fi.Mode().Perm(), os.Geteuid(), os.Getegid())
	}
	return fmt.Sprintf(" (directory uid=%d gid=%d mode=%04o; process uid=%d gid=%d)",
		uid, gid, fi.Mode().Perm(), os.Geteuid(), os.Getegid())
}
