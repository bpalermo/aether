package harnesscontract

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// MustLoad loads the contract or fails the test.
func MustLoad(tb testing.TB) *Contract {
	tb.Helper()
	c, err := Load()
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return c
}

// Errorf reports a difference between the code and the contract. Every such
// failure names the contract file and the rule, so whoever renamed something
// learns from the test log alone that a file outside the package has to change
// with it.
func Errorf(tb testing.TB, format string, args ...any) {
	tb.Helper()
	tb.Errorf("%s\n%s", fmt.Sprintf(format, args...), Rule)
}

// Owns asserts that the entries whose checked_by is target are exactly ids:
// the test that calls it declares which entries it holds to the code.
//
// Both directions matter. An entry assigned to this test that the test does
// not list is a promise nothing checks. An id the test lists that the contract
// no longer has was removed from the contract while the code that emits it is
// still here: removing an entry is a version bump, and the check goes with it.
func (c *Contract) Owns(tb testing.TB, target string, ids ...string) {
	tb.Helper()
	got := c.IDs(target)
	for _, id := range ids {
		if !slices.Contains(got, id) {
			Errorf(tb, "%s no longer has the entry %q with checked_by %s, and the code it describes is still here. "+
				"If a harness may no longer rely on it, bump `version` and remove this check in the same change; otherwise put the entry back.",
				File, id, target)
		}
	}
	for _, id := range got {
		if !slices.Contains(ids, id) {
			Errorf(tb, "%s assigns the entry %q to %s, and that test has no check for it: add one, or name the test that has.", File, id, target)
		}
	}
}

// Metric returns the metric entry with the given id, or fails the test.
func (c *Contract) Metric(tb testing.TB, id string) Metric {
	tb.Helper()
	for _, m := range c.Metrics {
		if m.ID == id {
			return m
		}
	}
	tb.Fatalf("%s has no metric %q", File, id)
	return Metric{}
}

// LogLine returns the log-line entry with the given id, or fails the test.
func (c *Contract) LogLine(tb testing.TB, id string) LogLine {
	tb.Helper()
	for _, l := range c.LogLines {
		if l.ID == id {
			return l
		}
	}
	tb.Fatalf("%s has no log line %q", File, id)
	return LogLine{}
}

// EnvoyStat returns the Envoy stat entry with the given id, or fails the test.
func (c *Contract) EnvoyStat(tb testing.TB, id string) EnvoyStat {
	tb.Helper()
	for _, s := range c.EnvoyStats {
		if s.ID == id {
			return s
		}
	}
	tb.Fatalf("%s has no Envoy stat %q", File, id)
	return EnvoyStat{}
}

// ResourceAttribute returns the resource-attribute entry with the given id, or
// fails the test.
func (c *Contract) ResourceAttribute(tb testing.TB, id string) ResourceAttribute {
	tb.Helper()
	for _, r := range c.ResourceAttributes {
		if r.ID == id {
			return r
		}
	}
	tb.Fatalf("%s has no resource attribute %q", File, id)
	return ResourceAttribute{}
}

// CheckNames compares the `names` entries assigned to target with the values
// the code has (id -> the Go constant), in both directions. others are the ids
// of the entries of other sections the same test holds with checks of its own:
// target's entries are exactly the names in code and those.
func (c *Contract) CheckNames(tb testing.TB, target string, code map[string]string, others ...string) {
	tb.Helper()
	c.Owns(tb, target, append(sortedKeys(code), others...)...)
	for _, n := range c.Names {
		if n.CheckedBy != target {
			continue
		}
		if got, ok := code[n.ID]; ok && got != n.Value {
			Errorf(tb, "%s says %s is %q, the code says %q.", File, n.ID, n.Value, got)
		}
	}
}

// Series is one data point of an instrument, as a test collected it: its
// attributes.
type Series map[string]string

