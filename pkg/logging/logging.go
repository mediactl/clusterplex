// Package logging is Clustarr's *slog.Logger, carried through context.
//
// There is no package-level logger, no global default and no logger field on
// any struct (see the amendment, §A2.1, and CLAUDE.md's logging invariant):
// the logger travels in the context, and handlers and reconcilers enrich it
// once at entry with [With] so every line logged below carries the request's
// identity without threading an extra parameter through every call.
//
// A service builds its root logger once at startup with [New], installs it
// on the base context with [NewContext], and bridges it to controller-runtime
// with [LogrBridge] for ctrl.SetLogger so controller-runtime's own output
// joins the same stream in the same format.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

// Options configures the logger [New] builds.
type Options struct {
	// Level is the minimum level the logger emits. The zero value is
	// slog.LevelInfo.
	Level slog.Level

	// Format selects the handler. "text" selects slog.NewTextHandler for
	// output meant to be read directly, such as a developer's terminal.
	// Anything else, including the zero value, selects slog.NewJSONHandler,
	// which every Clustarr Deployment runs with in-cluster so log lines are
	// structured for collection.
	Format string

	// AddSource adds the file and line of the log call to every record.
	AddSource bool

	// Output is where the logger writes. Nil defaults to os.Stderr.
	Output io.Writer

	// Subsystems are the per-subsystem level overrides, by dotted prefix
	// (design §4); Level is the floor under them.
	Subsystems map[string]slog.Level

	// ConfigPath is the logging config file (its "logging:" key), re-read
	// while the process runs. Empty means none.
	ConfigPath string

	// Service is stamped on every record as the service group.
	Service Service

	// flags is the set BindFlags bound this Options to, so Changed can say
	// which flags the command line set.
	flags *pflag.FlagSet
}

// Changed reports whether the command line set the named flag; false when
// no flags were bound.
func (o Options) Changed(name string) bool {
	return o.flags != nil && o.flags.Changed(name)
}

// New builds a root *slog.Logger from opts: a [Handler] over slog's JSON or
// text handler, gating each line by its subsystem's level.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	return slog.New(NewHandler(out, opts.Format, opts.AddSource, opts.Service, NewLevels(opts.Level, opts.Subsystems)))
}

// BindFlags registers the flags that configure opts on fs, using opts'
// current field values as each flag's default.
//
// A command builds its default Options first, then calls BindFlags so cobra
// fills opts in place during flag parsing -- the same pattern
// cmd/clustarr/flags.go's bindCommonFlags uses for k8s.Options.
func BindFlags(fs *pflag.FlagSet, opts *Options) {
	fs.Var((*levelValue)(&opts.Level), "log-level",
		"Minimum log level: debug, info, warn, or error.")
	fs.StringVar(&opts.Format, "log-format", opts.Format,
		`Log encoding: "json" or "text".`)
	fs.BoolVar(&opts.AddSource, "log-add-source", opts.AddSource,
		"Add the source file and line to every log record.")
	fs.Var((*subsystemLevels)(&opts.Subsystems), "log-subsystem-level",
		"A subsystem's level, name=level, repeatable; covers every subsystem below it by dotted prefix.")
	if opts.ConfigPath == "" {
		opts.ConfigPath = DefaultConfigPath
	}
	fs.StringVar(&opts.ConfigPath, "log-config", opts.ConfigPath,
		"The logging config file, whose logging: section is re-read while the process runs. A missing file is not an error.")
	opts.flags = fs
}

// subsystemLevels adapts a *map[string]slog.Level to pflag.Value for
// --log-subsystem-level name=level, repeatable.
type subsystemLevels map[string]slog.Level

func (s *subsystemLevels) String() string {
	names := make([]string, 0, len(*s))
	for name := range *s {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+LevelName((*s)[name]))
	}
	return strings.Join(parts, ",")
}

func (s *subsystemLevels) Set(v string) error {
	name, level, ok := strings.Cut(v, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return fmt.Errorf("--log-subsystem-level %q: want name=level", v)
	}
	l, err := ParseLevel(level)
	if err != nil {
		return fmt.Errorf("--log-subsystem-level %q: %w", v, err)
	}
	if *s == nil {
		*s = map[string]slog.Level{}
	}
	(*s)[name] = l
	return nil
}

func (s *subsystemLevels) Type() string { return "name=level" }

// levelValue adapts *slog.Level to pflag.Value by way of slog's own text
// codec, so --log-level accepts the same case-insensitive spellings
// (DEBUG, info, WARN+4, ...) that slog.Level.UnmarshalText accepts
// everywhere else in the stack.
type levelValue slog.Level

func (l *levelValue) String() string {
	return (*slog.Level)(l).String()
}

func (l *levelValue) Set(s string) error {
	return (*slog.Level)(l).UnmarshalText([]byte(s))
}

func (l *levelValue) Type() string {
	return "level"
}
