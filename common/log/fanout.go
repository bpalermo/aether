package log

import (
	"context"
	"errors"
	"log/slog"
)

// fanout is a slog.Handler that dispatches each record to several child handlers
// (e.g. stderr JSON + the otelslog OTLP bridge). Each child applies its own level
// gate in Handle, so a sink can drop records the others keep.
type fanout struct {
	handlers []slog.Handler
}

func newFanout(handlers ...slog.Handler) *fanout {
	return &fanout{handlers: handlers}
}

// Enabled reports true if any child would handle a record at this level, so the
// logger builds the record when at least one sink wants it.
func (f *fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle dispatches to every child that accepts this record's level. The record
// is cloned per child because slog.Record carries mutable shared state.
func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanout{handlers: hs}
}

func (f *fanout) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanout{handlers: hs}
}

// leveled wraps a handler with a minimum level, so a sink that does not filter by
// level itself (the otelslog bridge) honours the same threshold as stderr.
type leveled struct {
	handler slog.Handler
	level   slog.Level
}

func (l leveled) Enabled(_ context.Context, level slog.Level) bool {
	return level >= l.level
}

func (l leveled) Handle(ctx context.Context, r slog.Record) error {
	return l.handler.Handle(ctx, r)
}

func (l leveled) WithAttrs(attrs []slog.Attr) slog.Handler {
	return leveled{handler: l.handler.WithAttrs(attrs), level: l.level}
}

func (l leveled) WithGroup(name string) slog.Handler {
	return leveled{handler: l.handler.WithGroup(name), level: l.level}
}
