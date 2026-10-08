package hotrestart

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// The Envoy command-line flags the supervisor passes itself. buildEnvoyCmd and
// validateArgs spell them through these constants, and reservedEnvoyFlags below
// lists every one of them, so the argv the supervisor builds and the argv it
// refuses from an operator are the same list (issue #1376).
// TestEveryFlagTheSupervisorPassesIsReserved fails when one is passed without
// being reserved.
const (
	envoyFlagConfigPath         = "-c"
	envoyFlagConfigPathLong     = "--config-path"
	envoyFlagBaseID             = "--base-id"
	envoyFlagRestartEpoch       = "--restart-epoch"
	envoyFlagDrainTime          = "--drain-time-s"
	envoyFlagParentShutdownTime = "--parent-shutdown-time-s"
	envoyFlagAdminAddressPath   = "--admin-address-path"
	envoyFlagMode               = "--mode"

	// envoyFlagConcurrency is NOT reserved: the chart passes it through
	// --envoy-arg on purpose (proxy.concurrency) and concurrencyArg reads it
	// back. It is in onceEnvoyFlags: refused when it is given more than once,
	// and its value is checked (checkConcurrencyValue).
	envoyFlagConcurrency = "--concurrency"
)

// Envoy flags the supervisor does not pass, but refuses in ExtraArgs because
// each one defeats something the supervisor controls (issue #1409). What each
// one does was read in the pinned Envoy's source (source/server/options_impl.cc,
// source/exe/stripped_main_base.cc) and measured on the pinned binary; the
// reasons are in reservedEnvoyFlags.
const (
	envoyFlagUseDynamicBaseID  = "--use-dynamic-base-id"
	envoyFlagDisableHotRestart = "--disable-hot-restart"
	envoyFlagSocketPath        = "--socket-path"
	envoyFlagHotRestartVersion = "--hot-restart-version"
	envoyFlagVersion           = "--version"
	envoyFlagHelp              = "--help"
	envoyFlagHelpShort         = "-h"
	envoyFlagIgnoreRest        = "--"
	envoyFlagIgnoreRestLong    = "--ignore_rest"
)

// Envoy flags the chart passes through --envoy-arg, besides --concurrency.
// Each is allowed once (onceEnvoyFlags).
const (
	envoyFlagLogLevel            = "--log-level"
	envoyFlagLogLevelShort       = "-l"
	envoyFlagServiceCluster      = "--service-cluster"
	envoyFlagServiceNode         = "--service-node"
	envoyFlagServiceZone         = "--service-zone"
	envoyFlagDrainStrategy       = "--drain-strategy"
	envoyFlagSkipHotRestartStats = "--skip-hot-restart-parent-stats"
)

// reservedEnvoyFlag is one Envoy flag an operator may not pass through
// ExtraArgs (the supervisor's --envoy-arg).
type reservedEnvoyFlag struct {
	// spellings are all the names Envoy accepts for the flag. Envoy counts a
	// short and a long name as the same flag.
	spellings []string
	// owner says why the flag is refused and what an operator should change
	// instead. For a flag the supervisor passes it names who sets it.
	owner string
	// conflict marks a flag the supervisor does NOT pass. It is refused
	// because of what it does to a handoff, not because it would be a repeat;
	// owner then holds that reason. TestReservedFlagsAreAllPassedOrAliases
	// checks the mark against the command lines the supervisor builds.
	conflict bool
}

