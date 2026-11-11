package logging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is --log-config's default: the manager's own
// config.yaml, whose top-level logging: section is the knob (the design's
// §6); the chart mounts the ConfigMap holding it as a directory, never a
// subPath, which would never receive its updates.
const DefaultConfigPath = DefaultConfigDir + "/config.yaml"

// DefaultConfigDir is the directory that ConfigMap is mounted at.
const DefaultConfigDir = "/etc/clusterplex"

// FileConfig is the config file's "logging:" section (design §4).
//
// The parser is gopkg.in/yaml.v3, the one this module already uses.
type FileConfig struct {
	Level      string            `yaml:"level,omitempty"`
	Format     string            `yaml:"format,omitempty"`
	AddSource  *bool             `yaml:"addSource,omitempty"`
	Subsystems map[string]string `yaml:"subsystems,omitempty"`
}

// ReadConfig reads path's "logging:" key. A missing file's error wraps
// fs.ErrNotExist, so a caller can tell it from a bad one.
func ReadConfig(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read logging config: %w", err)
	}
	return parseConfig(data)
}

// parseConfig reads the "logging:" key, strictly within the section and
// leaving the file's other top-level sections alone (cluster-plex's
// config.yaml carries its own).
func parseConfig(data []byte) (FileConfig, error) {
	var raw struct {
		Logging yaml.Node `yaml:"logging"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return FileConfig{}, fmt.Errorf("parse logging config: %w", err)
	}
	var f FileConfig
	if raw.Logging.Kind == 0 { // no logging: key
		return f, nil
	}
	section, err := yaml.Marshal(&raw.Logging)
	if err != nil {
		return FileConfig{}, fmt.Errorf("parse logging config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(section))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return FileConfig{}, fmt.Errorf("parse logging config: %w", err)
	}
	return f, nil
}

// Resolve settles the options a process runs with (design §4): for the
// level and the subsystems the file (when haveFile), then a flag that was
// set, then LOG_LEVEL; for the format and addSource the same order over
// --log-format, --log-add-source and LOG_FORMAT, read at start only. The
// file's subsystems replace the flags' whole. changed reports whether a
// flag was set on the command line; getenv reads the environment.
func Resolve(file FileConfig, haveFile bool, flags Options, changed func(string) bool, getenv func(string) string) (Options, error) {
	out := flags
	switch {
	case haveFile && file.Level != "":
		l, err := ParseLevel(file.Level)
		if err != nil {
			return out, fmt.Errorf("logging config level %q: %w", file.Level, err)
		}
		out.Level = l
	case changed("log-level"):
		out.Level = flags.Level
	case getenv("LOG_LEVEL") != "":
		l, err := ParseLevel(getenv("LOG_LEVEL"))
		if err != nil {
			return out, fmt.Errorf("LOG_LEVEL %q: %w", getenv("LOG_LEVEL"), err)
		}
		out.Level = l
	default:
		out.Level = slog.LevelInfo
	}

	switch {
	case haveFile && file.Format != "":
		out.Format = file.Format
	case changed("log-format") && flags.Format != "":
		out.Format = flags.Format
	case getenv("LOG_FORMAT") != "":
		out.Format = getenv("LOG_FORMAT")
	default:
		out.Format = "json"
	}
	if out.Format != "json" && out.Format != "text" {
		return out, fmt.Errorf("log format %q: want json or text", out.Format)
	}

	switch {
	case haveFile && file.AddSource != nil:
		out.AddSource = *file.AddSource
	case changed("log-add-source"):
		out.AddSource = flags.AddSource
	default:
		out.AddSource = false
	}

	if haveFile && file.Subsystems != nil {
		subs, err := ParseSubsystems(file.Subsystems)
		if err != nil {
			return out, err
		}
		out.Subsystems = subs
	}
	return out, nil
}

// ParseSubsystems reads a subsystem-to-level map as the file spells it.
func ParseSubsystems(in map[string]string) (map[string]slog.Level, error) {
	out := make(map[string]slog.Level, len(in))
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		l, err := ParseLevel(in[name])
		if err != nil {
			return nil, fmt.Errorf("logging config subsystem %q: level %q: %w", name, in[name], err)
		}
		out[strings.TrimSpace(name)] = l
	}
	return out, nil
}
