package hotrestart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file are issue #1443: CheckExtraArgs reads the argument
// list the way the pinned Envoy does, a flag and then its value when the flag
// takes one, instead of comparing every item with its lists. Each case was run
// through the pinned binary first; //agent/test/envoyargs runs them again.

// TestCheckExtraArgsDoesNotReadAValueAsAFlag: the item after a flag that takes
// a value is that value, whatever it looks like. Each of these was refused
// while every item was compared, although Envoy takes it.
func TestCheckExtraArgsDoesNotReadAValueAsAFlag(t *testing.T) {
	for _, args := range [][]string{
		// A value spelled like a flag the supervisor passes itself.
		{"--service-node", "-c"},
		{"--service-node", "--base-id"},
		{"--service-zone", "--restart-epoch"},
		// A value spelled like a flag that breaks a handoff.
		{"--service-node", "--socket-path"},
		{"--service-node", "-h"},
		{"--service-node", "--version"},
		// "--" as a value does not end the list: the flag after it is read.
		{"--service-node", "--", "--service-zone", "z"},
		// A value spelled like "flag=value" or like a glued short flag.
		{"--log-format", "--x=y"},
		{"--log-format", "--concurrency=2"},
		{"--log-format", "-linfo"},
		// A value equal to its own flag is one flag, not two.
		{"--service-cluster", "--service-cluster"},
		// A value equal to a flag given once elsewhere.
		{"--service-node", "--concurrency", "--concurrency", "2"},
		{"--service-node", "-l", "-l", "info"},
		// A value for --stats-tag is free-form as far as the parser goes.
		{"--stats-tag", "a:--mode"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%q: Envoy reads the second item as the first one's value", args)
	}
}

// TestCheckExtraArgsFindsAFlagWhereverItStands: a flag and its value may share
// one item, split by a space. The pinned Envoy accepts that, so a reserved or
// repeated flag written that way reached every fork while only whole items
// were compared.
func TestCheckExtraArgsFindsAFlagWhereverItStands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--socket-path @aether"}, "--envoy-arg --socket-path @aether is reserved"},
		{[]string{"--base-id 5"}, "--envoy-arg --base-id 5 is reserved"},
		{[]string{"-c /etc/other.yaml"}, "--envoy-arg -c /etc/other.yaml is reserved"},
		{[]string{"--mode validate"}, "--envoy-arg --mode validate is reserved"},
		{[]string{"-l", "info", "--restart-epoch 3"}, "--envoy-arg --restart-epoch 3 is reserved"},
		// Behind a flag and its value, not behind a value.
		{[]string{"--service-node", "--service-zone", "--base-id", "5"}, "--envoy-arg --base-id is reserved"},
		{[]string{"--concurrency 2", "--concurrency", "2"}, "--concurrency is given more than once (2 times)"},
		{[]string{"-l info", "--log-level", "debug"}, "-l / --log-level is given more than once (2 times)"},
		// The value in a shared item is checked like any other.
		{[]string{"--concurrency x"}, `--concurrency "x" is not a whole number`},
		{[]string{"--concurrency 2 3"}, `--concurrency "2 3" is not a whole number`},
		{[]string{"--concurrency -1"}, "is negative"},
	} {
		err := CheckExtraArgs(tc.args)
		require.Error(t, err, "%q must be refused at startup", tc.args)
		assert.Contains(t, err.Error(), tc.want, "%q", tc.args)
	}

	// The same spelling of what is allowed is allowed.
	for _, args := range [][]string{
		{"--concurrency 2"},
		{"-l info", "--service-node n1"},
		{"--stats-tag a:b", "--stats-tag", "c:d"},
		// Two spaces: the value starts with one. Envoy's parser takes it.
		{"--service-node  n1"},
		// A space and nothing after it: the value is the next item.
		{"--service-node ", "n1"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%q", args)
	}
}

// TestCheckExtraArgsRefusesAnyRepeatedFlag: the pinned Envoy refuses every
// flag given twice except --stats-tag ("Argument already set!"), not only the
// flags the chart passes.
func TestCheckExtraArgsRefusesAnyRepeatedFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		flag string
	}{
		{[]string{"--log-path", "/dev/null", "--log-path", "/dev/null"}, "--log-path"},
		{[]string{"--cpuset-threads", "--cpuset-threads"}, "--cpuset-threads"},
		{[]string{"--file-flush-interval-msec", "1", "-l", "info", "--file-flush-interval-msec", "1"}, "--file-flush-interval-msec"},
		{[]string{"--component-log-level", "upstream:debug", "--component-log-level=config:trace"}, "--component-log-level"},
		{[]string{"--base-id-path", "/a", "--base-id-path", "/b", "--base-id-path", "/c"}, "--base-id-path"},
	} {
		err := CheckExtraArgs(tc.args)
		require.Error(t, err, "%q must be refused at startup", tc.args)
		assert.Contains(t, err.Error(), tc.flag+" is given more than once", "%q", tc.args)
		assert.Contains(t, err.Error(), "Pass it once", "%q", tc.args)
		assert.NotContains(t, err.Error(), "the chart", "%q: the chart does not pass this flag", tc.args)
	}

	for _, args := range [][]string{
		{"--stats-tag", "a:b", "--stats-tag", "c:d", "--stats-tag", "e:f"},
		// The second one is the first one's value.
		{"--log-path", "--log-path"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%q", args)
	}
}

