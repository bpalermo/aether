package manager

import (
	"context"
	"log/slog"
)

// The record controller-runtime's manager emits when a runnable reports an error
// after the stop sequence began, and the one error that is expected there.
const (
	stopSequenceErrorMessage = "error received after stop sequence was engaged"
	leaderElectionLostError  = "leader election lost"
)

// shutdownNoiseHandler demotes exactly one controller-runtime record from ERROR
// to INFO: "error received after stop sequence was engaged" carrying "leader
// election lost".
//
// That is what every leader-elected component (controller, registrar) logs on a
// clean SIGTERM: the manager cancels the leader-election context as part of its
// own shutdown, the elector reports that it stopped leading, and the manager —
// already stopping — logs it at ERROR. Nothing was lost; the lease is released
// on purpose. It was the last ERROR line left on a healthy roll once the designed
// retries of #766 went to WARN, which made "zero ERROR lines" unusable as a
// deploy bar for one line per rollout. Any other error arriving after the stop
// sequence — a runnable that really failed while shutting down — stays an ERROR.
type shutdownNoiseHandler struct {
	slog.Handler
}

// controllerRuntimeHandler wraps the handler controller-runtime logs through.
func controllerRuntimeHandler(h slog.Handler) slog.Handler {
	return shutdownNoiseHandler{Handler: h}
}

func (h shutdownNoiseHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && r.Message == stopSequenceErrorMessage && carriesError(r, leaderElectionLostError) {
		demoted := slog.NewRecord(r.Time, slog.LevelInfo, "leader election released on shutdown", r.PC)
		r.Attrs(func(a slog.Attr) bool {
			demoted.AddAttrs(a)
			return true
		})
		return h.Handler.Handle(ctx, demoted)
	}
	return h.Handler.Handle(ctx, r)
}

func (h shutdownNoiseHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return shutdownNoiseHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h shutdownNoiseHandler) WithGroup(name string) slog.Handler {
	return shutdownNoiseHandler{Handler: h.Handler.WithGroup(name)}
}

// carriesError reports whether any attribute of r is an error (or string) whose
// text is exactly want. logr's slog bridge attaches the error as "err".
func carriesError(r slog.Record, want string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		switch v := a.Value.Resolve().Any().(type) {
		case error:
			found = v.Error() == want
		case string:
			found = v == want
		}
		return !found
	})
	return found
}
