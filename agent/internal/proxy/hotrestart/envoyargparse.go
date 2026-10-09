package hotrestart

import "strings"

// argProblem is why the pinned Envoy does not take an argument as it is
// written. argOK covers an argument Envoy takes, including a flag the
// supervisor then refuses for another reason (reserved, repeated).
type argProblem int

const (
	argOK argProblem = iota
	// argEqualsSpelling is "--flag=value" or "-f=value" of a flag Envoy has.
	argEqualsSpelling
	// argGluedSpelling is a value glued to a short flag: "-linfo".
	argGluedSpelling
	// argMissingValue is a flag that takes a value, standing last.
	argMissingValue
	// argUnknownFlag starts with a dash and is no flag of the pinned Envoy.
	argUnknownFlag
	// argStray is not a flag, and no flag before it takes it as its value.
	argStray
)

// envoyArg is one argument as the pinned Envoy reads it: a flag with the value
// it takes, or something Envoy has no match for.
type envoyArg struct {
	// item is the ExtraArgs item the flag (or the unmatched argument) is in.
	item string
	// flag is the Envoy flag the argument names, or nil when it names none.
	// It is set for a misspelled argument too ("--concurrency=2"): what the
	// operator meant counts when looking for a reserved or a repeated flag.
	flag *EnvoyFlag
	// name is the flag as it is spelled in item ("-l", "--log-level"). For an
	// unknown flag it is the part of item before any "=".
	name string
	// value is the flag's value and hasValue whether one was found: in the
	// next item, or in the same item after a space, "=" or a short flag.
	value    string
	hasValue bool
	problem  argProblem
	// readAsHelp marks a single-dash item with an "h" in it. Envoy reads that
	// "h" as its -h switch, prints its usage and exits 0, or refuses the item
	// when it holds more than one ("-h (--help) Argument already set!").
	readAsHelp bool
}

// tclapBlank is the character TCLAP overwrites a matched short switch with
// inside a combined item. An item that is a dash followed only by them is
// accepted and ignored, and a value flag in an item that holds one is not
// matched.
const tclapBlank = '\a'

// parseEnvoyArgs reads an Envoy argument list the way the pinned Envoy's
// parser does (TCLAP with a space as the value delimiter; issue #1443). It
// never fails: what Envoy would refuse comes back as an envoyArg with a
// problem, in the order Envoy meets it.
//
// The rules, each measured on the pinned binary (//agent/test/envoyargs runs
// them again):
//
//   - A switch is one item, spelled exactly.
//   - A flag that takes a value is followed by its value in the next item,
//     whatever that item looks like: "--service-node -c" names a node "-c",
//     and "--log-format --x=y" is a format. The value is never read as a flag.
//   - The flag and its value may also share one item, split by a space:
//     "--concurrency 2". Envoy accepts it. "--flag=value", "-f=value" and
//     "-fvalue" it does not ("Couldn't find match for argument").
//   - A flag that takes a value and stands last has none ("Missing a value
//     for this argument!").
//   - An empty item and a lone "-" are accepted and ignored.
//   - A single-dash item with an "h" in it and no space is read as -h.
//   - Everything after "--" or "--ignore_rest" is ignored.
//   - Anything else is refused ("Couldn't find match for argument"): a flag
//     Envoy does not have, and an argument that is not a flag at all.
func parseEnvoyArgs(args []string) []envoyArg {
	var out []envoyArg
	for i := 0; i < len(args); i++ {
		item := args[i]
		if ignoredEnvoyItem(item) {
			continue
		}
		if f := envoyFlagNamed(item); f != nil && !f.takesValue() {
			out = append(out, envoyArg{item: item, flag: f, name: item})
			if f.Long == envoyFlagIgnoreRestLong {
				return out
			}
			continue
		}
		if f, name, inline := valueFlagIn(item); f != nil {
			a := envoyArg{item: item, flag: f, name: name}
			switch {
			case inline != "":
				a.value, a.hasValue = inline, true
			case i+1 < len(args):
				i++
				a.value, a.hasValue = args[i], true
			default:
				a.problem = argMissingValue
			}
			out = append(out, a)
			continue
		}
		out = append(out, unmatchedEnvoyArg(item))
	}
	return out
}

// ignoredEnvoyItem reports whether Envoy accepts item and does nothing with
// it: the empty string, and a dash followed only by TCLAP's blank character.
func ignoredEnvoyItem(item string) bool {
	if item == "" {
		return true
	}
	return item[0] == '-' && strings.Trim(item[1:], string(tclapBlank)) == ""
}

// valueFlagIn returns the flag that takes a value which item names, how it is
// spelled, and the value written after a space in the same item (empty when
// the value is the next item). TCLAP splits an item at its first space, if
// that is not one of its first two characters.
func valueFlagIn(item string) (f *EnvoyFlag, name, inline string) {
	if strings.ContainsRune(item, tclapBlank) {
		return nil, "", ""
	}
	name = item
	if at := strings.IndexByte(item, ' '); at > 1 {
		name, inline = item[:at], item[at+1:]
	}
	if f = envoyFlagNamed(name); f == nil || !f.takesValue() {
		return nil, "", ""
	}
	return f, name, inline
}

// unmatchedEnvoyArg classifies an item Envoy has no match for, so the error
// can say what to write instead.
func unmatchedEnvoyArg(item string) envoyArg {
	a := envoyArg{item: item, name: item, readAsHelp: readAsHelpSwitch(item)}
	if name, value, ok := strings.Cut(item, "="); ok && isEnvoyFlagToken(name) {
		a.name = name
		if a.flag = envoyFlagNamed(name); a.flag == nil {
			a.problem = argUnknownFlag
			return a
		}
		a.value, a.hasValue, a.problem = value, true, argEqualsSpelling
		return a
	}
	if len(item) > 2 && item[1] != '-' {
		if f := envoyFlagNamed(item[:2]); f != nil && f.takesValue() {
			a.flag, a.name, a.value, a.hasValue, a.problem = f, item[:2], item[2:], true, argGluedSpelling
			return a
		}
	}
	switch {
	case a.readAsHelp:
		a.flag, a.name = envoyFlagNamed(envoyFlagHelpShort), envoyFlagHelpShort
	case item[0] == '-':
		a.problem = argUnknownFlag
	default:
		a.problem = argStray
	}
	return a
}

// readAsHelpSwitch reports whether Envoy reads item as its -h switch: TCLAP
// lets one-letter switches be combined behind one dash ("-abc"), -h is the
// only one-letter switch Envoy has, and TCLAP looks for it anywhere in a
// single-dash item that holds no space.
func readAsHelpSwitch(item string) bool {
	if len(item) < 2 || item[0] != '-' || item[1] == '-' || strings.ContainsRune(item, ' ') {
		return false
	}
	return strings.Contains(item[1:], "h")
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
