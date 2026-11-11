// Package plexlogs reads Plex Media Server's own log files into the
// contract's records (the 2026-11-10 structured-logging design §4): the
// files under <plex-dir>/Logs and PMS Plugin Logs, each a subsystem, each
// line a record carrying Plex's own time, level, thread and request id.
package plexlogs

import (
	"path"
	"regexp"
	"strings"
)

// Format is the layout a file's lines follow.
type Format int

const (
	// FormatPMS is the server's and its helpers' layout:
	// "Jan 02, 2006 15:04:05.000 [thread] LEVEL - [Req#id] message".
	FormatPMS Format = iota
	// FormatPlugin is the plugin framework's:
	// "2006-01-02 15:04:05,000 (thread) :  LEVEL (module:line) - message".
	FormatPlugin
)

// PluginDir is the directory under Logs holding the plugins' files.
const PluginDir = "PMS Plugin Logs"

// rotated matches a rotation index: "X.3.log" and "X.log.2".
var (
	rotatedBefore = regexp.MustCompile(`\.(\d+)\.log$`)
	rotatedAfter  = regexp.MustCompile(`\.log\.(\d+)$`)
)

// Stem is base without its rotation index: "X.3.log" and "X.log.2" are
// both "X.log", the live file's name, which the per-file metrics are
// labelled with so a label never carries an index.
func Stem(base string) string {
	base = rotatedBefore.ReplaceAllString(base, ".log")
	return rotatedAfter.ReplaceAllString(base, ".log")
}

// FileFor names the subsystem of a log file under Logs, given its path
// relative to Logs (a bare base name, or "PMS Plugin Logs/<bundle>.log"):
// the server's files are pms and its helpers theirs (pms.scanner,
// pms.tuner, pms.dlna, pms.transcoder); a plugin's file is pms.plugin with
// plugin its bundle id. A rotated file maps as its live name. ok is false
// for a file that is no log (a lock file, a crash dump).
func FileFor(rel string) (subsystem, plugin string, format Format, ok bool) {
	dir, base := path.Split(path.Clean(strings.ReplaceAll(rel, "\\", "/")))
	stem := Stem(base)
	if !strings.HasSuffix(stem, ".log") {
		return "", "", FormatPMS, false
	}
	switch strings.Trim(dir, "/") {
	case "":
	case PluginDir:
		return "pms.plugin", strings.TrimSuffix(stem, ".log"), FormatPlugin, true
	default:
		return "", "", FormatPMS, false
	}
	switch {
	case strings.HasPrefix(stem, "Plex Media Scanner"):
		return "pms.scanner", "", FormatPMS, true
	case stem == "Plex Tuner Service.log":
		return "pms.tuner", "", FormatPMS, true
	case stem == "Plex DLNA Server.log":
		return "pms.dlna", "", FormatPMS, true
	case strings.HasPrefix(stem, "Plex Transcoder"):
		return "pms.transcoder", "", FormatPMS, true
	case strings.HasPrefix(stem, "Plex "):
		return "pms", "", FormatPMS, true
	}
	return "", "", FormatPMS, false
}
