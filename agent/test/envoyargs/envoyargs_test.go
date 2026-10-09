// Package envoyargs_test holds the supervisor's --envoy-arg check
// (hotrestart.CheckExtraArgs) against the pinned Envoy's own command-line
// parser (issues #1407, #1408, #1409).
//
// The check refuses at startup what Envoy would refuse at every fork. What
// Envoy refuses was measured, and a measurement goes stale at the next pin
// bump: these tests run the same arguments through the binary the mesh
// deploys, so a parser that changes (say, one that starts to accept
// --flag=value) fails here instead of leaving a rule that refuses a working
// command line, or accepts a broken one.
package envoyargs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"aethermesh.dev/agent/test/envoybin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalBootstrap is enough for Envoy to validate and to serve its admin on a
// port of the kernel's choosing.
const minimalBootstrap = `admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 0 }
static_resources: {}
`

// envoyRunTimeout bounds one Envoy run. A validate run takes well under a
// second.
const envoyRunTimeout = 30 * time.Second

func pinnedEnvoy(t *testing.T) string {
	t.Helper()
	bin, err := envoybin.Path()
	var unsupported *envoybin.ErrUnsupportedArch
	if errors.As(err, &unsupported) {
		t.Skipf("skipping: %v", err)
	}
	require.NoError(t, err)
	return bin
}

func writeBootstrap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "envoy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(minimalBootstrap), 0o600))
	return path
}

// validate runs `envoy --mode validate -c <bootstrap> <extra...>`, the command
// line the supervisor's own bootstrap check builds, and reports whether Envoy
// accepted it and what it printed.
func validate(t *testing.T, envoy, bootstrap string, extra ...string) (accepted bool, output string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), envoyRunTimeout)
	defer cancel()
	args := append([]string{"--mode", "validate", "-c", bootstrap}, extra...)
	out, err := exec.CommandContext(ctx, envoy, args...).CombinedOutput()
	require.NoError(t, ctx.Err(), "envoy %v did not finish in %s", args, envoyRunTimeout)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		require.NoError(t, err, "running envoy %v", args)
	}
	return err == nil, string(out)
}

