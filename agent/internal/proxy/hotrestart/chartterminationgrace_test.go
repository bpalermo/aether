package hotrestart

import (
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chartTerminationGraceFixed matches the one place the chart keeps the fixed
// part of the smallest termination grace period it renders.
var chartTerminationGraceFixed = regexp.MustCompile(
	`\{\{- define "aether\.proxy\.terminationGraceFixedSeconds" -\}\}([0-9]+)\{\{- end -\}\}`)

// TestChartTerminationGraceFloorMatchesTheSupervisor holds the chart's render
// check to this package's arithmetic. The chart refuses a
// proxy.terminationGracePeriodSeconds that is not greater than
// proxy.hotRestart.drainTime plus a fixed number of seconds, because below
// that successorWaitBudget and replacementFinishBudget apply no bound at all.
// That number is shutdownGrace + terminationFallbackMargin, written a second
// time in a template: changing either constant without the chart would let the
// chart render a grace period the supervisor cannot use, or refuse one it can.
func TestChartTerminationGraceFloorMatchesTheSupervisor(t *testing.T) {
	const helpers = "charts/aether/templates/_helpers.tpl"
	path := findRepoFile(t, helpers)
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)

	m := chartTerminationGraceFixed.FindAllSubmatch(raw, -1)
	require.Len(t, m, 1, "%s must define aether.proxy.terminationGraceFixedSeconds exactly once, as a bare integer", helpers)
	seconds, err := strconv.Atoi(string(m[0][1]))
	require.NoError(t, err)
	chartFixed := time.Duration(seconds) * time.Second

	require.Equal(t, shutdownGrace+terminationFallbackMargin, chartFixed,
		"%s: aether.proxy.terminationGraceFixedSeconds must equal shutdownGrace (%s) + terminationFallbackMargin (%s)",
		helpers, shutdownGrace, terminationFallbackMargin)

	// The chart's comparison is strict, in whole seconds. Show that this is
	// where the supervisor's bounds start to exist: none at the boundary, one
	// second of budget just above it.
	for _, drain := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 90 * time.Second} {
		budgets := func(grace time.Duration) (successorWait, replacementFinish time.Duration) {
			s := New(Config{TerminationGrace: grace, DrainTime: drain}, slog.New(slog.DiscardHandler), nil)
			return s.successorWaitBudget(), s.replacementFinishBudget()
		}
		uncapped := 2*drain + shutdownGrace + predecessorExitSettle + replacementLiveAllowance

		wait, finish := budgets(drain + chartFixed)
		assert.Zero(t, wait, "drainTime %s: at the grace period the chart refuses, the successor wait has no bound", drain)
		assert.Equal(t, uncapped, finish,
			"drainTime %s: at the grace period the chart refuses, a held termination is not capped", drain)

		wait, finish = budgets(drain + chartFixed + time.Second)
		assert.Equal(t, time.Second, wait,
			"drainTime %s: the smallest grace period the chart renders must bound the successor wait", drain)
		assert.Equal(t, time.Second, finish,
			"drainTime %s: the smallest grace period the chart renders must cap a held termination", drain)
	}
}
