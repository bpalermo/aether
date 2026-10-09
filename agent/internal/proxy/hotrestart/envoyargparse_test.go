package hotrestart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnvoyFlagTable holds the table to itself and to the two lists that are
// written in terms of it. That it is the pinned Envoy's table is held by
// //agent/test/envoyargs, against the binary.
func TestEnvoyFlagTable(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range EnvoyFlags() {
		require.True(t, strings.HasPrefix(f.Long, "--"), f.Long)
		for _, spelling := range f.spellings() {
			assert.False(t, seen[spelling], "%s is in the table twice", spelling)
			seen[spelling] = true
			assert.Equal(t, f, *envoyFlagNamed(spelling))
		}
		if f.Short != "" {
			assert.Len(t, f.Short, 2, f.Short)
			assert.Equal(t, byte('-'), f.Short[0], f.Short)
		}
	}
	assert.Nil(t, envoyFlagNamed(""), "a flag with no short spelling must not answer to the empty string")
	assert.Nil(t, envoyFlagNamed("--concurrenc"))

	// Every spelling the check reserves or allows once is a flag of the
	// table, and the spellings of one entry are one flag: an entry that named
	// a flag Envoy does not have could never match an argument.
	for _, r := range reservedEnvoyFlags {
		assertOneFlag(t, r.spellings)
	}
	for _, o := range onceEnvoyFlags {
		assertOneFlag(t, o.spellings)
		assert.NotEqual(t, EnvoyMultiValue, envoyFlagNamed(o.spellings[0]).Kind,
			"%v: a repeatable flag cannot be once-only", o.spellings)
	}

	// EnvoyFlags hands out a copy.
	EnvoyFlags()[0].Long = "--changed"
	assert.Equal(t, envoyFlagBaseID, envoyFlags[0].Long)
}

func assertOneFlag(t *testing.T, spellings []string) {
	t.Helper()
	first := envoyFlagNamed(spellings[0])
	require.NotNil(t, first, "%s is not a flag of the pinned Envoy", spellings[0])
	for _, spelling := range spellings {
		assert.Same(t, first, envoyFlagNamed(spelling), "%v are listed as one flag", spellings)
	}
	assert.ElementsMatch(t, first.spellings(), spellings, "every spelling of the flag must be listed")
}

// parsedArg is an envoyArg with its flag as a name, for a table.
type parsedArg struct {
	item, flag, name, value string
	hasValue                bool
	problem                 argProblem
	readAsHelp              bool
}

func parsedArgs(args []string) []parsedArg {
	var out []parsedArg
	for _, a := range parseEnvoyArgs(args) {
		p := parsedArg{
			item: a.item, name: a.name, value: a.value, hasValue: a.hasValue,
			problem: a.problem, readAsHelp: a.readAsHelp,
		}
		if a.flag != nil {
			p.flag = a.flag.Long
		}
		out = append(out, p)
	}
	return out
}

