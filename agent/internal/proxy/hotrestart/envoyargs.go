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
		owner: "Envoy ignores every argument after it, so nothing written behind it is applied, and " +
			"it is not refused when it is wrong either: a --concurrency there would never take effect",
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
//
// checkRepeatedArgs refuses a repeat of any flag, in this list or not. What an
// entry adds is the owner: the error says the first occurrence is the chart's,
// which an operator who wrote only one cannot see otherwise.
// TestChartEnvoyArgFlagsAreAllowedOnce (supervisorcmd) reads the flags off the
// chart template and fails when one has no entry here.
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

// CheckExtraArgs refuses a Config.ExtraArgs (the supervisor's --envoy-arg) that
// cannot give a working Envoy. Either the pinned Envoy rejects the command line
// on every fork, or it starts and the next handoff breaks.
//
// The list is read the way the pinned Envoy reads it (parseEnvoyArgs, issue
// #1443): a flag, then its value when the flag takes one. So a value is never
// mistaken for a flag (a --service-node named "-c" is a node name), and a flag
// is found wherever it stands and however it is spelled ("--socket-path @x" in
// one item included). Refused, in this order:
//
//   - a flag in reservedEnvoyFlags: one the supervisor passes itself, or one
//     that defeats what the supervisor controls (issues #1376, #1409);
//   - any flag given more than once, except --stats-tag (#1375). The flags the
//     chart passes (onceEnvoyFlags) are therefore allowed once and no more;
//   - then the first argument, in order, that Envoy does not take: a spelling
//     it does not parse ("--flag=value", "-f=value", "-fvalue"; #1407), a flag
//     it does not have, an argument that is no flag and no flag's value, a
//     flag whose value is missing, and a --concurrency value it refuses or
//     reads as a count the supervisor cannot know (#1408).
//
// It is meant to run once, before the first fork, so the mistake is a startup
// error naming the argument instead of a supervisor that fails every fork.
//
// What it does not check is what a value means: a log level Envoy does not
// know, a number that is not one for a flag other than --concurrency. Those
// still fail at the fork.
func CheckExtraArgs(args []string) error {
	parsed := parseEnvoyArgs(args)
	if err := checkReservedArgs(parsed); err != nil {
		return err
	}
	if err := checkRepeatedArgs(parsed); err != nil {
		return err
	}
	for i := range parsed {
		if err := checkEnvoyArg(&parsed[i]); err != nil {
			return err
		}
	}
	return nil
}

// reservedEntry returns the reservedEnvoyFlags entry for f, or nil.
func reservedEntry(f *EnvoyFlag) *reservedEnvoyFlag {
	for i := range reservedEnvoyFlags {
		if r := &reservedEnvoyFlags[i]; slices.Contains(r.spellings, f.Long) {
			return r
		}
	}
	return nil
}

// onceEntry returns the onceEnvoyFlags entry for f, or nil.
func onceEntry(f *EnvoyFlag) *onceEnvoyFlag {
	for i := range onceEnvoyFlags {
		if o := &onceEnvoyFlags[i]; slices.Contains(o.spellings, f.Long) {
			return o
		}
	}
	return nil
}

// checkReservedArgs refuses a flag in reservedEnvoyFlags, in any spelling.
func checkReservedArgs(parsed []envoyArg) error {
	for _, a := range parsed {
		if a.flag == nil {
			continue
		}
		r := reservedEntry(a.flag)
		if r == nil {
			continue
		}
		owner := r.owner
		if a.readAsHelp && a.problem == argOK {
			owner = `Envoy reads an "h" in a single-dash argument as its -h switch. ` + owner
		}
		if r.conflict {
			return fmt.Errorf("--envoy-arg %s is reserved: %s", a.item, owner)
		}
		return fmt.Errorf("--envoy-arg %s is reserved: %s. Envoy refuses a flag given twice, "+
			"so every fork would fail", a.item, owner)
	}
	return nil
}

