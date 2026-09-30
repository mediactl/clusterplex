package bootstrap

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
)

// PlexUID and PlexGID are the plex user the image's /etc/passwd defines, the
// same ids plexinc/pms-docker uses, so a volume written by one image is
// readable by the other.
const (
	PlexUID = 1000
	PlexGID = 1000
)

// plexDirs are created before Plex starts. Plex crashes with a
// boost::filesystem error when it scans for plug-ins or metadata in one that
// is missing.
var plexDirs = []string{"Plug-ins", "Metadata", "Cache", "Logs", "Crash Reports"}

// initialPreferences is written when Preferences.xml does not exist yet.
//
// OldestPreviousVersion describes the database, not the binary, so it does not
// follow the Plex version the Dockerfile pins. The library this server opens is
// the shim's dump, taken from Plex 1.43.0.10492; saying so is what lets a newer
// Plex decide which one-time fixups a 1.43.0-era database still needs. Raising
// it to match the running build would suppress those fixups rather than make
// them unnecessary. It moves only when the dump is re-taken.
const initialPreferences = `<?xml version="1.0" encoding="utf-8"?>
<Preferences OldestPreviousVersion="1.43.0.10492-121068a07" MachineIdentifier="%[1]s" ProcessedMachineIdentifier="%[1]s" AnonymousMachineIdentifier="%[1]s" AcceptedEULA="1" PublishServerOnPlexOnline="0"/>
`

// ensurePlexDirs creates the directories Plex expects and, if there is none
// yet, an initial Preferences.xml. Plex crashes with a boost::filesystem error
// without the file.
func ensurePlexDirs(log *slog.Logger, plexDir string) error {
	for _, name := range plexDirs {
		if err := os.MkdirAll(filepath.Join(plexDir, name), 0o755); err != nil {
			return err
		}
	}
	id, err := machineIdentifier()
	if err != nil {
		return err
	}
	path := filepath.Join(plexDir, "Preferences.xml")
	created, err := plexprefs.CreateIfAbsent(path, fmt.Appendf(nil, initialPreferences, id))
	if err != nil {
		return err
	}
	if created {
		log.Info("created initial Preferences.xml", "machine_identifier", id)
	}
	return nil
}

// machineIdentifier returns a new UUID in its hyphenated form.
//
// Hyphens included: upstream strips them, and Plex parses the identifier as a
// UUID and dies on the first plug-in that reads it ("std::domain_error:
// Invalid uuid length"). Upstream's fallback, "plex-pg-<time>", is no UUID
// at all, so the fallback here generates one instead.
func machineIdentifier() (string, error) {
	if b, err := os.ReadFile("/proc/sys/kernel/random/uuid"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

// ensureTempDir creates a world-writable, sticky directory, as /tmp is.
func ensureTempDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o777|os.ModeSticky)
	_ = os.Lchown(path, PlexUID, PlexGID)
	return nil
}

// cleanCrashReports empties Plex's crash report directory. A report left there
// makes Plex run CrashUploader, which is a no-op binary here but is better not
// started at all. Dotfiles stay, as the shell glob upstream uses leaves them.
func cleanCrashReports(log *slog.Logger, plexDir string) error {
	dir := filepath.Join(plexDir, "Crash Reports")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removed := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 {
		log.Info("cleaned crash reports", "removed", removed)
	}
	return nil
}

// chownTree gives everything under root to the plex user, changing only what
// is not already its, so a start after the first touches almost nothing on a
// large metadata tree. Symlinks are changed, not followed, as chown -R does.
// Failures are counted rather than returned, as upstream ignores them.
func chownTree(root string, uid, gid int) (changed, failed int) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			failed++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			failed++
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			failed++
			return nil
		}
		changed++
		return nil
	})
	return changed, failed
}

// checkWritable warns when Plex's state directory cannot be written, which
// otherwise surfaces much later as "attempt to write a readonly database".
func checkWritable(log *slog.Logger, dir string) {
	f, err := os.CreateTemp(dir, ".write_test_*")
	if err != nil {
		log.Warn("Plex's state directory is not writable; Plex will fail with 'attempt to write a readonly database'",
			"dir", dir, "error", err)
		return
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
}

func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