// TestEnvoyRefusesWhatTheCheckRefuses: for each argument list, the pinned Envoy
// rejects the command line and so does the startup check. The Envoy message is
// pinned too, because the check's errors quote it.
func TestEnvoyRefusesWhatTheCheckRefuses(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	const (
		noMatch    = "Couldn't find match for argument"
		alreadySet = "Argument already set!"
		badValue   = "Couldn't read argument value from string"
		noValue    = "Missing a value for this argument!"
		twoValues  = "More than one valid value parsed from string"
	)
	for _, tc := range []struct {
		args  []string
		envoy string
	}{
		// #1407: a flag and its value in one argument.
		{[]string{"--concurrency=2"}, noMatch},
		{[]string{"--service-node=n1"}, noMatch},
		{[]string{"--service-cluster=aether-proxy"}, noMatch},
		{[]string{"--service-zone=z"}, noMatch},
		{[]string{"--drain-strategy=gradual"}, noMatch},
		{[]string{"--log-level=info"}, noMatch},
		{[]string{"-l=info"}, noMatch},
		{[]string{"-linfo"}, noMatch},
		{[]string{"--log-format=%v"}, noMatch},
		{[]string{"--component-log-level=upstream:debug"}, noMatch},
		{[]string{"--stats-tag=a:b"}, noMatch},
		{[]string{"--log-path=/dev/null"}, noMatch},
		{[]string{"--skip-hot-restart-parent-stats=true"}, noMatch},
		// The same spelling of flags the supervisor reserves.
		{[]string{"--base-id=3"}, noMatch},
		{[]string{"--socket-path=@x"}, noMatch},

		// #1408: the value of --concurrency.
		{[]string{"--concurrency"}, noValue},
		{[]string{"--concurrency", "x"}, badValue},
		{[]string{"--concurrency", "2x"}, badValue},
		{[]string{"--concurrency", "1.5"}, badValue},
		{[]string{"--concurrency", "0x2"}, badValue},
		{[]string{"--concurrency", "2 "}, badValue},
		{[]string{"--concurrency", "4294967296"}, badValue},
		// The next argument is the value, whatever it looks like.
		{[]string{"--concurrency", "-l", "info"}, badValue},
		{[]string{"--concurrency", "--skip-hot-restart-parent-stats"}, badValue},

		// A flag given twice (#1375, #1376), for the flags the chart passes
		// and for the ones the supervisor passes on this command line.
		{[]string{"--concurrency", "2", "--concurrency", "2"}, alreadySet},
		{[]string{"-l", "info", "-l", "debug"}, alreadySet},
		{[]string{"-l", "info", "--log-level", "debug"}, alreadySet},
		{[]string{"--service-cluster", "a", "--service-cluster", "a"}, alreadySet},
		{[]string{"--service-node", "a", "--service-node", "a"}, alreadySet},
		{[]string{"--service-zone", "a", "--service-zone", "a"}, alreadySet},
		{[]string{"--drain-strategy", "gradual", "--drain-strategy", "immediate"}, alreadySet},
		{[]string{"--skip-hot-restart-parent-stats", "--skip-hot-restart-parent-stats"}, alreadySet},
		{[]string{"--mode", "validate"}, alreadySet},
		{[]string{"-c", "/dev/null"}, alreadySet},
		{[]string{"--config-path", "/dev/null"}, alreadySet},

		// #1443: the check reads the list as this parser does. A repeat of
		// any flag, not only of the ones the chart passes.
		{[]string{"--log-path", "/dev/null", "--log-path", "/dev/null"}, alreadySet},
		{[]string{"--cpuset-threads", "--cpuset-threads"}, alreadySet},
		{[]string{"--file-flush-interval-msec", "1", "--file-flush-interval-msec", "1"}, alreadySet},
		// A flag and its value sharing one item, split by a space, is a
		// spelling Envoy does take, so a repeat written that way is a repeat.
		{[]string{"--concurrency 2", "--concurrency", "2"}, alreadySet},
		{[]string{"-l info", "--log-level", "info"}, alreadySet},
		{[]string{"-c /dev/null"}, alreadySet},
		{[]string{"--mode validate"}, alreadySet},
		// And its value is read like any other.
		{[]string{"--concurrency x"}, badValue},
		{[]string{"--concurrency 2 3"}, twoValues},
		// A flag Envoy does not have.
		{[]string{"--some-future-flag"}, noMatch},
		{[]string{"--some-future-flag", "x"}, noMatch},
		{[]string{"--some-future-flag=x"}, noMatch},
		{[]string{"-x"}, noMatch},
		{[]string{"-v"}, noMatch},
		// An argument that is not a flag, where no flag takes it as a value.
		{[]string{"stray"}, noMatch},
		{[]string{" "}, noMatch},
		{[]string{"--cpuset-threads", "true"}, noMatch},
		{[]string{"-l", "info", "stray"}, noMatch},
		// A switch shares its item with nothing.
		{[]string{"--cpuset-threads x"}, noMatch},
		// A flag that takes a value, standing last.
		{[]string{"--service-node"}, noValue},
		{[]string{"--service-node "}, noValue},
		{[]string{"--stats-tag"}, noValue},
		// Two of the "h" Envoy reads as -h (one is in
		// TestCheckIsStricterThanEnvoyOnPurpose: Envoy exits 0 for it).
		{[]string{"-hh"}, alreadySet},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			accepted, out := validate(t, envoy, bootstrap, tc.args...)
			require.False(t, accepted, "the pinned Envoy accepts %v now; the rule that refuses it is out of date.\n%s", tc.args, out)
			assert.Contains(t, out, tc.envoy, "the pinned Envoy refuses %v for another reason now", tc.args)
			assert.Error(t, hotrestart.CheckExtraArgs(tc.args),
				"the pinned Envoy refuses %v on every fork, so the startup check must refuse it", tc.args)
		})
	}
}

