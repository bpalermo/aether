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
// reserved as "the supervisor passes it" that the supervisor does not pass,
// except the long spelling of a flag it passes by its short one. A stale entry
// would refuse an Envoy flag an operator is entitled to.
//
// An entry marked conflict (#1409) is the opposite case and is held to the
// opposite rule: the supervisor passes none of its spellings. So neither kind
// can be mislabelled as the other, and each error says the right thing ("Envoy
// refuses a flag given twice" is only true of a flag that is passed).
func TestReservedFlagsAreAllPassedOrAliases(t *testing.T) {
	s := New(Config{ConfigPath: "/etc/envoy/envoy.yaml"}, slog.New(slog.DiscardHandler), nil)
	passed := append(suppliedFlags(s.buildEnvoyCmd(0).Args[1:]), suppliedFlags(s.validateArgs())...)

	conflicts := 0
	for _, r := range reservedEnvoyFlags {
		require.NotEmpty(t, r.spellings)
		assert.NotEmpty(t, r.owner, "%v: the error must say what owns the flag, or why it is refused", r.spellings)
		used := false
		for _, spelling := range r.spellings {
			assert.True(t, strings.HasPrefix(spelling, "-"), spelling)
			used = used || slices.Contains(passed, spelling)
		}
		if r.conflict {
			conflicts++
			assert.False(t, used, "%v is marked conflict but the supervisor passes it: drop the mark", r.spellings)
			continue
		}
		assert.True(t, used, "%v is reserved but the supervisor passes none of its spellings", r.spellings)
	}
	// Control: the conflict branch above ran.
	assert.Positive(t, conflicts)
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
		{"--base-id-path", "/tmp/b", "--config-yaml", "{}"},
		// Envoy runs one worker for 0 (#1408), so it is a value like another.
		{"--concurrency", "0"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%v", args)
	}

	// Until #1443 "--admin-address-pathology" closed the third list above: a
	// flag that only starts like a reserved one was passed through. It is
	// still not reserved, but the pinned Envoy has no such flag and refuses it
	// on every fork, so it is refused here, as what it is.
	err := CheckExtraArgs([]string{"--base-id-path", "/tmp/b", "--config-yaml", "{}", "--admin-address-pathology"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the pinned Envoy has no flag --admin-address-pathology")
	assert.NotContains(t, err.Error(), "is reserved")

	// These two were in the allowed list above until #1407 and #1408: the
	// pinned Envoy refuses both on every fork.
	for _, args := range [][]string{
		{"--concurrency=2"},
		{"--concurrency"},
	} {
		assert.Error(t, CheckExtraArgs(args), "%v", args)
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

// TestCheckExtraArgsRefusesSpellingsEnvoyDoesNotParse is #1407. The pinned
// Envoy takes a flag and its value as two arguments and nothing else:
// "--flag=value", "-f=value" and "-fvalue" each answer "Couldn't find match for
// argument" (//agent/test/envoyargs runs the same items through the binary).
// The error has to show the two-item form, ready to paste.
func TestCheckExtraArgsRefusesSpellingsEnvoyDoesNotParse(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		twoItems string
	}{
		{[]string{"--concurrency=2"}, "--envoy-arg=--concurrency --envoy-arg=2"},
		{[]string{"-l", "info", "--concurrency=2"}, "--envoy-arg=--concurrency --envoy-arg=2"},
		{[]string{"--service-node=n1"}, "--envoy-arg=--service-node --envoy-arg=n1"},
		{[]string{"--log-level=debug"}, "--envoy-arg=--log-level --envoy-arg=debug"},
		{[]string{"-l=debug"}, "--envoy-arg=-l --envoy-arg=debug"},
		{[]string{"-ldebug"}, "--envoy-arg=-l --envoy-arg=debug"},
		{
			[]string{"--component-log-level=upstream:debug,config:trace"},
			"--envoy-arg=--component-log-level --envoy-arg=upstream:debug,config:trace",
		},
		// Only the first "=" splits: the value keeps the rest.
		{[]string{"--stats-tag=a:b=c"}, "--envoy-arg=--stats-tag --envoy-arg=a:b=c"},
		{[]string{"--log-path="}, "--envoy-arg=--log-path --envoy-arg=<value>"},
		// A flag that takes no value is not a match for Envoy either.
		{[]string{"--skip-hot-restart-parent-stats=true"}, "--envoy-arg=--skip-hot-restart-parent-stats"},
	} {
		err := CheckExtraArgs(tc.args)
		require.Error(t, err, "%v", tc.args)
		assert.Contains(t, err.Error(), tc.twoItems, "%v: the error must show the two-item form", tc.args)
		assert.Contains(t, err.Error(), "Couldn't find match for argument", "%v", tc.args)
	}

	// Until #1443 a flag the check had never heard of was told to use two
	// items as well. Envoy has no such flag in any spelling, so that advice
	// led to a second refusal; the error says there is no such flag.
	err := CheckExtraArgs([]string{"--some-future-flag=x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the pinned Envoy has no flag --some-future-flag")
	assert.Contains(t, err.Error(), "Couldn't find match for argument")
	assert.NotContains(t, err.Error(), "--envoy-arg=--some-future-flag --envoy-arg=x")

	// A flag that takes no value is told to go alone. "--envoy-arg=true" as a
	// second item would be refused by Envoy too.
	switches := unreservedSwitches()
	require.GreaterOrEqual(t, len(switches), 13, "the scan of the flag table for switches came back short")
	for _, flag := range switches {
		err := CheckExtraArgs([]string{flag + "=true"})
		require.Error(t, err, flag)
		assert.Contains(t, err.Error(), flag+" takes no value: pass it alone, as the one item --envoy-arg="+flag, flag)
		assert.NotContains(t, err.Error(), "--envoy-arg=true", flag)
		assert.NotContains(t, err.Error(), "two items", flag)
	}

	// Not a flag spelled with "=": a value that contains one, and a value that
	// only starts with a dash.
	for _, args := range [][]string{
		{"--stats-tag", "a:b=c"},
		{"--log-format", "[%Y] level=%l %v"},
		{"--config-yaml", "{admin: {}}"},
		{"--service-zone", ""},
		{"--log-format", "- %v"},
		{"--log-format", "-=-"},
		{"-l", "info"},
		{"--log-level", "info"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%v", args)
	}
}

// TestCheckExtraArgsConcurrencyValue is #1408: a lone --concurrency whose
// value Envoy refuses, or reads as something the supervisor cannot know, used
// to pass the startup check and surface at a handoff as "successor worker
// count unknown". Each is a startup error now, naming the chart value.
func TestCheckExtraArgsConcurrencyValue(t *testing.T) {
	for name, args := range map[string][]string{
		"no value":               {"-l", "info", "--concurrency"},
		"not a number":           {"--concurrency", "x"},
		"number with a suffix":   {"--concurrency", "2x"},
		"fraction":               {"--concurrency", "1.5"},
		"hexadecimal":            {"--concurrency", "0x2"},
		"trailing space":         {"--concurrency", "2 "},
		"above uint32":           {"--concurrency", "4294967296"},
		"value is a short flag":  {"--concurrency", "-l", "info"},
		"value is a long flag":   {"--concurrency", "--skip-hot-restart-parent-stats"},
		"negative":               {"--concurrency", "-1"},
		"empty":                  {"--concurrency", ""},
		"plus sign":              {"--concurrency", "+2"},
		"leading space":          {"--concurrency", " 2"},
		"more than can run":      {"--concurrency", "4294967295"},
		"bad value, flags after": {"--concurrency", "two", "--skip-hot-restart-parent-stats"},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckExtraArgs(args)
			require.Error(t, err, "%v must be refused at startup", args)
			assert.Contains(t, err.Error(), "--concurrency")
			assert.Contains(t, err.Error(), "proxy.concurrency", "the error must name the chart value that owns the flag")
			assert.NotErrorIs(t, err, errRepeatedConcurrency)
		})
	}

	for _, args := range [][]string{
		{"--concurrency", "1"},
		{"--concurrency", "2", "--skip-hot-restart-parent-stats"},
		{"--concurrency", "02"},
		{"--concurrency", "128"},
		// Envoy runs one worker for 0; concurrencyArg reports 1.
		{"--concurrency", "0"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%v", args)
	}
}

// TestCheckExtraArgsRefusesWhatBreaksAHandoff is #1409: flags the supervisor
// does not pass, so they are no repeat and Envoy starts, but with them the
// next handoff fails or Envoy does not serve. The error must say why, and must
// not claim the flag is given twice.
func TestCheckExtraArgsRefusesWhatBreaksAHandoff(t *testing.T) {
	for flag, why := range map[string]string{
		"--use-dynamic-base-id": "ignore the fixed --base-id",
		"--disable-hot-restart": "never contacts its predecessor",
		"--socket-path":         "hot-restart socket",
		"--hot-restart-version": "exits 0 without serving",
		"--version":             "exits 0 without serving",
		"-h":                    "exits 0 without serving",
		"--help":                "exits 0 without serving",
		"--":                    "ignores every argument after it",
		"--ignore_rest":         "ignores every argument after it",
	} {
		for _, args := range [][]string{{flag}, {"-l", "info", flag, "x"}, {flag, "--concurrency", "2"}} {
			err := CheckExtraArgs(args)
			require.Error(t, err, "%v must be refused at startup", args)
			assert.Contains(t, err.Error(), "--envoy-arg "+flag+" is reserved", "%v", args)
			assert.Contains(t, err.Error(), why, "%v: the error must say what the flag breaks", args)
			assert.NotContains(t, err.Error(), "given twice", "%v: the supervisor does not pass this flag", args)
		}
	}
	// The "=" spelling of one of them is told it is reserved, not how to
	// respell it.
	err := CheckExtraArgs([]string{"--socket-path=@x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is reserved")

	// Hot-restart flags that are left alone, and why:
	//   --base-id-path only writes the base id Envoy uses to a file (measured:
	//   the file held the fixed --base-id at epoch 0 and at epoch 1, and the
	//   handoff completed);
	//   --skip-hot-restart-parent-stats is the chart's
	//   proxy.hotRestart.skipParentStats (#1050);
	//   --skip-hot-restart-on-no-parent only changes what a child does when
	//   its parent is already gone;
	//   --socket-mode is not read while the socket path is the abstract
	//   default, which reserving --socket-path guarantees;
	//   --cpuset-threads is a no-op in the pinned Envoy (it logs "now the
	//   default behavior").
	for _, args := range [][]string{
		{"--base-id-path", "/tmp/base-id"},
		{"--skip-hot-restart-parent-stats"},
		{"--skip-hot-restart-on-no-parent"},
		{"--socket-mode", "600"},
		{"--cpuset-threads"},
		{"--drain-strategy", "immediate"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%v", args)
	}

	// Longer flags that only start like a reserved one are not reserved. Until
	// #1443 they were passed through; the pinned Envoy has none of them, so
	// each is refused as a flag Envoy does not have.
	for _, flag := range []string{"--socket-pathology", "--versioned", "--helpful", "--use-dynamic-base-ids"} {
		err := CheckExtraArgs([]string{flag})
		require.Error(t, err, flag)
		assert.Contains(t, err.Error(), "the pinned Envoy has no flag "+flag)
		assert.NotContains(t, err.Error(), "is reserved", flag)
	}
}

// unreservedSwitches returns the pinned Envoy's flags that take no value and
// that the check does not reserve: the ones an operator can pass.
func unreservedSwitches() []string {
	var out []string
	for i := range envoyFlags {
		if f := &envoyFlags[i]; !f.takesValue() && reservedEntry(f) == nil {
			out = append(out, f.Long)
		}
	}
	return out
}

// TestCheckExtraArgsAllowsTheChartFlagsOnce: the pinned Envoy refuses any flag
// given twice (measured: --skip-hot-restart-parent-stats, --drain-strategy, -l
// with --log-level; --stats-tag is the one repeatable flag). The chart passes
// each flag in onceEnvoyFlags itself, so one more of it fails every fork.
func TestCheckExtraArgsAllowsTheChartFlagsOnce(t *testing.T) {
	for _, o := range onceEnvoyFlags {
		require.NotEmpty(t, o.spellings)
		assert.NotEmpty(t, o.owner, "%v", o.spellings)
		first := o.spellings[0]
		last := o.spellings[len(o.spellings)-1]

		// One occurrence as Envoy takes it: with a value, or alone for a flag
		// that takes none (a "1" after such a flag is refused since #1443, as
		// Envoy refuses it).
		once := []string{first, "1"}
		if !envoyFlagNamed(first).takesValue() {
			once = []string{first}
		}
		assert.NoError(t, CheckExtraArgs(once), "one %s is allowed", first)
		for _, args := range [][]string{
			{first, "1", last, "1"},
			{first, "1", "--stats-tag", "a:b", last + "=1"},
		} {
			err := CheckExtraArgs(args)
			require.Error(t, err, "%v", args)
			assert.Contains(t, err.Error(), "more than once", "%v", args)
			assert.Contains(t, err.Error(), first, "%v", args)
			assert.Contains(t, err.Error(), "Pass it once; "+o.owner, "%v", args)
		}
	}

	// --stats-tag is repeatable in Envoy and is not in the list.
	assert.NoError(t, CheckExtraArgs([]string{"--stats-tag", "a:b", "--stats-tag", "c:d"}))
	// Until #1443 a repeat of a flag the chart does not pass was let through
	// here. Envoy refuses it like any other ("Argument already set!", measured
	// for --log-path), so it is refused, without a word about the chart.
	err := CheckExtraArgs([]string{"--log-path", "/dev/null", "--log-path", "/dev/null"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--log-path is given more than once")
}
