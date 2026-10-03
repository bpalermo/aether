package manager

import (
	"context"
	"log/slog"
)

// ControllerRuntimeMaxVerbosity is the most verbose logr V-level controller-runtime
// (and anything else logging through ctrl.Log) may emit, whatever the component's
// own slog level.
//
// logr's slog bridge maps V(n) to slog level -n, and --debug lowers the shared
// handler to log.LevelTrace (-8) for aether's own debug output — which, without a
// cap, also switches on everything controller-runtime logs at V(2)..V(8). Its
// V(5) is the expensive one (issue #1131): every priority queue's logState
// goroutine, ticking every 10 s, then locks the queue, copies every item and
// serialises a "workqueue_items" record (900 lines per agent per 30 min, almost
// all "items=[]"), and every reconcile logs "Reconciling"/"Reconcile successful".
// V(1) keeps controller-runtime's own debug lines while dropping that per-item
// tracing; aether's slog records do not pass through this cap.
const ControllerRuntimeMaxVerbosity = 1

// verbosityCapHandler drops every record more verbose than floor. It sits in front
// of the handler controller-runtime logs through, so Enabled answers false for a
// capped V-level and callers (logState included) skip building the record at all.
type verbosityCapHandler struct {
	slog.Handler
	floor slog.Level
}

// capVerbosity wraps h so that nothing below logr V(maxV) reaches it.
func capVerbosity(h slog.Handler, maxV int) slog.Handler {
	return verbosityCapHandler{Handler: h, floor: slog.Level(-maxV)}
}

func (h verbosityCapHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.floor && h.Handler.Enabled(ctx, level)
}

func (h verbosityCapHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.floor {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h verbosityCapHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return verbosityCapHandler{Handler: h.Handler.WithAttrs(attrs), floor: h.floor}
}

func (h verbosityCapHandler) WithGroup(name string) slog.Handler {
	return verbosityCapHandler{Handler: h.Handler.WithGroup(name), floor: h.floor}
}
