package supervisorcmd

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chartDaemonSet is the rendered-template source the aether-proxy pod's argv
// comes from. It is read as test data (Bazel `data`) rather than duplicated
// here, so this cannot drift from what actually ships.
const chartDaemonSet = "charts/aether/templates/agent-proxy-daemonset.yaml"

// argFlag matches a YAML list item that is a long flag, e.g. `- "--config=..."`
// or `- "--watch-config=true"`. It deliberately anchors at the start of the
// quoted value so that `--envoy-arg=--service-cluster` contributes `envoy-arg`
// and not `service-cluster` — the latter is Envoy's flag, not ours.
var argFlag = regexp.MustCompile(`(?m)^\s*-\s*"--([a-z0-9-]+)`)

// The supervisor's argv spans the install-supervisor initContainer and the proxy
// container. Since #1275 the optional authz sidecar sits BETWEEN them — it is a
// native sidecar, an init container after install-supervisor — and its flags
// (OPA's --server, --addr, --set; proxy-ready's --unix-socket) live in the same
// file but are not ours. So the scan is `initContainers:` up to the sidecar,
// plus `containers:` (the proxy) to the end.
const (
	argvStart  = "initContainers:"
	authzStart = "- name: authz"
	proxyStart = "\n      containers:\n"
)

// supervisorArgv slices the template down to the two blocks whose flags the
// supervisor is handed.
func supervisorArgv(t *testing.T, template string) string {
	t.Helper()
	start := strings.Index(template, argvStart)
	require.GreaterOrEqual(t, start, 0,
		"%s has no %q — the template was restructured and this scan cannot be trusted",
		chartDaemonSet, argvStart)
	authz := strings.Index(template, authzStart)
	proxy := strings.Index(template, proxyStart)
	require.True(t, start < authz && authz < proxy,
		"%s: expected %q, then the authz sidecar %q, then %q — the template was restructured "+
			"and this scan would pick up another container's flags or miss the supervisor's",
		chartDaemonSet, argvStart, authzStart, proxyStart)
	return template[start:authz] + template[proxy:]
}

// TestFlagsCoverTheChartContract pins the flag set against the chart.
//
// The aether-proxy DaemonSet passes these names literally, in two places (the
// install-supervisor initContainer's args and the proxy container's command), so
// a rename or a removal here is a chart change — and the failure mode without
// this test is a supervisor that exits on "unknown flag" at pod start, i.e. the
// whole proxy fleet, discovered only at rollout time.
//
// #772 moved this command out of the agent binary into its own one. Every flag
// had to survive that move byte-for-byte; this is what proves it did, and what
// keeps it true afterwards.
func TestFlagsCoverTheChartContract(t *testing.T) {
	raw, err := os.ReadFile(findRepoFile(t, chartDaemonSet))
	require.NoError(t, err, "reading the proxy DaemonSet template")

	seen := map[string]struct{}{}
	for _, m := range argFlag.FindAllStringSubmatch(supervisorArgv(t, string(raw)), -1) {
		seen[m[1]] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	// Control test: a template that stopped matching (retitled, restructured,
	// moved) must fail loudly rather than pass this test vacuously on an empty
	// set. The chart passes well over a dozen flags.
	require.GreaterOrEqual(t, len(names), 12,
		"only %d flags found in %s — the scan cannot be trusted", len(names), chartDaemonSet)
	require.Contains(t, names, "install-readiness-path", "the initContainer's args were not scanned")

	flags := New("test").Flags()
	for _, name := range names {
		assert.NotNil(t, flags.Lookup(name),
			"the chart passes --%s to the supervisor but the command does not define it; "+
				"every pod would fail to start on 'unknown flag'", name)
	}
	t.Logf("chart passes %d supervisor flags: %v", len(names), names)
}

// TestRetiredFlagsGone pins the removal of --readiness-check, the pre-#673 exec
// readiness probe (deprecated by #673, removed once no supported chart used it).
// The chart execs the stdlib-only proxy-ready prober instead; re-registering the
// flag would re-open a path that re-execs this binary every 2s per pod.
func TestRetiredFlagsGone(t *testing.T) {
	flags := New("test").Flags()
	for _, name := range []string{"readiness-check"} {
		assert.Nil(t, flags.Lookup(name), "flag --%s was retired and must not be re-registered", name)
	}
}

// findRepoFile locates a repo-relative path from the test's working directory,
// which is the package dir under both `go test` and Bazel's runfiles tree.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Never skip: this test exists precisely to fail when the pinned file is
	// not the one we think it is.
	t.Fatalf("%s not found above the working directory; under Bazel the go_test "+
		"needs data = [\"//charts/aether:templates/agent-proxy-daemonset.yaml\"]", rel)
	return ""
}

