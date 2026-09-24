package plexprefs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockSuffix names the lock file that sits beside Preferences.xml.
//
// A separate file, not Preferences.xml itself: the merge finishes by renaming
// a temporary file over it, so a lock held on Preferences.xml would be a lock
// on an inode that is no longer the file anyone else will open.
const lockSuffix = ".lock"

// lockPreferences takes an exclusive advisory lock covering a whole
// read-modify-write of Preferences.xml, and returns the function that releases
// it.
//
// Every pod runs Plex and merges into one file on shared storage as it starts
// (ADR-0004). The write is atomic, so the file cannot tear, but without this
// two pods that read the same original both merge onto that original and the
// second rename discards the first's settings. A rolling update starts pods
// together, so this is the ordinary case.
//
// flock is advisory and only binds processes that take it, which is all of
// them here. On NFS it is honoured from NFSv4 and on Linux NFSv3 via the lock
// manager; a filesystem that ignores it silently returns to losing updates,
// which is the same behaviour as before this existed rather than worse.
func lockPreferences(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open preferences lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock preferences: %w", err)
	}
	return func() {
		// Closing the descriptor releases the lock on its own; unlocking
		// first makes that explicit rather than incidental.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