// CheckMetric compares what an instrument really emits with its contract
// entry: the name it is registered with, the name a store holds for it, its
// type, and for every series its label names and, for a closed label, its
// values. series must hold every series the instrument can emit: the closed
// sets are compared in both directions, so a value the code emits and the
// contract lacks fails as surely as one the contract promises and the code
// never emits.
//
// registeredType is the type of the instrument found under the contract's
// otel_name, or "" when no instrument has that name.
func (m Metric) CheckMetric(tb testing.TB, registeredType string, series []Series) {
	tb.Helper()
	if registeredType == "" {
		Errorf(tb, "%s promises the metric %s (%s, stored as %s), and no instrument is registered under that name.", File, m.ID, m.OTelName, m.StoredName)
		return
	}
	if registeredType != m.Type {
		Errorf(tb, "%s says %s is a %s, the instrument is a %s.", File, m.OTelName, m.Type, registeredType)
	}
	if want := StoredName(m.OTelName, m.Type); want != m.StoredName {
		Errorf(tb, "%s says %s is stored as %s; the OTLP translation of that name and type is %s.", File, m.OTelName, m.StoredName, want)
	}
	if len(series) == 0 {
		Errorf(tb, "the test recorded nothing on %s, so its labels were not compared with %s.", m.OTelName, File)
		return
	}
	emitted := m.emitted(tb, series)
	for _, l := range m.Labels {
		got := emitted[l.Name]
		if len(got) == 0 {
			Errorf(tb, "%s lists the label %q for %s, and no series of %s carries it.", File, l.Name, m.ID, m.OTelName)
			continue
		}
		if l.Open {
			continue
		}
		if missing, extra := diff(l.Values, got); len(missing)+len(extra) > 0 {
			Errorf(tb, "%s label %q: %s lists [%s]; the code emits [%s] (in the contract only: [%s]; in the code only: [%s]). "+
				"A harness branches on this closed set, so a new value changes what the entry means as much as a removed one.",
				m.OTelName, l.Name, File, strings.Join(l.Values, ", "), strings.Join(sorted(got), ", "), strings.Join(missing, ", "), strings.Join(extra, ", "))
		}
	}
}

// emitted returns the values each label takes across series. It reports, for
// each series on its own, a label the contract does not list, a label the
// contract says the series carries and it does not, and a conditional label
// (`when`) on a series that should not have it: a harness selects ONE series
// by its labels, so a label that most series still carry is no help to it.
func (m Metric) emitted(tb testing.TB, series []Series) map[string][]string {
	tb.Helper()
	out := map[string][]string{}
	for _, s := range series {
		for _, k := range sortedKeys(s) {
			if _, ok := m.Label(k); !ok {
				Errorf(tb, "%s has a series with the label %q (%v), which %s does not list for %s.", m.OTelName, k, s, File, m.ID)
				continue
			}
			if !slices.Contains(out[k], s[k]) {
				out[k] = append(out[k], s[k])
			}
		}
		m.checkPresence(tb, s)
	}
	return out
}

// checkPresence reports a label one series should carry and does not, or
// carries and should not.
func (m Metric) checkPresence(tb testing.TB, s Series) {
	tb.Helper()
	for _, l := range m.Labels {
		_, has := s[l.Name]
		switch want := l.On(s); {
		case want && !has:
			Errorf(tb, "%s has a series without the label %q (%v), and %s says %s.", m.OTelName, l.Name, s, File, l.presence())
		case !want && has:
			Errorf(tb, "%s has a series with the label %q (%v), and %s says %s.", m.OTelName, l.Name, s, File, l.presence())
		}
	}
}

// presence says which series carry the label, for a failure message.
func (l Label) presence() string {
	if len(l.When) == 0 {
		return "every series carries it"
	}
	var conds []string
	for _, k := range sortedKeys(l.When) {
		conds = append(conds, k+"="+l.When[k])
	}
	return "only the series with " + strings.Join(conds, ", ") + " carry it"
}

// diff returns what is only in want and what is only in got, each sorted.
func diff(want, got []string) (onlyWant, onlyGot []string) {
	for _, v := range want {
		if !slices.Contains(got, v) {
			onlyWant = append(onlyWant, v)
		}
	}
	for _, v := range got {
		if !slices.Contains(want, v) {
			onlyGot = append(onlyGot, v)
		}
	}
	return sorted(onlyWant), sorted(onlyGot)
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// CheckFields compares the keys of one real log line (in the order the product
// wrote them) with the entry's field list.
func (l LogLine) CheckFields(tb testing.TB, keys []string) {
	tb.Helper()
	if slices.Equal(keys, l.Fields) {
		return
	}
	missing, extra := diff(l.Fields, keys)
	Errorf(tb, "%s lists the fields of %s as [%s]; the line the code writes has [%s] (in the contract only: [%s]; in the code only: [%s]; if both are empty the order differs). "+
		"A new field is added to the contract without a version bump; a removed or renamed one bumps it.",
		File, l.ID, strings.Join(l.Fields, ", "), strings.Join(keys, ", "), strings.Join(missing, ", "), strings.Join(extra, ", "))
}
