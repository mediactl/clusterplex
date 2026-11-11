package logging

import (
	"context"
	"io"
	"log/slog"
)

// Service is the process's identity on every record (design §3): the
// product, the process within it, the build and the instance.
type Service struct {
	Namespace  string
	Name       string
	Version    string
	InstanceID string
}

// Handler writes the contract's record (design §3) and gates each line by
// its subsystem's level (design §4). It wraps slog's JSON or text handler:
// the inner handler is rebuilt as the base, then the service group, then the
// subsystem, then every WithAttrs and WithGroup in the order they were
// called, so a record reads time, level, msg, service, subsystem, the
// logger's attributes, the record's -- and a subsystem is one key however
// many times it is named, the innermost winning.
type Handler struct {
	base      slog.Handler
	levels    *Levels
	svc       Service
	subsystem string
	ops       []handlerOp
	inner     slog.Handler
}

// handlerOp is one WithAttrs (attrs set) or WithGroup (group set) to replay.
type handlerOp struct {
	group string
	attrs []slog.Attr
}

// NewHandler builds the root handler. format "text" selects slog's text
// handler; anything else its JSON handler, which every pod runs with.
func NewHandler(w io.Writer, format string, addSource bool, svc Service, levels *Levels) *Handler {
	opts := &slog.HandlerOptions{
		AddSource:   addSource,
		Level:       LevelTrace, // the gate is Enabled below, by subsystem
		ReplaceAttr: replaceAttr,
	}
	var base slog.Handler
	if format == "text" {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}
	h := &Handler{base: base, levels: levels, svc: svc}
	h.rebuild()
	return h
}

// replaceAttr names the level and puts the time in UTC.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(a.Key, LevelName(lvl))
		}
	case slog.TimeKey:
		if a.Value.Kind() == slog.KindTime {
			return slog.Time(a.Key, a.Value.Time().UTC())
		}
	}
	return a
}

func (h *Handler) rebuild() {
	inner := h.base
	if attrs := h.serviceAttrs(); len(attrs) != 0 {
		inner = inner.WithAttrs([]slog.Attr{{Key: "service", Value: slog.GroupValue(attrs...)}})
	}
	if h.subsystem != "" {
		inner = inner.WithAttrs([]slog.Attr{slog.String("subsystem", h.subsystem)})
	}
	for _, op := range h.ops {
		if op.group != "" {
			inner = inner.WithGroup(op.group)
		} else {
			inner = inner.WithAttrs(op.attrs)
		}
	}
	h.inner = inner
}

func (h *Handler) serviceAttrs() []slog.Attr {
	var attrs []slog.Attr
	for _, kv := range [...][2]string{
		{"namespace", h.svc.Namespace}, {"name", h.svc.Name}, {"version", h.svc.Version}, {"instance.id", h.svc.InstanceID},
	} {
		if kv[1] != "" {
			attrs = append(attrs, slog.String(kv[0], kv[1]))
		}
	}
	return attrs
}

func (h *Handler) clone() *Handler {
	c := *h
	c.ops = append([]handlerOp(nil), h.ops...)
	return &c
}

// WithSubsystem is this handler writing under name's level and key.
func (h *Handler) WithSubsystem(name string) *Handler {
	c := h.clone()
	c.subsystem = name
	c.rebuild()
	return c
}

// Subsystem is the name this handler writes under; "" for the root.
func (h *Handler) Subsystem() string { return h.subsystem }

// Levels is the process's levels this handler reads.
func (h *Handler) Levels() *Levels { return h.levels }

// Enabled reports whether level logs under this handler's subsystem.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.levels.For(h.subsystem)
}

// Handle writes r.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}

// WithAttrs appends attrs after the subsystem.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := h.clone()
	c.ops = append(c.ops, handlerOp{attrs: attrs})
	c.rebuild()
	return c
}

// WithGroup opens name for every later attribute.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.ops = append(c.ops, handlerOp{group: name})
	c.rebuild()
	return c
}