// TestEnvoyArgRejectsTheReservedAdminAddressPath: the supervisor sets Envoy's
// --admin-address-path itself — it carries the admin identity that keeps a
// drain off another pod's Envoy (#1127) — and Envoy refuses the flag twice, so
// an operator-supplied one must fail fast rather than fail every fork.
func TestEnvoyArgRejectsTheReservedAdminAddressPath(t *testing.T) {
	require.NoError(t, checkEnvoyArgs([]string{"-l", "info", "--service-node", "n1"}))
	assert.Error(t, checkEnvoyArgs([]string{"--admin-address-path", "/tmp/x"}))
	assert.Error(t, checkEnvoyArgs([]string{"--admin-address-path=/tmp/x"}))
	// One item, split by a space: Envoy accepts that spelling (#1443).
	assert.Error(t, checkEnvoyArgs([]string{"--admin-address-path /tmp/x"}))

	// "--admin-address-pathology" was in the accepted list above until #1443.
	// It is not the reserved flag, and it is no flag of the pinned Envoy
	// either, so it is refused as that.
	err := checkEnvoyArgs([]string{"-l", "info", "--service-node", "n1", "--admin-address-pathology"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "is reserved")
	assert.Contains(t, err.Error(), "has no flag --admin-address-pathology")
}

// TestEnvoyArgRejectsEveryFlagTheSupervisorSets: Envoy refuses a flag given
// twice ("Argument already set!"), so an --envoy-arg that repeats a flag the
// supervisor passes itself fails every fork of every epoch. Each one must be a
// startup error instead, in both spellings, naming the flag and whatever owns
// it (#1376).
func TestEnvoyArgRejectsEveryFlagTheSupervisorSets(t *testing.T) {
	for _, tc := range []struct {
		flag, value string
		// owner is the supervisor option or chart value the error must point
		// the operator at.
		owner string
	}{
		{flag: "-c", value: "/etc/other.yaml", owner: "--config"},
		{flag: "--config-path", value: "/etc/other.yaml", owner: "--config"},
		{flag: "--base-id", value: "7", owner: "proxy.hotRestart.baseId"},
		{flag: "--restart-epoch", value: "3", owner: "epochs"},
		{flag: "--drain-time-s", value: "30", owner: "proxy.hotRestart.drainTime"},
		{flag: "--parent-shutdown-time-s", value: "45", owner: "proxy.hotRestart.parentShutdownTime"},
		{flag: "--admin-address-path", value: "/tmp/x", owner: "admin identity"},
		{flag: "--mode", value: "validate", owner: "--mode validate"},
	} {
		for spelling, args := range map[string][]string{
			"separate value": {"-l", "info", tc.flag, tc.value},
			"equals value":   {"-l", "info", tc.flag + "=" + tc.value},
			"first argument": {tc.flag, tc.value, "-l", "info"},
		} {
			t.Run(tc.flag+"/"+spelling, func(t *testing.T) {
				err := checkEnvoyArgs(args)
				require.Error(t, err, "%v must be refused at startup", args)
				assert.Contains(t, err.Error(), tc.flag, "the error must name the flag")
				assert.Contains(t, err.Error(), tc.owner, "the error must name what owns the flag")
			})
		}
	}
}

// TestEnvoyArgAllowsWhatIsNotReserved: only a reserved flag is refused as
// reserved. A value that contains one, or that is spelled exactly like one, is
// a value (#1443).
func TestEnvoyArgAllowsWhatIsNotReserved(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-l", "info", "--service-cluster", "aether-proxy", "--service-node", "n1", "--service-zone", "z"},
		{"--drain-strategy", "immediate", "--skip-hot-restart-parent-stats"},
		{"--base-id-path", "/tmp/base-id"},
		{"--config-yaml", "{}"},
		{"--component-log-level", "upstream:debug,config:trace"},
		{"--log-format", "[%Y] --base-id %v"},
		{"--service-node", "--base-id"},
		{"--service-node", "-c", "--service-zone", "--mode"},
	} {
		assert.NoError(t, checkEnvoyArgs(args), "%v", args)
	}

	// A longer flag that merely starts like a reserved one is not reserved.
	// This list was accepted until #1443; the pinned Envoy has none of these
	// flags and refuses each on every fork, so the first one is refused, as a
	// flag Envoy does not have.
	err := checkEnvoyArgs([]string{"--drain-time-seconds", "1", "--restart-epochs", "1", "--modest", "--config-path-x", "y"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "is reserved")
	assert.Contains(t, err.Error(), "has no flag --drain-time-seconds")
}

// TestEnvoyArgConcurrencyOnceOnly: --concurrency is the one Envoy flag the chart
// passes through --envoy-arg on purpose (proxy.concurrency), so one is fine. Two
// are the same every-fork failure as a reserved flag — Envoy does not keep the
// last one, it refuses the command line (#1375).
func TestEnvoyArgConcurrencyOnceOnly(t *testing.T) {
	require.NoError(t, checkEnvoyArgs([]string{"-l", "info", "--concurrency", "2"}))
	// Until #1407 this line required --concurrency=2 to be accepted. The pinned
	// Envoy does not parse that spelling, so one of them is refused as well.
	require.Error(t, checkEnvoyArgs([]string{"--concurrency=2", "-l", "info"}))

	for name, args := range map[string][]string{
		"separate, separate":    {"--concurrency", "2", "-l", "info", "--concurrency", "4"},
		"separate, equals":      {"--concurrency", "2", "--concurrency=4"},
		"equals, separate":      {"--concurrency=2", "--concurrency", "4"},
		"equals, equals":        {"--concurrency=2", "--concurrency=2"},
		"same value twice":      {"--concurrency", "2", "--concurrency", "2"},
		"three times":           {"--concurrency", "1", "--concurrency", "2", "--concurrency", "3"},
		"second one is invalid": {"--concurrency", "2", "--concurrency", "x"},
		"first one is invalid":  {"--concurrency", "x", "--concurrency", "2"},
		// A second --concurrency standing where the first one's value should be.
		"adjacent, separate": {"--concurrency", "--concurrency", "2"},
		"adjacent, equals":   {"--concurrency", "--concurrency=4"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkEnvoyArgs(args)
			require.Error(t, err, "%v must be refused at startup", args)
			assert.Contains(t, err.Error(), "--concurrency")
			assert.Contains(t, err.Error(), "proxy.concurrency", "the error must name the chart value that owns the flag")
		})
	}
}

