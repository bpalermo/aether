package hook

import (
	"context"
	"time"

	"github.com/go-logr/logr"
)

type ShutdownHook interface {
	Shutdown(ctx context.Context) error
}

func AddShutdownHook(ctx context.Context, timeout time.Duration, log logr.Logger, hooks ...ShutdownHook) {
	// Wait for context cancellation (from signal or parent context)
	<-ctx.Done()
	log.V(1).Info("context cancelled, shutting down")

	// Perform cleanup here if is needed to
	// Give some time for the graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), timeout)
	defer shutdownCancel()

	for _, hook := range hooks {
		if err := hook.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "error during shutdown hook")
		}
	}

}
