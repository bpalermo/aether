package manager

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"aethermesh.dev/common/log"
	"github.com/go-logr/logr"
)

// TestControllerRuntimeVerbosityIsCappedUnderDebug drives controller-runtime's
// logger exactly as --debug configures it (the shared handler at log.LevelTrace)
// and checks the logr V-level mapping: V(0) and V(1) pass, V(2)+ — the
// priority queue's V(5) "workqueue_items" dump above all (issue #1131) — is not
// even Enabled, so logState skips building it.
func TestControllerRuntimeVerbosityIsCappedUnderDebug(t *testing.T) {
	var out bytes.Buffer
	base := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: log.LevelTrace})
	crLog := logr.FromSlogHandler(controllerRuntimeHandler(base)).WithName("controller-runtime").WithValues("controller", "gamma")

	for v := 0; v <= 10; v++ {
		want := v <= ControllerRuntimeMaxVerbosity
		if got := crLog.V(v).Enabled(); got != want {
			t.Errorf("V(%d).Enabled() = %v, want %v", v, got, want)
		}
	}

	crLog.V(5).Info("workqueue_items", "items", []string{})
	crLog.V(5).Info("Reconcile successful")
	if strings.Contains(out.String(), "workqueue_items") || strings.Contains(out.String(), "Reconcile successful") {
		t.Fatalf("V(5) records must be dropped under --debug, got: %s", out.String())
	}
	crLog.V(1).Info("debug line kept")
	crLog.Info("info line kept")
	for _, want := range []string{"debug line kept", "info line kept"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in: %s", want, out.String())
		}
	}
}

// TestControllerRuntimeCapLeavesAetherDebugAlone checks the cap is local to the
// handler controller-runtime logs through: aether's own logger on the same base
// handler keeps emitting Trace records under --debug.
func TestControllerRuntimeCapLeavesAetherDebugAlone(t *testing.T) {
	var out bytes.Buffer
	base := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: log.LevelTrace})
	_ = logr.FromSlogHandler(controllerRuntimeHandler(base))

	aether := slog.New(base)
	aether.Log(context.Background(), log.LevelTrace, "aether trace line")
	aether.Debug("aether debug line")
	for _, want := range []string{"aether trace line", "aether debug line"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("aether's own %q must still be emitted, got: %s", want, out.String())
		}
	}
}

// TestControllerRuntimeCapWithoutDebug checks the production (Info) level is
// unaffected: V(1) stays off, Info and errors pass.
func TestControllerRuntimeCapWithoutDebug(t *testing.T) {
	var out bytes.Buffer
	base := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})
	crLog := logr.FromSlogHandler(controllerRuntimeHandler(base))
	if crLog.V(1).Enabled() {
		t.Fatal("V(1) must stay off at Info level")
	}
	if !crLog.V(0).Enabled() {
		t.Fatal("V(0) must stay on at Info level")
	}
}