// TestEnvoyAcceptsWhatTheCheckAccepts is the other direction, for the arguments
// the chart passes and the spellings next to the refused ones: the check lets
// them through and the pinned Envoy takes them.
func TestEnvoyAcceptsWhatTheCheckAccepts(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	for _, args := range [][]string{
		{},
		{
			"-l", "info", "--service-cluster", "aether-proxy", "--service-node", "n1", "--service-zone", "z",
			"--drain-strategy", "gradual", "--concurrency", "2", "--skip-hot-restart-parent-stats",
		},
		{"--log-level", "info"},
		{"--concurrency", "1"},
		{"--concurrency", "0"},
		{"--concurrency", "02"},
		{"--stats-tag", "a:b", "--stats-tag", "c:d"},
		{"--base-id-path", filepath.Join(t.TempDir(), "base-id")},
		{"--skip-hot-restart-on-no-parent"},
		{"--socket-mode", "600"},
		{"--cpuset-threads"},
		// Envoy reads the argument after a value flag as the value even when
		// it starts with a dash.
		{"--log-format", "- %v"},

		// #1443: and even when it is spelled exactly like a flag. Until then
		// the check compared every item and refused each of these.
		{"--service-node", "-c"},
		{"--service-node", "--base-id"},
		{"--service-node", "--socket-path"},
		{"--service-node", "-h"},
		{"--service-node", "--version"},
		{"--service-node", "--", "--service-zone", "z"},
		{"--log-format", "--x=y"},
		{"--log-format", "-linfo"},
		{"--service-cluster", "--service-cluster"},
		{"--log-path", "--log-path"},
		{"--service-node", "--concurrency", "--concurrency", "2"},
		// A flag and its value sharing one item, split by a space.
		{"--concurrency 2"},
		{"-l info", "--service-node n1"},
		{"--stats-tag a:b", "--stats-tag", "c:d"},
		{"--service-node ", "n1"},
		// Envoy accepts an empty item and a lone dash and ignores them.
		{""},
		{"-"},
		{"-l", "info", "", "--concurrency", "2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.NoError(t, hotrestart.CheckExtraArgs(args))
			accepted, out := validate(t, envoy, bootstrap, args...)
			assert.True(t, accepted, "the startup check accepts %v but the pinned Envoy refuses it:\n%s", args, out)
		})
	}
}

// TestEnvoySwitchFlagsTakeNoValue: for a flag that takes no value the check's
// advice for "--flag=true" is the flag alone, not two items. That is only right
// while the pinned Envoy takes each of these alone and refuses a value item
// after it. The list is Envoy's SwitchArg options that the check does not
// reserve: the switches of hotrestart.EnvoyFlags it lets through.
func TestEnvoySwitchFlagsTakeNoValue(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	for _, flag := range []string{
		"--skip-hot-restart-parent-stats",
		"--skip-hot-restart-on-no-parent",
		"--allow-unknown-fields",
		"--allow-unknown-static-fields",
		"--reject-unknown-dynamic-fields",
		"--ignore-unknown-dynamic-fields",
		"--skip-deprecated-logs",
		"--log-stacktrace-single-entry",
		"--log-format-escaped",
		"--enable-fine-grain-logging",
		"--enable-mutex-tracing",
		"--cpuset-threads",
		"--enable-core-dump",
	} {
		t.Run(flag, func(t *testing.T) {
			err := hotrestart.CheckExtraArgs([]string{flag + "=true"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "takes no value: pass it alone",
				"%s is not a switch in hotrestart.EnvoyFlags, so the error advises a second item Envoy refuses", flag)

			accepted, out := validate(t, envoy, bootstrap, flag)
			assert.True(t, accepted, "the pinned Envoy does not take %s alone:\n%s", flag, out)
			accepted, out = validate(t, envoy, bootstrap, flag, "true")
			assert.False(t, accepted, "the pinned Envoy takes a value after %s now:\n%s", flag, out)
		})
	}
}

// TestCheckIsStricterThanEnvoyOnPurpose lists what the check refuses although
// the pinned Envoy's parser accepts it. Each is deliberate: Envoy starts, and
// then does not do what the supervisor needs. The test pins that Envoy still
// accepts them, so the reason given in each error stays true: if Envoy starts
// to refuse one, the entry moves to TestEnvoyRefusesWhatTheCheckRefuses.
//
// "--concurrency -1" belongs here and is left out: Envoy accepts it, reads it
// as 4294967295 workers and does not finish starting.
func TestCheckIsStricterThanEnvoyOnPurpose(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	for _, args := range [][]string{
		// #1409. These are refused for what they do to a handoff, which a
		// validate run does not show.
		{"--use-dynamic-base-id"},
		{"--disable-hot-restart"},
		{"--socket-path", "@aether_test_socket"},
		// Envoy prints and exits 0 without serving.
		{"--hot-restart-version"},
		{"--version"},
		{"--help"},
		{"-h"},
		// Envoy ignores what follows.
		{"--", "--concurrency", "2"},
		{"--ignore_rest", "--concurrency", "2"},
		// #1408. Envoy reads these as a number; the check takes digits only.
		{"--concurrency", ""},
		{"--concurrency", "+2"},
		{"--concurrency", " 2"},
		// #1443. A reserved flag sharing an item with its value. Envoy takes
		// the spelling, so these reached every fork until the check read it.
		// (--base-id and --restart-epoch are accepted HERE because a validate
		// run does not pass them; a serving Envoy gets them twice.)
		{"--socket-path @aether_test_socket"},
		{"--base-id 5"},
		{"--restart-epoch 0"},
		// An "h" behind a single dash is read as -h: Envoy prints its usage
		// and exits 0 (TestEnvoyReadsAnHBehindOneDashAsHelp).
		{"-xh"},
		{"-lwhatever"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.Error(t, hotrestart.CheckExtraArgs(args))
			accepted, out := validate(t, envoy, bootstrap, args...)
			assert.True(t, accepted, "the pinned Envoy refuses %v now; the check's error should say so:\n%s", args, out)
		})
	}
}