// TestEnvoyArgRejectsSpellingsEnvoyDoesNotParse is #1407. The pinned Envoy
// accepts a flag and its value as two arguments only; "--flag=value",
// "-f=value" and "-fvalue" answer "Couldn't find match for argument" on every
// fork. --envoy-arg=--concurrency=2 used to pass this check. Each is a startup
// error that shows the two items to pass instead.
func TestEnvoyArgRejectsSpellingsEnvoyDoesNotParse(t *testing.T) {
	for _, tc := range []struct {
		arg, twoItems string
	}{
		{"--concurrency=2", "--envoy-arg=--concurrency --envoy-arg=2"},
		{"--service-node=n1", "--envoy-arg=--service-node --envoy-arg=n1"},
		{"--service-cluster=aether-proxy", "--envoy-arg=--service-cluster --envoy-arg=aether-proxy"},
		{"--drain-strategy=immediate", "--envoy-arg=--drain-strategy --envoy-arg=immediate"},
		{"--log-level=debug", "--envoy-arg=--log-level --envoy-arg=debug"},
		{"-l=debug", "--envoy-arg=-l --envoy-arg=debug"},
		{"-ldebug", "--envoy-arg=-l --envoy-arg=debug"},
		{"--stats-tag=a:b", "--envoy-arg=--stats-tag --envoy-arg=a:b"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			err := checkEnvoyArgs([]string{tc.arg})
			require.Error(t, err, "%s must be refused at startup: the pinned Envoy does not parse it", tc.arg)
			assert.Contains(t, err.Error(), tc.arg, "the error must name the argument")
			assert.Contains(t, err.Error(), tc.twoItems, "the error must show the accepted two-item form")
		})
	}
	// The accepted spelling of the same flags.
	require.NoError(t, checkEnvoyArgs([]string{
		"--concurrency", "2", "--service-node", "n1", "--service-cluster", "aether-proxy",
		"--drain-strategy", "immediate", "-l", "debug", "--stats-tag", "a:b=c",
	}))
}

