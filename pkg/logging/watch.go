package logging

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultPollInterval is how often WatchFile looks at the config file. A
// mounted ConfigMap's symlink swap lands within the kubelet's sync period,
// about a minute, so ten seconds adds little to it.
const DefaultPollInterval = 10 * time.Second

// fileMark is what a stat says about the file; a change in any part means
// the file is read again.
type fileMark struct {
	size    int64
	modTime time.Time
	target  string
}

// WatchFile re-reads path's "logging:" section whenever its stat or its
// symlink target changes and applies the level and subsystems it resolves
// to through levels.Set (design §4). A file that does not parse, or whose
// levels do not, keeps the last good settings and is said once per bad
// content at WARN under the logging subsystem; a file that goes missing
// after it was read keeps them too. A file that was never there is no
// error. It returns when ctx ends.
func WatchFile(ctx context.Context, path string, levels *Levels, flags Options, changed func(string) bool, getenv func(string) string, every time.Duration) {
	if every <= 0 {
		every = DefaultPollInterval
	}
	log := SubsystemLogger(FromContext(ctx), "logging")
	var (
		last        fileMark
		applied     [32]byte
		everApplied bool
		badHash     [32]byte
		bad         bool
		missingSaid bool
	)
	check := func() {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if everApplied && !missingSaid {
					log.Warn("logging config file gone; keeping the last settings", "path", path)
					missingSaid = true
				}
				return
			}
			if !bad {
				log.Warn("logging config file unreadable; keeping the last settings", "path", path, "err", err)
				bad = true
			}
			return
		}
		missingSaid = false
		target, _ := filepath.EvalSymlinks(path)
		mark := fileMark{size: info.Size(), modTime: info.ModTime(), target: target}
		if mark == last {
			return
		}
		last = mark
		data, err := os.ReadFile(path)
		if err != nil {
			if !bad {
				log.Warn("logging config file unreadable; keeping the last settings", "path", path, "err", err)
				bad = true
			}
			return
		}
		sum := sha256.Sum256(data)
		if everApplied && sum == applied {
			return
		}
		fc, err := parseConfig(data)
		var opts Options
		if err == nil {
			opts, err = Resolve(fc, true, flags, changed, getenv)
		}
		if err != nil {
			if !bad || sum != badHash {
				log.Warn("logging config file rejected; keeping the last settings", "path", path, "err", err)
				bad, badHash = true, sum
			}
			return
		}
		bad = false
		levels.Set(opts.Level, opts.Subsystems)
		applied, everApplied = sum, true
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
