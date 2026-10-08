package hotrestart

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suppliedFlags returns the flags in a command line the supervisor built with
// no ExtraArgs: every argument that starts with a dash.
func suppliedFlags(args []string) []string {
	var flags []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		}
	}
	return flags
}

// TestEveryFlagTheSupervisorPassesIsReserved is the drift guard of #1376: it
// reads the flags off the command lines the supervisor really builds (a serving
// Envoy and a `--mode validate` run) and requires CheckExtraArgs to refuse each
// one in ExtraArgs, in both spellings. A flag added to buildEnvoyCmd or
// validateConfig without a reservedEnvoyFlags entry fails here, instead of
// failing every fork in a cluster whose operator repeated it.
func TestEveryFlagTheSupervisorPassesIsReserved(t *testing.T) {
	s := New(Config{
		EnvoyPath:          "/usr/local/bin/envoy",
		ConfigPath:         "/etc/envoy/envoy.yaml",
		BaseID:             7,
		DrainTime:          45 * time.Second,
		ParentShutdownTime: 60 * time.Second,
	}, slog.New(slog.DiscardHandler), nil)

	serve := suppliedFlags(s.buildEnvoyCmd(3).Args[1:])
	validate := suppliedFlags(s.validateArgs())
	// Control: the scan found the flags. A supervisor that stopped passing
	// these would make the loop below vacuous.
	require.Equal(t, []string{
		"-c", "--base-id", "--restart-epoch", "--drain-time-s", "--parent-shutdown-time-s", "--admin-address-path",
	}, serve, "the serving command line changed: reserve any new flag in reservedEnvoyFlags, then update this list")
	require.Equal(t, []string{"--mode", "-c"}, validate,
		"the validate command line changed: reserve any new flag in reservedEnvoyFlags, then update this list")

	for _, flag := range append(serve, validate...) {
		for _, args := range [][]string{{flag, "x"}, {flag + "=x"}, {"-l", "info", flag, "x"}} {
			err := CheckExtraArgs(args)
			require.Error(t, err, "the supervisor passes %s itself, so ExtraArgs %v must be refused", flag, args)
			assert.Contains(t, err.Error(), flag)
		}
	}
}

// TestReservedFlagsAreAllPassedOrAliases is the other direction: nothing is
// reserved that the supervisor does not pass, except the long spelling of a
// flag it passes by its short one. A stale entry would refuse an Envoy flag an
// operator is entitled to.
func TestReservedFlagsAreAllPassedOrAliases(t *testing.T) {
	s := New(Config{ConfigPath: "/etc/envoy/envoy.yaml"}, slog.New(slog.DiscardHandler), nil)
	passed := append(suppliedFlags(s.buildEnvoyCmd(0).Args[1:]), suppliedFlags(s.validateArgs())...)

	for _, r := range reservedEnvoyFlags {
		require.NotEmpty(t, r.spellings)
		assert.NotEmpty(t, r.owner, "%v: the error must say what owns the flag", r.spellings)
		used := false
		for _, spelling := range r.spellings {
			assert.True(t, strings.HasPrefix(spelling, "-"), spelling)
			used = used || slices.Contains(passed, spelling)
		}
		assert.True(t, used, "%v is reserved but the supervisor passes none of its spellings", r.spellings)
	}
}

// TestCheckExtraArgs covers what is allowed: the chart's own extra arguments,
// one --concurrency, and flags that only start like a reserved one.
func TestCheckExtraArgs(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{
			"-l", "info", "--service-cluster", "aether-proxy", "--service-node", "n1", "--service-zone", "z",
			"--drain-strategy", "immediate", "--concurrency", "2", "--skip-hot-restart-parent-stats",
		},
		{"--concurrency=2"},
		{"--base-id-path", "/tmp/b", "--config-yaml", "{}", "--admin-address-pathology"},
		// A value problem is not this check's: it only refuses a repeat.
		{"--concurrency", "0"},
		{"--concurrency"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%v", args)
	}

	for _, args := range [][]string{
		{"--concurrency", "2", "--concurrency", "2"},
		{"--concurrency=2", "-l", "info", "--concurrency", "4"},
		{"--concurrency", "x", "--concurrency=4"},
	} {
		err := CheckExtraArgs(args)
		require.Error(t, err, "%v", args)
		assert.ErrorIs(t, err, errRepeatedConcurrency, "%v", args)
		assert.Contains(t, err.Error(), "proxy.concurrency")
	}
}