// TestEnvoyArgRejectsABadConcurrencyValue is #1408: a lone --concurrency with a
// missing or unusable value passed this check and surfaced only at a handoff,
// as "successor worker count unknown". What the pinned Envoy does with each
// value is recorded on hotrestart.parseConcurrencyValue.
func TestEnvoyArgRejectsABadConcurrencyValue(t *testing.T) {
	for name, args := range map[string][]string{
		"last argument, no value": {"-l", "info", "--concurrency"},
		"not a number":            {"--concurrency", "x"},
		"negative":                {"--concurrency", "-1"},
		"value is another flag":   {"--concurrency", "--skip-hot-restart-parent-stats"},
		"value is a short flag":   {"--concurrency", "-l", "info"},
		"empty":                   {"--concurrency", ""},
		"fraction":                {"--concurrency", "1.5"},
		"above uint32":            {"--concurrency", "4294967296"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkEnvoyArgs(args)
			require.Error(t, err, "%v must be refused at startup", args)
			assert.Contains(t, err.Error(), "--concurrency")
			assert.Contains(t, err.Error(), "proxy.concurrency", "the error must name the chart value that owns the flag")
		})
	}
	// 0 is not an error: Envoy runs one worker for it.
	require.NoError(t, checkEnvoyArgs([]string{"--concurrency", "0"}))
	require.NoError(t, checkEnvoyArgs([]string{"--concurrency", "4"}))
}

// TestEnvoyArgRejectsFlagsThatBreakAHandoff is #1409: Envoy flags the
// supervisor does not pass, so they are no repeat, but that defeat what it
// controls. --use-dynamic-base-id makes Envoy ignore the fixed --base-id (and
// Envoy refuses it at any epoch above 0); the others are classified in
// hotrestart.reservedEnvoyFlags. Each error says why.
func TestEnvoyArgRejectsFlagsThatBreakAHandoff(t *testing.T) {
	for flag, why := range map[string]string{
		"--use-dynamic-base-id": "proxy.hotRestart.baseId",
		"--disable-hot-restart": "neither drained nor stopped",
		"--socket-path":         "cannot hand off",
		"--hot-restart-version": "without serving",
		"--version":             "without serving",
		"--help":                "without serving",
		"-h":                    "without serving",
		"--":                    "ignores every argument after it",
		"--ignore_rest":         "ignores every argument after it",
	} {
		t.Run(flag, func(t *testing.T) {
			err := checkEnvoyArgs([]string{"-l", "info", flag, "x"})
			require.Error(t, err, "%s must be refused at startup", flag)
			assert.Contains(t, err.Error(), flag, "the error must name the flag")
			assert.Contains(t, err.Error(), why, "the error must say why")
		})
	}
	// Left alone on purpose: --base-id-path only writes the base id to a file,
	// and --skip-hot-restart-parent-stats is a chart value.
	require.NoError(t, checkEnvoyArgs([]string{
		"--base-id-path", "/tmp/base-id", "--skip-hot-restart-parent-stats", "--skip-hot-restart-on-no-parent",
	}))
}

// envoyArgItem matches one `- "--envoy-arg=<token>"` list item of the template.
var envoyArgItem = regexp.MustCompile(`(?m)^\s*-\s*"--envoy-arg=(.*)"\s*$`)