// TestEnvoyRefusesADynamicBaseIDAboveEpochZero is the half of #1409 a parser
// run can show: with --use-dynamic-base-id every hot-restart child, which the
// supervisor starts at --restart-epoch N > 0, is refused.
func TestEnvoyRefusesADynamicBaseIDAboveEpochZero(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	accepted, out := validate(t, envoy, bootstrap, "--restart-epoch", "1", "--use-dynamic-base-id")
	require.False(t, accepted, out)
	assert.Contains(t, out, "cannot use --restart-epoch=1 with --use-dynamic-base-id")
}

// serverInfo is the part of Envoy's /server_info these tests read.
type serverInfo struct {
	CommandLineOptions struct {
		Concurrency int `json:"concurrency"`
	} `json:"command_line_options"`
}

// servedConcurrency starts the pinned Envoy for real with extra and returns the
// worker count its admin reports. Hot restart is off, so the run takes no base
// id and no shared memory on the machine.
func servedConcurrency(t *testing.T, envoy string, extra ...string) int {
	t.Helper()
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "envoy.yaml")
	require.NoError(t, os.WriteFile(bootstrap, []byte(minimalBootstrap), 0o600))
	adminPath := filepath.Join(dir, "admin")

	ctx, cancel := context.WithTimeout(t.Context(), envoyRunTimeout)
	defer cancel()
	args := append([]string{"-c", bootstrap, "--disable-hot-restart", "--admin-address-path", adminPath, "-l", "warn"}, extra...)
	cmd := exec.CommandContext(ctx, envoy, args...)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var info serverInfo
	require.Eventually(t, func() bool {
		addr, err := os.ReadFile(adminPath)
		if err != nil || len(addr) == 0 {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+strings.TrimSpace(string(addr))+"/server_info", nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return err == nil && resp.StatusCode == http.StatusOK && json.Unmarshal(body, &info) == nil
	}, envoyRunTimeout, 100*time.Millisecond, "envoy %v never answered /server_info", args)
	return info.CommandLineOptions.Concurrency
}

// TestEnvoyRunsOneWorkerForConcurrencyZero pins the one value the check
// translates instead of passing through or refusing (#1408): the pinned Envoy
// accepts --concurrency 0 and runs ONE worker (options_impl.cc takes
// max(1, value)). The supervisor compares its successor's count with a live
// predecessor's, so it reads 0 as 1; if Envoy ever gave 0 another meaning,
// that comparison would be wrong on every handoff.
func TestEnvoyRunsOneWorkerForConcurrencyZero(t *testing.T) {
	envoy := pinnedEnvoy(t)

	assert.Equal(t, 1, servedConcurrency(t, envoy, "--concurrency", "0"))
	// Control: the reported number follows the flag.
	assert.Equal(t, 2, servedConcurrency(t, envoy, "--concurrency", "2"))
}

