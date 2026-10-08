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
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.NoError(t, hotrestart.CheckExtraArgs(args))
			accepted, out := validate(t, envoy, bootstrap, args...)
			assert.True(t, accepted, "the startup check accepts %v but the pinned Envoy refuses it:\n%s", args, out)
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