// reservedEnvoyFlags is every Envoy flag CheckExtraArgs refuses outright. It is
// the one list, in two parts.
//
// The first part is every flag the supervisor sets itself. The pinned Envoy
// refuses a flag given twice ("PARSE ERROR: Argument: (--base-id) Argument
// already set!"), so a second one in ExtraArgs would fail every fork of every
// epoch.
//
// The second part (conflict) is flags the supervisor does not pass but that
// break what it controls: Envoy would start, and the next handoff would not
// work, or Envoy would not serve at all (issue #1409).
var reservedEnvoyFlags = []reservedEnvoyFlag{
	{
		spellings: []string{envoyFlagConfigPath, envoyFlagConfigPathLong},
		owner: "the supervisor passes the bootstrap path itself, from its own --config " +
			"(the chart mounts the proxy ConfigMap there and has no value for the path)",
	},
	{
		spellings: []string{envoyFlagBaseID},
		owner:     "the supervisor passes it itself, from its own --base-id (chart value proxy.hotRestart.baseId)",
	},
	{
		spellings: []string{envoyFlagRestartEpoch},
		owner: "the supervisor numbers the hot-restart epochs itself and passes each child its own; " +
			"there is no option or chart value for it",
	},
	{
		spellings: []string{envoyFlagDrainTime},
		owner:     "the supervisor passes it itself, from its own --drain-time (chart value proxy.hotRestart.drainTime)",
	},
	{
		spellings: []string{envoyFlagParentShutdownTime},
		owner: "the supervisor passes it itself, from its own --parent-shutdown-time " +
			"(chart value proxy.hotRestart.parentShutdownTime)",
	},
	{
		spellings: []string{envoyFlagAdminAddressPath},
		owner: "the supervisor sets it to its own admin identity, which keeps a drain off another " +
			"pod's Envoy (#1127); there is no option or chart value for it",
	},
	{
		spellings: []string{envoyFlagMode},
		owner: "the supervisor always serves, and adds --mode validate itself when it checks a " +
			"changed bootstrap; there is no option or chart value for it",
	},

	// Not passed by the supervisor; refused for what they do to a handoff.
	{
		// stripped_main_base.cc: with this flag Envoy ignores --base-id and
		// picks a random one, and options_impl.cc refuses the flag at any
		// epoch above 0. Measured with --base-id 4242 on the command line: the
		// epoch-0 Envoy chose base id 76596540 (--base-id-path wrote it; its
		// shared memory was /dev/shm/envoy_shared_memory_765965400), its
		// epoch-1 successor exited on the error quoted below, and a successor
		// without the flag looped on "hot restart sendmsg() connection
		// refused".
		spellings: []string{envoyFlagUseDynamicBaseID},
		conflict:  true,
		owner: "it makes Envoy ignore the fixed --base-id the supervisor passes and pick a random one, " +
			"so no successor can find this Envoy's shared memory and hot-restart socket, and Envoy " +
			"refuses the flag at any restart epoch above 0 (\"cannot use --restart-epoch=1 with " +
			"--use-dynamic-base-id\"), so every hot restart would fail. The base id is the " +
			"supervisor's --base-id (chart value proxy.hotRestart.baseId)",
	},
	{
		// Measured: with the flag on both, the epoch-1 Envoy went LIVE next
		// to the epoch-0 one, which was never drained or stopped.
		spellings: []string{envoyFlagDisableHotRestart},
		conflict:  true,
		owner: "the supervisor's whole job is the hot restart: with it a successor never contacts its " +
			"predecessor, so the old Envoy is neither drained nor stopped and no socket is handed over",
	},
	{
		// Measured: a successor on another path looped on "hot restart
		// sendmsg() connection refused" and never initialized.
		spellings: []string{envoyFlagSocketPath},
		conflict:  true,
		owner: "a successor reaches its predecessor on Envoy's default hot-restart socket, an abstract " +
			"one in the host network namespace every proxy pod shares. A predecessor and a successor " +
			"that disagree on the path cannot hand off (\"hot restart sendmsg() connection refused\"), " +
			"which is what the roll that adds or changes the flag does, and a path on a pod's own " +
			"filesystem is not shared with the next pod. There is no option or chart value for it",
	},
	{
		// options_impl.cc throws NoServingException after printing.
		spellings: []string{envoyFlagHotRestartVersion},
		conflict:  true,
		owner:     "Envoy prints its hot-restart version and exits 0 without serving, on every fork",
	},
	{
		spellings: []string{envoyFlagVersion},
		conflict:  true,
		owner:     "Envoy prints its version and exits 0 without serving, on every fork",
	},
	{
		spellings: []string{envoyFlagHelpShort, envoyFlagHelp},
		conflict:  true,
		owner:     "Envoy prints its usage and exits 0 without serving, on every fork",
	},
	{
		// Measured: "-- --concurrency 1" ran the default worker count.
		spellings: []string{envoyFlagIgnoreRest, envoyFlagIgnoreRestLong},
		conflict:  true,
		owner: "Envoy ignores every argument after it, while the supervisor still reads them: a " +
			"--concurrency behind it would be compared with a predecessor's worker count and never applied",
	},
}

// onceEnvoyFlag is an Envoy flag that is allowed in ExtraArgs, once.
type onceEnvoyFlag struct {
	spellings []string
	// owner says where the one occurrence normally comes from.
	owner string
	// repeated, when set, is the error a repeat wraps.
	repeated error
}

