package hotrestart

import (
	"errors"
	"fmt"
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
	// back. It is only refused when it is given more than once.
	envoyFlagConcurrency = "--concurrency"
)

// reservedEnvoyFlag is one Envoy flag an operator may not pass through
// ExtraArgs (the supervisor's --envoy-arg).
type reservedEnvoyFlag struct {
	// spellings are all the names Envoy accepts for the flag. Envoy counts a
	// short and a long name as the same flag.
	spellings []string
	// owner says who sets the flag and what an operator should change instead.
	owner string
}

// reservedEnvoyFlags is every Envoy flag the supervisor sets itself. The pinned
// Envoy refuses a flag given twice ("PARSE ERROR: Argument: (--base-id)
// Argument already set!"), so a second one in ExtraArgs would fail every fork
// of every epoch. CheckExtraArgs refuses it once, at startup, instead.
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
}

// errRepeatedConcurrency is concurrencyArg's error for a --concurrency given
// more than once.
var errRepeatedConcurrency = errors.New("--concurrency is given more than once")

// CheckExtraArgs refuses a Config.ExtraArgs (the supervisor's --envoy-arg) that
// can only produce an Envoy command line the pinned Envoy rejects on every
// fork:
//
//   - a flag the supervisor passes itself (reservedEnvoyFlags), in either the
//     "--flag value" or the "--flag=value" spelling;
//   - --concurrency given more than once. One --concurrency is allowed: it is
//     how the chart's proxy.concurrency reaches Envoy.
//
// It is meant to run once, before the first fork, so the mistake is a startup
// error naming the flag instead of a supervisor that fails every fork.
//
// Every argument is compared, because the check does not know which Envoy flags
// take a value. So a flag's value that is itself spelled exactly like a
// reserved flag (say a --service-node named "-c") is refused too.
func CheckExtraArgs(args []string) error {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		for _, r := range reservedEnvoyFlags {
			for _, spelling := range r.spellings {
				if name == spelling {
					return fmt.Errorf("--envoy-arg %s is reserved: %s. Envoy refuses a flag given twice, "+
						"so every fork would fail", a, r.owner)
				}
			}
		}
	}
	if _, _, err := concurrencyArg(args); errors.Is(err, errRepeatedConcurrency) {
		return fmt.Errorf("--envoy-arg %w: Envoy refuses a flag given twice, so every fork would fail. "+
			"Pass it once; the chart already passes it when proxy.concurrency is set", err)
	}
	return nil
}