// chartEnvoyArgs returns every --envoy-arg item of the DaemonSet template, all
// conditional branches at once, which is more than any one set of values
// produces.
//
// The items are template source, not a render. That only matters for the value
// after --concurrency, which the check reads (#1408): the template action
// there (`{{ . }}`, the validated proxy.concurrency) is replaced by a number,
// as a render would. Every other value is an opaque string to the check.
func chartEnvoyArgs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(findRepoFile(t, chartDaemonSet))
	require.NoError(t, err, "reading the proxy DaemonSet template")

	var args []string
	for _, m := range envoyArgItem.FindAllStringSubmatch(supervisorArgv(t, string(raw)), -1) {
		args = append(args, m[1])
	}
	// Control: an empty or truncated scan must not pass vacuously. The chart
	// passes at least -l, the three --service-* pairs, --drain-strategy and the
	// optional --concurrency.
	require.GreaterOrEqual(t, len(args), 12, "only %d --envoy-arg items found in %s: %v", len(args), chartDaemonSet, args)

	at := slices.Index(args, "--concurrency")
	require.GreaterOrEqual(t, at, 0, "the chart's proxy.concurrency pass-through was not scanned")
	require.Less(t, at+1, len(args), "the chart passes --concurrency as its last --envoy-arg, with no value")
	require.Regexp(t, `^\{\{.*\}\}$`, args[at+1],
		"the item after --concurrency is no longer a template action; this test stands a number in for it")
	args[at+1] = "2"
	return args
}

// TestChartEnvoyArgsPassTheCheck: every --envoy-arg the chart can render goes
// through the startup check. A chart that passed a flag the supervisor
// reserves, --concurrency twice, or a flag and its value in one item, would
// take down every proxy pod at its next start; this fails first.
func TestChartEnvoyArgsPassTheCheck(t *testing.T) {
	args := chartEnvoyArgs(t)
	require.Contains(t, args, "--concurrency", "the chart's proxy.concurrency pass-through was not scanned")

	require.NoError(t, checkEnvoyArgs(args), "the chart renders an --envoy-arg the supervisor refuses: %v", args)
	t.Logf("chart --envoy-arg items: %v", args)

	// Control: the same list with a reserved flag added is refused, so the
	// assertion above can fail.
	require.Error(t, checkEnvoyArgs(append(args, "--base-id", "1")))
}

// TestChartEnvoyArgFlagsAreAllowedOnce: the pinned Envoy refuses any flag given
// twice, so each flag the chart passes through --envoy-arg can be passed once
// and no more. Since #1443 a repeat of any flag is refused; what a flag of the
// chart's needs besides is an entry in hotrestart.onceEnvoyFlags, so the error
// tells the operator that the first occurrence is the chart's own. This reads
// the flags off the template, so a flag the chart starts passing fails here
// until it has that entry.
func TestChartEnvoyArgFlagsAreAllowedOnce(t *testing.T) {
	args := chartEnvoyArgs(t)
	flags := 0
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flags++
		err := checkEnvoyArgs(append(slices.Clone(args), a, "x"))
		require.Error(t, err, "the chart passes %s; a second one must be refused at startup", a)
		assert.Contains(t, err.Error(), a)
		assert.Contains(t, err.Error(), "more than once")
		assert.Contains(t, err.Error(), "the chart already passes",
			"%s has no entry in hotrestart.onceEnvoyFlags: the error does not say the chart passes it", a)
	}
	// Control: -l, three --service-*, --drain-strategy, --concurrency and
	// --skip-hot-restart-parent-stats.
	require.GreaterOrEqual(t, flags, 7, "only %d flags found among the chart's --envoy-arg items: %v", flags, args)
}

// TestStallTopConsumersFlag: the stall line's consumer list is on by default
// (the chart passes no flag for it) and --stall-top-consumers sizes it or turns
// it off (#1392).
func TestStallTopConsumersFlag(t *testing.T) {
	cmd := New("test")
	f := cmd.Flags().Lookup("stall-top-consumers")
	require.NotNil(t, f)
	assert.Equal(t, "5", f.DefValue)

	cfg := &config{}
	bound := &cobra.Command{Use: "x"}
	bindFlags(bound, cfg)
	require.NoError(t, bound.ParseFlags(nil))
	assert.Equal(t, hotrestart.DefaultStallTopConsumers, cfg.supervisor.StallTopConsumers)
	require.NoError(t, bound.ParseFlags([]string{"--stall-top-consumers=0"}))
	assert.Zero(t, cfg.supervisor.StallTopConsumers)
	assert.Error(t, bound.ParseFlags([]string{"--stall-top-consumers=many"}))
}
