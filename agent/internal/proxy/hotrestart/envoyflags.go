package hotrestart

import "slices"

// EnvoyFlagKind is how the pinned Envoy's command-line parser reads one flag.
type EnvoyFlagKind int

const (
	// EnvoySwitch takes no value (a TCLAP::SwitchArg): the flag is one
	// argument, and the argument after it is the next flag.
	EnvoySwitch EnvoyFlagKind = iota
	// EnvoyValue takes one value and may be given once (a TCLAP::ValueArg).
	// The value is the next argument, whatever it looks like.
	EnvoyValue
	// EnvoyMultiValue takes one value per occurrence and may be repeated (a
	// TCLAP::MultiArg). --stats-tag is the only one.
	EnvoyMultiValue
)

// EnvoyFlag is one command-line flag of the pinned Envoy.
type EnvoyFlag struct {
	// Long is the flag with its two dashes: "--log-level".
	Long string
	// Short is the one-letter spelling with its dash ("-l"), or empty. Envoy
	// counts a short and a long spelling as the same flag.
	Short string
	Kind  EnvoyFlagKind
}

// takesValue reports whether the argument after the flag is its value.
func (f *EnvoyFlag) takesValue() bool { return f.Kind != EnvoySwitch }

// spellings is every way to write the flag, short one first.
func (f *EnvoyFlag) spellings() []string {
	if f.Short == "" {
		return []string{f.Long}
	}
	return []string{f.Short, f.Long}
}

// envoyFlags is every flag the pinned Envoy's command line has: the options
// source/server/options_impl.cc declares, in that order, then the three TCLAP
// adds by itself. It is what lets CheckExtraArgs read an argument list the way
// Envoy does, which needs to know which flags take a value (issue #1443).
//
// It is the pinned Envoy's list, not Envoy's in general. A pin bump that adds,
// removes or changes a flag fails TestEnvoyFlagTableMatchesThePinnedEnvoy
// (//agent/test/envoyargs), which reads the same list off `envoy --help` of the
// binary the mesh deploys.
var envoyFlags = []EnvoyFlag{
	{Long: envoyFlagBaseID, Kind: EnvoyValue},
	{Long: envoyFlagUseDynamicBaseID},
	{Long: "--skip-hot-restart-on-no-parent"},
	{Long: envoyFlagSkipHotRestartStats},
	{Long: "--base-id-path", Kind: EnvoyValue},
	{Long: envoyFlagConcurrency, Kind: EnvoyValue},
	{Long: envoyFlagConfigPathLong, Short: envoyFlagConfigPath, Kind: EnvoyValue},
	{Long: "--config-yaml", Kind: EnvoyValue},
	{Long: "--allow-unknown-fields"},
	{Long: "--allow-unknown-static-fields"},
	{Long: "--reject-unknown-dynamic-fields"},
	{Long: "--ignore-unknown-dynamic-fields"},
	{Long: "--skip-deprecated-logs"},
	{Long: "--log-stacktrace-single-entry"},
	{Long: envoyFlagAdminAddressPath, Kind: EnvoyValue},
	{Long: "--local-address-ip-version", Kind: EnvoyValue},
	{Long: envoyFlagLogLevel, Short: envoyFlagLogLevelShort, Kind: EnvoyValue},
	{Long: "--component-log-level", Kind: EnvoyValue},
	{Long: "--log-format", Kind: EnvoyValue},
	{Long: "--log-format-escaped"},
	{Long: "--enable-fine-grain-logging"},
	{Long: "--log-path", Kind: EnvoyValue},
	{Long: envoyFlagRestartEpoch, Kind: EnvoyValue},
	{Long: envoyFlagHotRestartVersion},
	{Long: envoyFlagServiceCluster, Kind: EnvoyValue},
	{Long: envoyFlagServiceNode, Kind: EnvoyValue},
	{Long: envoyFlagServiceZone, Kind: EnvoyValue},
	{Long: "--file-flush-interval-msec", Kind: EnvoyValue},
	{Long: "--file-flush-min-size-kb", Kind: EnvoyValue},
	{Long: envoyFlagDrainTime, Kind: EnvoyValue},
	{Long: envoyFlagDrainStrategy, Kind: EnvoyValue},
	{Long: envoyFlagParentShutdownTime, Kind: EnvoyValue},
	{Long: envoyFlagMode, Kind: EnvoyValue},
	{Long: envoyFlagDisableHotRestart},
	{Long: "--enable-mutex-tracing"},
	{Long: "--cpuset-threads"},
	{Long: "--disable-extensions", Kind: EnvoyValue},
	{Long: envoyFlagSocketPath, Kind: EnvoyValue},
	{Long: "--socket-mode", Kind: EnvoyValue},
	{Long: "--enable-core-dump"},
	{Long: "--stats-tag", Kind: EnvoyMultiValue},

	// Added by TCLAP itself.
	{Long: envoyFlagIgnoreRestLong, Short: envoyFlagIgnoreRest},
	{Long: envoyFlagVersion},
	{Long: envoyFlagHelp, Short: envoyFlagHelpShort},
}

// EnvoyFlags returns the pinned Envoy's command-line flags as the --envoy-arg
// check knows them. //agent/test/envoyargs holds the list against the binary.
func EnvoyFlags() []EnvoyFlag {
	return slices.Clone(envoyFlags)
}

// envoyFlagNamed returns the flag one of whose spellings is exactly name, or
// nil. The pointer is into envoyFlags, so two results can be compared.
func envoyFlagNamed(name string) *EnvoyFlag {
	if name == "" {
		return nil
	}
	for i := range envoyFlags {
		if f := &envoyFlags[i]; f.Long == name || f.Short == name {
			return f
		}
	}
	return nil
}
