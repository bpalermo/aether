package manager

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// TestLeaderElectionLostOnShutdownIsNotAnError drives the record exactly as
// controller-runtime does — through logr's slog bridge, error under "err" — and
// checks that only the expected shutdown record is demoted.
func TestLeaderElectionLostOnShutdownIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	base := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := logr.FromSlogHandler(controllerRuntimeHandler(base)).WithName("manager")

	log.Error(errors.New(leaderElectionLostError), stopSequenceErrorMessage)
	if got := out.String(); strings.Contains(got, `"level":"ERROR"`) || !strings.Contains(got, "leader election released on shutdown") {
		t.Fatalf("the expected shutdown record must be INFO, got: %s", got)
	}
	if !strings.Contains(out.String(), leaderElectionLostError) {
		t.Fatalf("the original error must be kept as an attribute, got: %s", out.String())
	}

	out.Reset()
	log.Error(errors.New("webhook server: address already in use"), stopSequenceErrorMessage)
	if !strings.Contains(out.String(), `"level":"ERROR"`) {
		t.Fatalf("a real failure during shutdown must stay ERROR, got: %s", out.String())
	}

	out.Reset()
	log.Error(errors.New(leaderElectionLostError), "problem running manager")
	if !strings.Contains(out.String(), `"level":"ERROR"`) {
		t.Fatalf("losing the lease outside shutdown must stay ERROR, got: %s", out.String())
	}
}