// checkRepeatedArgs refuses a flag given more than once, in any mix of its
// spellings. The pinned Envoy refuses every repeat except --stats-tag's.
func checkRepeatedArgs(parsed []envoyArg) error {
	seen := make(map[*EnvoyFlag]int)
	for _, a := range parsed {
		if a.flag != nil && a.flag.Kind != EnvoyMultiValue {
			seen[a.flag]++
		}
	}
	// In the order the flags first appear, so the error does not depend on
	// map iteration.
	for _, a := range parsed {
		if seen[a.flag] < 2 {
			continue
		}
		err := fmt.Errorf("%s is given more than once", strings.Join(a.flag.spellings(), " / "))
		advice := "Pass it once"
		if o := onceEntry(a.flag); o != nil {
			if o.repeated != nil {
				err = o.repeated
			}
			advice += "; " + o.owner
		}
		return fmt.Errorf("--envoy-arg %w (%d times): Envoy refuses a flag given twice, so every fork would fail. %s",
			err, seen[a.flag], advice)
	}
	return nil
}

// noMatch is how the pinned Envoy answers an argument it has no flag for.
const noMatch = `it answers "Couldn't find match for argument"`

// checkEnvoyArg refuses one argument the pinned Envoy does not take as it is
// written.
func checkEnvoyArg(a *envoyArg) error {
	switch a.problem {
	case argEqualsSpelling, argGluedSpelling:
		return spellingError(a)
	case argUnknownFlag:
		return fmt.Errorf("--envoy-arg %q: the pinned Envoy has no flag %s (%s), so every fork would fail. "+
			"Its flags are the ones `envoy --help` lists; a flag and its value are two items",
			a.item, a.name, noMatch)
	case argStray:
		return fmt.Errorf("--envoy-arg %q is not a flag, and the argument before it takes no value (%s), "+
			"so every fork would fail. A flag that takes no value goes alone; a flag and its value are "+
			"two items in that order: --envoy-arg=<flag> --envoy-arg=<value>", a.item, noMatch)
	}
	if a.flag == nil {
		return nil
	}
	if a.flag.Long == envoyFlagConcurrency {
		if _, err := concurrencyValue(a); err != nil {
			return fmt.Errorf("--envoy-arg %w. Pass the flag and a whole number of workers as two items "+
				"(--envoy-arg=--concurrency --envoy-arg=2); the chart does when proxy.concurrency is set", err)
		}
		return nil
	}
	if a.problem == argMissingValue {
		return fmt.Errorf("--envoy-arg %s is the last argument and has no value (Envoy: \"Missing a value for "+
			"this argument!\"), so every fork would fail. Pass its value as the next item: "+
			"--envoy-arg=%s --envoy-arg=<value>", a.item, a.name)
	}
	return nil
}

// spellingError is the error for an item that carries a flag and its value in
// a way the pinned Envoy does not parse. It shows what to write instead.
func spellingError(a *envoyArg) error {
	what := `the "flag=value" spelling, for any flag`
	if a.problem == argGluedSpelling {
		what = "a value glued to a short flag"
	}
	answer := noMatch
	if a.readAsHelp {
		// Measured: "-lwhatever" printed the usage and exited 0.
		answer = `it reads an "h" in a single-dash argument as its -h switch, so it prints its usage ` +
			`instead of serving or refuses the argument; without the "h" ` + noMatch
	}
	refused := fmt.Sprintf("--envoy-arg %s: the pinned Envoy does not accept %s (%s), so every fork would fail",
		a.item, what, answer)
	if !a.flag.takesValue() {
		return fmt.Errorf("%s. %s takes no value: pass it alone, as the one item --envoy-arg=%s", refused, a.name, a.name)
	}
	value := a.value
	if value == "" {
		value = "<value>"
	}
	return fmt.Errorf("%s. Pass the flag and its value as two items: --envoy-arg=%s --envoy-arg=%s",
		refused, a.name, value)
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