// TestParseEnvoyArgs: an argument list comes back as the flags and values the
// pinned Envoy reads in it.
func TestParseEnvoyArgs(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want []parsedArg
	}{
		"nothing": {},
		"the chart's arguments": {
			args: []string{"-l", "info", "--service-node", "n1", "--concurrency", "2", "--skip-hot-restart-parent-stats"},
			want: []parsedArg{
				{item: "-l", flag: "--log-level", name: "-l", value: "info", hasValue: true},
				{item: "--service-node", flag: "--service-node", name: "--service-node", value: "n1", hasValue: true},
				{item: "--concurrency", flag: "--concurrency", name: "--concurrency", value: "2", hasValue: true},
				{item: "--skip-hot-restart-parent-stats", flag: "--skip-hot-restart-parent-stats", name: "--skip-hot-restart-parent-stats"},
			},
		},
		"a value is taken whatever it looks like": {
			args: []string{"--service-node", "-c", "--log-format", "--x=y", "--service-zone", ""},
			want: []parsedArg{
				{item: "--service-node", flag: "--service-node", name: "--service-node", value: "-c", hasValue: true},
				{item: "--log-format", flag: "--log-format", name: "--log-format", value: "--x=y", hasValue: true},
				{item: "--service-zone", flag: "--service-zone", name: "--service-zone", hasValue: true},
			},
		},
		"a switch takes nothing": {
			args: []string{"--cpuset-threads", "true"},
			want: []parsedArg{
				{item: "--cpuset-threads", flag: "--cpuset-threads", name: "--cpuset-threads"},
				{item: "true", name: "true", problem: argStray},
			},
		},
		"a flag and its value in one item": {
			args: []string{"--concurrency 2", "-l  info", "--service-node ", "n1"},
			want: []parsedArg{
				{item: "--concurrency 2", flag: "--concurrency", name: "--concurrency", value: "2", hasValue: true},
				{item: "-l  info", flag: "--log-level", name: "-l", value: " info", hasValue: true},
				{item: "--service-node ", flag: "--service-node", name: "--service-node", value: "n1", hasValue: true},
			},
		},
		"a value flag standing last": {
			args: []string{"--stats-tag", "a:b", "--stats-tag"},
			want: []parsedArg{
				{item: "--stats-tag", flag: "--stats-tag", name: "--stats-tag", value: "a:b", hasValue: true},
				{item: "--stats-tag", flag: "--stats-tag", name: "--stats-tag", problem: argMissingValue},
			},
		},
		"ignored items": {
			args: []string{"", "-", "-\a\a", "--cpuset-threads"},
			want: []parsedArg{{item: "--cpuset-threads", flag: "--cpuset-threads", name: "--cpuset-threads"}},
		},
		"the rest is ignored after --": {
			args: []string{"-l", "info", "--", "--concurrency", "2"},
			want: []parsedArg{
				{item: "-l", flag: "--log-level", name: "-l", value: "info", hasValue: true},
				{item: "--", flag: "--ignore_rest", name: "--"},
			},
		},
		"and after --ignore_rest": {
			args: []string{"--ignore_rest", "anything"},
			want: []parsedArg{{item: "--ignore_rest", flag: "--ignore_rest", name: "--ignore_rest"}},
		},
		"-- as a value ends nothing": {
			args: []string{"--service-node", "--", "--cpuset-threads"},
			want: []parsedArg{
				{item: "--service-node", flag: "--service-node", name: "--service-node", value: "--", hasValue: true},
				{item: "--cpuset-threads", flag: "--cpuset-threads", name: "--cpuset-threads"},
			},
		},
		"read as -h": {
			args: []string{"-xh", "-h", "--help"},
			want: []parsedArg{
				{item: "-xh", flag: "--help", name: "-h", readAsHelp: true},
				{item: "-h", flag: "--help", name: "-h"},
				{item: "--help", flag: "--help", name: "--help"},
			},
		},
		"TCLAP's blank in a value flag's item": {
			args: []string{"--log-format\a x"},
			want: []parsedArg{{item: "--log-format\a x", name: "--log-format\a x", problem: argUnknownFlag}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, parsedArgs(tc.args))
		})
	}
}