// onceEnvoyFlags are the flags the chart passes through --envoy-arg. The pinned
// Envoy refuses every flag given twice except --stats-tag (measured for these:
// "Argument already set!", and -l with --log-level counts as twice), so an
// ExtraArgs that adds one of them to the chart's own fails every fork.
// TestChartEnvoyArgFlagsAreAllowedOnce (supervisorcmd) reads the flags off the
// chart template and fails when one is missing here.
//
// A flag in neither list is passed through unchecked: the supervisor does not
// carry Envoy's whole flag table.
var onceEnvoyFlags = []onceEnvoyFlag{
	{
		spellings: []string{envoyFlagConcurrency},
		owner:     "the chart already passes it when proxy.concurrency is set",
		repeated:  errRepeatedConcurrency,
	},
	{
		spellings: []string{envoyFlagLogLevelShort, envoyFlagLogLevel},
		owner:     "the chart already passes -l (chart value proxy.logLevel)",
	},
	{
		spellings: []string{envoyFlagServiceCluster},
		owner:     "the chart already passes it",
	},
	{
		spellings: []string{envoyFlagServiceNode},
		owner:     "the chart already passes it (the node name)",
	},
	{
		spellings: []string{envoyFlagServiceZone},
		owner:     "the chart already passes it (the node's zone)",
	},
	{
		spellings: []string{envoyFlagDrainStrategy},
		owner:     "the chart already passes it (chart value proxy.hotRestart.drainStrategy)",
	},
	{
		spellings: []string{envoyFlagSkipHotRestartStats},
		owner:     "the chart already passes it when proxy.hotRestart.skipParentStats is set",
	},
}

// errRepeatedConcurrency is concurrencyArg's error for a --concurrency given
// more than once.
var errRepeatedConcurrency = errors.New("--concurrency is given more than once")

// errConcurrencyEquals is concurrencyArg's error for the --concurrency=N
// spelling, which the pinned Envoy does not accept (issue #1407).
var errConcurrencyEquals = errors.New("--concurrency=N is not a spelling Envoy accepts")

// argSpelling is how one ExtraArgs item spells a flag.
type argSpelling int

const (
	// spelledPlain is a flag alone in its item ("--concurrency"), or an item
	// that is not a flag at all.
	spelledPlain argSpelling = iota
	// spelledEquals is "--flag=value" or "-f=value".
	spelledEquals
	// spelledGlued is a value glued to a short flag: "-linfo".
	spelledGlued
)

// splitEnvoyArg returns the flag name an ExtraArgs item carries, the value
// written into the same item (if any) and how it is spelled. An item that is
// not shaped like a flag comes back unchanged as spelledPlain.
//
// The pinned Envoy accepts one spelling only: the flag in one argument and its
// value in the next. Measured on 17 flags, long and short ("--concurrency=2",
// "--service-node=n1", "--log-level=info", "-l=info", "-linfo", ...): each is
// "PARSE ERROR: Argument: <item> Couldn't find match for argument", while
// "-l info" and "--log-level info" are accepted (issue #1407).
func splitEnvoyArg(a string) (name, value string, spelling argSpelling) {
	if n, v, ok := strings.Cut(a, "="); ok && isEnvoyFlagToken(n) {
		return n, v, spelledEquals
	}
	// Envoy's short flags that take a value are -l and -c.
	if len(a) > 2 && a[0] == '-' && a[1] != '-' {
		switch short := a[:2]; short {
		case envoyFlagLogLevelShort, envoyFlagConfigPath:
			return short, a[2:], spelledGlued
		}
	}
	return a, "", spelledPlain
}

// isEnvoyFlagToken reports whether s is shaped like a flag name: one or two
// dashes, then letters, digits, dashes and underscores, starting with a letter
// or a digit.
func isEnvoyFlagToken(s string) bool {
	rest := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "-")
	if rest == s || rest == "" {
		return false
	}
	for i, c := range rest {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '-' || c == '_'):
		default:
			return false
		}
	}
	return true
}

// CheckExtraArgs refuses a Config.ExtraArgs (the supervisor's --envoy-arg) that
// cannot give a working Envoy. Either the pinned Envoy rejects the command line
// on every fork, or it starts and the next handoff breaks:
//
//   - a flag in reservedEnvoyFlags: one the supervisor passes itself, or one
//     that defeats what the supervisor controls (issues #1376, #1409);
//   - a flag in onceEnvoyFlags given more than once (#1375). One is allowed:
//     it is how the chart's values reach Envoy;
//   - a spelling the pinned Envoy does not accept: "--flag=value", "-f=value"
//     or "-fvalue" (#1407). The error shows the two-item form;
//   - a --concurrency whose value is missing or is not a plain number (#1408).
//
// It is meant to run once, before the first fork, so the mistake is a startup
// error naming the flag instead of a supervisor that fails every fork.
//
// Every argument is compared, because the check does not carry Envoy's whole
// flag table and so does not know which flags take a value. Envoy does: it
// reads the argument after a value flag as that value, whatever it looks like.
// So a flag's value that is itself spelled like a refused argument (a
// --service-node named "-c", a --log-format that is "--x=y") is refused too,
// although Envoy would take it.
func CheckExtraArgs(args []string) error {
	if err := checkReservedArgs(args); err != nil {
		return err
	}
	if err := checkRepeatedArgs(args); err != nil {
		return err
	}
	if err := checkArgSpellings(args); err != nil {
		return err
	}
	if _, _, err := concurrencyArg(args); err != nil {
		return fmt.Errorf("--envoy-arg %w. Pass the flag and a whole number of workers as two items "+
			"(--envoy-arg=--concurrency --envoy-arg=2); the chart does when proxy.concurrency is set", err)
	}
	return nil
}