// TestCheckExtraArgsRefusesWhatEnvoyHasNoMatchFor: a flag the pinned Envoy does
// not have, an argument that is not a flag and that no flag takes as its
// value, and a flag that needs a value and stands last. Envoy refuses each on
// every fork; all of them passed while the check knew only its own lists.
func TestCheckExtraArgsRefusesWhatEnvoyHasNoMatchFor(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--some-future-flag"}, "the pinned Envoy has no flag --some-future-flag"},
		{[]string{"--some-future-flag", "x"}, "the pinned Envoy has no flag --some-future-flag"},
		{[]string{"-x"}, "the pinned Envoy has no flag -x"},
		{[]string{"-v"}, "the pinned Envoy has no flag -v"},
		{[]string{"--log-levle", "info"}, "the pinned Envoy has no flag --log-levle"},
		{[]string{"--", "x"}, "is reserved"},
		{[]string{"stray"}, `--envoy-arg "stray" is not a flag`},
		// A switch takes no value, so the item after it is on its own.
		{[]string{"--cpuset-threads", "true"}, `--envoy-arg "true" is not a flag`},
		{[]string{"--skip-hot-restart-parent-stats", "1"}, `--envoy-arg "1" is not a flag`},
		{[]string{"-l", "info", "debug"}, `--envoy-arg "debug" is not a flag`},
		{[]string{" "}, `--envoy-arg " " is not a flag`},
		{[]string{"--service-node"}, "--envoy-arg --service-node is the last argument and has no value"},
		{[]string{"-l", "info", "--stats-tag"}, "--envoy-arg --stats-tag is the last argument and has no value"},
		{[]string{"--service-node "}, "is the last argument and has no value"},
		// A switch does not share an item with anything.
		{[]string{"--cpuset-threads x"}, "the pinned Envoy has no flag"},
	} {
		err := CheckExtraArgs(tc.args)
		require.Error(t, err, "%q must be refused at startup: the pinned Envoy refuses it on every fork", tc.args)
		assert.Contains(t, err.Error(), tc.want, "%q", tc.args)
	}

	// Envoy accepts an empty item and a lone dash, and does nothing with them.
	// A chart value that renders empty must not stop the proxy.
	for _, args := range [][]string{
		{""},
		{"-"},
		{"-l", "info", "", "--concurrency", "2"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%q", args)
	}
}

// TestCheckExtraArgsRefusesWhatEnvoyReadsAsHelp: Envoy's parser lets one-letter
// switches share a dash, and -h is the only one it has, so it reads an "h"
// anywhere in a single-dash item as -h. Measured: "-xh" and "-lwhatever" print
// the usage and exit 0, on every fork.
func TestCheckExtraArgsRefusesWhatEnvoyReadsAsHelp(t *testing.T) {
	for _, item := range []string{"-xh", "-ah", "-hx", "-hh"} {
		err := CheckExtraArgs([]string{"-l", "info", item})
		require.Error(t, err, item)
		assert.Contains(t, err.Error(), "--envoy-arg "+item+" is reserved", item)
		assert.Contains(t, err.Error(), `reads an "h" in a single-dash argument as its -h switch`, item)
		assert.Contains(t, err.Error(), "prints its usage and exits 0 without serving", item)
	}

	// A value glued to -l is still told how to respell it, and no longer that
	// Envoy only has no match for it.
	err := CheckExtraArgs([]string{"-lwhatever"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--envoy-arg=-l --envoy-arg=whatever")
	assert.Contains(t, err.Error(), "prints its usage")

	// Not single-dash, or a value: no -h.
	for _, args := range [][]string{
		{"--service-node", "-xh"},
		{"--skip-hot-restart-parent-stats"},
		{"--log-path", "/var/log/h"},
	} {
		assert.NoError(t, CheckExtraArgs(args), "%q", args)
	}
}

// TestCheckExtraArgsNamesTheArgumentItRefuses: every error starts with the
// option and quotes the item, so the operator can find it in a long list.
func TestCheckExtraArgsNamesTheArgumentItRefuses(t *testing.T) {
	for _, args := range [][]string{
		{"--socket-path @x"},
		{"--nope"},
		{"stray"},
		{"--service-node"},
		{"--log-path", "a", "--log-path", "b"},
		{"-xh"},
	} {
		err := CheckExtraArgs(args)
		require.Error(t, err, "%q", args)
		assert.True(t, strings.HasPrefix(err.Error(), "--envoy-arg "), "%q: %v", args, err)
	}
}

// TestConcurrencyArgReadsTheListAsEnvoyDoes: the worker count the supervisor
// compares with a predecessor's has to be the one Envoy will run, so the
// --concurrency it reads has to be the one Envoy reads.
func TestConcurrencyArgReadsTheListAsEnvoyDoes(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		n        int
		explicit bool
	}{
		// A flag and its value in one item.
		{args: []string{"--concurrency 4"}, n: 4, explicit: true},
		// "--concurrency" as another flag's value is not the flag.
		{args: []string{"--service-node", "--concurrency"}},
		{args: []string{"--service-node", "--concurrency", "--concurrency", "3"}, n: 3, explicit: true},
		{args: []string{"--log-format", "--concurrency=9", "--concurrency", "2"}, n: 2, explicit: true},
		// Envoy ignores what follows "--", so that count is never applied.
		{args: []string{"--", "--concurrency", "2"}},
		// "--" as a value ignores nothing.
		{args: []string{"--service-node", "--", "--concurrency", "2"}, n: 2, explicit: true},
	} {
		n, explicit, err := concurrencyArg(tc.args)
		require.NoError(t, err, "%q", tc.args)
		assert.Equal(t, tc.n, n, "%q", tc.args)
		assert.Equal(t, tc.explicit, explicit, "%q", tc.args)
	}
}