// TestParseEnvoyArgsOneItem is what one item on its own is read as. The first
// seventeen cases are TestSplitEnvoyArg's, which this replaces (#1443): the
// same items, read by the parser that took splitEnvoyArg's place.
func TestParseEnvoyArgsOneItem(t *testing.T) {
	for _, tc := range []struct {
		item string
		want *parsedArg
	}{
		{"--concurrency", &parsedArg{flag: "--concurrency", name: "--concurrency", problem: argMissingValue}},
		{"--concurrency=2", &parsedArg{flag: "--concurrency", name: "--concurrency", value: "2", hasValue: true, problem: argEqualsSpelling}},
		{"--ignore_rest=1", &parsedArg{flag: "--ignore_rest", name: "--ignore_rest", value: "1", hasValue: true, problem: argEqualsSpelling}},
		{"-l=info", &parsedArg{flag: "--log-level", name: "-l", value: "info", hasValue: true, problem: argEqualsSpelling}},
		{"-linfo", &parsedArg{flag: "--log-level", name: "-l", value: "info", hasValue: true, problem: argGluedSpelling}},
		{"-c/etc/envoy.yaml", &parsedArg{flag: "--config-path", name: "-c", value: "/etc/envoy.yaml", hasValue: true, problem: argGluedSpelling}},
		{"-l", &parsedArg{flag: "--log-level", name: "-l", problem: argMissingValue}},
		{"--", &parsedArg{flag: "--ignore_rest", name: "--"}},
		// Envoy accepts a lone dash and an empty item and ignores them.
		{"-", nil},
		{"", nil},
		{"-1", &parsedArg{name: "-1", problem: argUnknownFlag}},
		// "-x" is no flag of Envoy's, so there is nothing to respell.
		{"-x=y z", &parsedArg{name: "-x", problem: argUnknownFlag}},
		{"a=b", &parsedArg{name: "a=b", problem: argStray}},
		{"--=x", &parsedArg{name: "--=x", problem: argUnknownFlag}},
		{"- a=b", &parsedArg{name: "- a=b", problem: argUnknownFlag}},
		{"--a b=c", &parsedArg{name: "--a b=c", problem: argUnknownFlag}},
		{"---x=1", &parsedArg{name: "---x=1", problem: argUnknownFlag}},

		// An "h" behind a single dash is Envoy's -h, whatever was meant.
		{"-lwhatever", &parsedArg{flag: "--log-level", name: "-l", value: "whatever", hasValue: true, problem: argGluedSpelling, readAsHelp: true}},
		{"-l=bash", &parsedArg{flag: "--log-level", name: "-l", value: "bash", hasValue: true, problem: argEqualsSpelling, readAsHelp: true}},
		{"-oh", &parsedArg{flag: "--help", name: "-h", readAsHelp: true}},
		{"-hx", &parsedArg{flag: "--help", name: "-h", readAsHelp: true}},
		{"-\ah", &parsedArg{flag: "--help", name: "-h", readAsHelp: true}},
		// Not with a space in the item, and not behind two dashes.
		{"-h x", &parsedArg{name: "-h x", problem: argUnknownFlag}},
		{"--oh", &parsedArg{name: "--oh", problem: argUnknownFlag}},
		// A switch shares its item with nothing.
		{"--version ", &parsedArg{name: "--version ", problem: argUnknownFlag}},
		{"--cpuset-threads=true", &parsedArg{flag: "--cpuset-threads", name: "--cpuset-threads", value: "true", hasValue: true, problem: argEqualsSpelling}},
		// A space as one of the first two characters does not split the item.
		{"- x", &parsedArg{name: "- x", problem: argUnknownFlag}},
		{" ", &parsedArg{name: " ", problem: argStray}},
		{"x y", &parsedArg{name: "x y", problem: argStray}},
	} {
		got := parsedArgs([]string{tc.item})
		if tc.want == nil {
			assert.Empty(t, got, "%q", tc.item)
			continue
		}
		want := *tc.want
		want.item = tc.item
		assert.Equal(t, []parsedArg{want}, got, "%q", tc.item)
	}
}

func TestIsEnvoyFlagToken(t *testing.T) {
	for _, s := range []string{"-l", "--log-level", "--ignore_rest", "-1", "--a-b_c9"} {
		assert.True(t, isEnvoyFlagToken(s), s)
	}
	for _, s := range []string{"", "-", "--", "a", "---x", "--a b", "- a", "---", "--_a"} {
		assert.False(t, isEnvoyFlagToken(s), s)
	}
}