// checkReservedArgs refuses a flag in reservedEnvoyFlags, in any spelling.
func checkReservedArgs(args []string) error {
	for _, a := range args {
		name, _, _ := splitEnvoyArg(a)
		for _, r := range reservedEnvoyFlags {
			if !slices.Contains(r.spellings, name) {
				continue
			}
			if r.conflict {
				return fmt.Errorf("--envoy-arg %s is reserved: %s", a, r.owner)
			}
			return fmt.Errorf("--envoy-arg %s is reserved: %s. Envoy refuses a flag given twice, "+
				"so every fork would fail", a, r.owner)
		}
	}
	return nil
}

// checkRepeatedArgs refuses a flag in onceEnvoyFlags given more than once, in
// any mix of its spellings.
func checkRepeatedArgs(args []string) error {
	for _, o := range onceEnvoyFlags {
		seen := 0
		for _, a := range args {
			if name, _, _ := splitEnvoyArg(a); slices.Contains(o.spellings, name) {
				seen++
			}
		}
		if seen < 2 {
			continue
		}
		err := o.repeated
		if err == nil {
			err = fmt.Errorf("%s is given more than once", strings.Join(o.spellings, " / "))
		}
		return fmt.Errorf("--envoy-arg %w (%d times): Envoy refuses a flag given twice, so every fork would fail. "+
			"Pass it once; %s", err, seen, o.owner)
	}
	return nil
}

// checkArgSpellings refuses an item that carries a flag and its value
// together, which the pinned Envoy does not parse.
func checkArgSpellings(args []string) error {
	for _, a := range args {
		name, value, spelling := splitEnvoyArg(a)
		if spelling == spelledPlain {
			continue
		}
		what := `the "flag=value" spelling, for any flag`
		if spelling == spelledGlued {
			what = "a value glued to a short flag"
		}
		if value == "" {
			value = "<value>"
		}
		return fmt.Errorf("--envoy-arg %s: the pinned Envoy does not accept %s (it answers \"Couldn't find "+
			"match for argument\"), so every fork would fail. Pass the flag and its value as two items: "+
			"--envoy-arg=%s --envoy-arg=%s (a flag that takes no value goes alone)", a, what, name, value)
	}
	return nil
}

// maxConcurrency bounds a --concurrency value. Envoy reads a uint32; nothing
// near that many worker threads can start.
const maxConcurrency = math.MaxInt32

// parseConcurrencyValue turns the argument after --concurrency into the worker
// count Envoy will run with, or says why the supervisor cannot know it.
//
// Measured on the pinned Envoy (issue #1408):
//
//   - "x", "2x", "1.5", "0x2", "2 ", "4294967296", another flag ("-l",
//     "--skip-hot-restart-parent-stats"): refused, "Couldn't read argument
//     value from string '<value>'". Envoy reads the next argument as the value
//     whatever it looks like.
//   - "0": accepted, and Envoy runs ONE worker (options_impl.cc:
//     concurrency_ = std::max(1U, value); /server_info reported concurrency 1
//     and the process had one worker thread). It is not "one per core". So 0
//     is 1 here.
//   - "": accepted, and Envoy runs its default count as if the flag were
//     absent. Refused here: the handoff check would compare the wrong number.
//   - "-1": accepted. Envoy reads it as an unsigned number, 4294967295 workers,
//     and did not finish starting. Refused here.
//   - "+2", " 2": accepted as 2. Refused here: only digits are taken, so the
//     supervisor never has to read a number the way a C++ stream does. "02"
//     is 2 in both.
func parseConcurrencyValue(v string) (int, error) {
	switch {
	case v == "":
		return 0, errors.New("--concurrency has an empty value: Envoy would run its default worker count " +
			"as if the flag were absent")
	case strings.HasPrefix(v, "-"):
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return 0, fmt.Errorf("--concurrency %q is negative: Envoy does not refuse it, it reads it as an "+
				"unsigned number (-1 becomes 4294967295 workers)", v)
		}
		return 0, fmt.Errorf("--concurrency is followed by %q, which Envoy reads as its value and refuses "+
			"(\"Couldn't read argument value\"): the value is missing or is not a number", v)
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("--concurrency %q is not a whole number written in digits only", v)
		}
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n > maxConcurrency {
		return 0, fmt.Errorf("--concurrency %q is out of range (at most %d)", v, maxConcurrency)
	}
	if n == 0 {
		// Envoy runs one worker for 0.
		return 1, nil
	}
	return int(n), nil
}
