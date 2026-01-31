package hook

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

type ShutdownHook interface {
	Shutdown(ctx context.Context) error
}

func AddShutdownHook(ctx context.Context, timeout time.Duration, log logr.Logger, hooks ...ShutdownHook) {
	// Create a context that will be canceled on SIGTERM/SIGINT
	sigCtx, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Wait for context cancellation (from signal or parent context)
	<-sigCtx.Done()

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
