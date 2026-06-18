package log

import (
	"context"
	"log/slog"
)

// nameHandler injects exactly one "logger" attribute (the component name) into
// every record, delegating everything else to the wrapped handler. Named() unwraps
// and re-wraps it so re-naming dot-joins into a single name rather than stacking
// duplicate "logger" attributes.
type nameHandler struct {
	inner slog.Handler
	name  string
}

func (h *nameHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *nameHandler) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(slog.String(loggerKey, h.name))
	return h.inner.Handle(ctx, r)
}

func (h *nameHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &nameHandler{inner: h.inner.WithAttrs(attrs), name: h.name}
}

func (h *nameHandler) WithGroup(name string) slog.Handler {
	return &nameHandler{inner: h.inner.WithGroup(name), name: h.name}
}