// TestEnvoyReadsAnHBehindOneDashAsHelp pins why the check refuses a
// single-dash item with an "h" in it (#1443): the pinned Envoy does not answer
// "Couldn't find match for argument" for it, it prints its usage and exits 0
// without serving. Without the "h" the same item is refused.
func TestEnvoyReadsAnHBehindOneDashAsHelp(t *testing.T) {
	envoy, bootstrap := pinnedEnvoy(t), writeBootstrap(t)

	for _, item := range []string{"-xh", "-lwhatever", "-hx"} {
		accepted, out := validate(t, envoy, bootstrap, item)
		assert.True(t, accepted, "%s: the pinned Envoy no longer exits 0:\n%s", item, out)
		assert.Contains(t, out, "USAGE:", "%s: the pinned Envoy no longer prints its usage", item)
		assert.Error(t, hotrestart.CheckExtraArgs([]string{item}), item)
	}
	for _, item := range []string{"-x", "-linfo"} {
		accepted, out := validate(t, envoy, bootstrap, item)
		assert.False(t, accepted, "%s:\n%s", item, out)
		assert.Contains(t, out, "Couldn't find match for argument", item)
	}
	// As a value it is a value.
	accepted, out := validate(t, envoy, bootstrap, "--service-node", "-xh")
	assert.True(t, accepted, out)
	assert.NotContains(t, out, "USAGE:")
}

// helpFlagLine matches one flag of the long part of `envoy --help`:
//
//	--stats-tag <string>  (accepted multiple times)
//	-l <string>,  --log-level <string>
//	--,  --ignore_rest
//	--cpuset-threads
var helpFlagLine = regexp.MustCompile(
	`^   (-[^ ,]+)( <[^>]+>)?(?:,  (--[^ ]+)(?: <[^>]+>)?)?(  \(accepted multiple times\))?$`)

// TestEnvoyFlagTableMatchesThePinnedEnvoy holds the flag table the check
// parses with (hotrestart.EnvoyFlags, #1443) against the flags the pinned
// binary lists itself: the same names, the same short spellings, and the same
// answer to "does it take a value" and "may it repeat". The check refuses a
// flag that is not in the table, so a pin bump that adds one has to add it
// here first, and one that removes or changes one fails here instead of at a
// fork.
func TestEnvoyFlagTableMatchesThePinnedEnvoy(t *testing.T) {
	envoy := pinnedEnvoy(t)

	ctx, cancel := context.WithTimeout(t.Context(), envoyRunTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, envoy, "--help").Output()
	require.NoError(t, err, "envoy --help")

	var listed []hotrestart.EnvoyFlag
	for _, line := range strings.Split(string(out), "\n") {
		m := helpFlagLine.FindStringSubmatch(line)
		if m == nil {
			require.False(t, strings.HasPrefix(line, "   -"),
				"a flag line of `envoy --help` this test does not understand: %q", line)
			continue
		}
		f := hotrestart.EnvoyFlag{Long: m[1]}
		if m[3] != "" {
			f.Short, f.Long = m[1], m[3]
		}
		switch {
		case m[4] != "":
			require.NotEmpty(t, m[2], "%q repeats and takes no value", line)
			f.Kind = hotrestart.EnvoyMultiValue
		case m[2] != "":
			f.Kind = hotrestart.EnvoyValue
		}
		listed = append(listed, f)
	}
	// Control: the scan read the list. The pinned Envoy has 44 flags.
	require.GreaterOrEqual(t, len(listed), 40, "only %d flags read from `envoy --help`", len(listed))

	assert.ElementsMatch(t, listed, hotrestart.EnvoyFlags(),
		"the pinned Envoy's flags differ from hotrestart.envoyFlags: update the table (and reservedEnvoyFlags, "+
			"if a new flag touches the base id, the hot-restart socket or what Envoy serves)")
}

// TestEnvoyRunsTheConcurrencyThatSharesItsItem: the supervisor reads
// "--concurrency 2" in one item as two workers (#1443) and compares that with
// a live predecessor's count. It is only right while the pinned Envoy reads the
// item the same way.
func TestEnvoyRunsTheConcurrencyThatSharesItsItem(t *testing.T) {
	envoy := pinnedEnvoy(t)

	require.NoError(t, hotrestart.CheckExtraArgs([]string{"--concurrency 2"}))
	assert.Equal(t, 2, servedConcurrency(t, envoy, "--concurrency 2"))
}
