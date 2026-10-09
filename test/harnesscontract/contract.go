// Package harnesscontract loads external-harness.yaml: the checked-in list of
// what a harness outside this repository may read from a deployed mesh (metric
// names and labels, log-line fields, chart object names).
//
// It is test support. The tests that hold the code to the contract live with
// the code that owns each name (the `checked_by` of an entry names the Bazel
// test); they load the contract through this package, so a rename fails with a
// message that names the contract file. README.md says how each kind of entry
// is tied.
package harnesscontract

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// File is the contract's path in the repository. Failure messages name it.
const File = "test/harnesscontract/external-harness.yaml"

// ReviewOnly is the checked_by of an entry no test in this repository can
// hold to the code.
const ReviewOnly = "review-only"

// Rule is appended to every failure a contract check reports.
const Rule = "Change the code and " + File + " together, in one pull request, and say so in its description. " +
	"Removing an entry or changing what it means bumps the contract's `version`; adding one does not. " +
	"See test/harnesscontract/README.md."

//go:embed external-harness.yaml
var source []byte

// Contract is external-harness.yaml.
type Contract struct {
	Version            int                 `json:"version"`
	Metrics            []Metric            `json:"metrics"`
	ReasonClasses      []ReasonClasses     `json:"reason_classes"`
	ResourceAttributes []ResourceAttribute `json:"resource_attributes"`
	LogLines           []LogLine           `json:"log_lines"`
	EnvoyStats         []EnvoyStat         `json:"envoy_stats"`
	Names              []Name              `json:"names"`
	Charts             []Render            `json:"charts"`
	NotContract        []NotContract       `json:"not_contract"`
}

// Metric types, as the contract spells them.
const (
	TypeCounter = "counter"
	TypeGauge   = "gauge"
)

// Metric is one instrument a harness queries.
type Metric struct {
	ID         string  `json:"id"`
	Component  string  `json:"component"`
	OTelName   string  `json:"otel_name"`
	StoredName string  `json:"stored_name"`
	Type       string  `json:"type"`
	Labels     []Label `json:"labels"`
	Notes      string  `json:"notes"`
	CheckedBy  string  `json:"checked_by"`
}

// Label is one attribute of a Metric: a closed set of Values, or Open.
//
// Every series carries the label, unless When is set: then exactly the series
// whose other labels have the values When gives carry it, and no other does.
type Label struct {
	Name   string            `json:"name"`
	Values []string          `json:"values"`
	Open   bool              `json:"open"`
	When   map[string]string `json:"when"`
}

// On reports whether a series with these attributes carries the label.
func (l Label) On(s map[string]string) bool {
	for k, v := range l.When {
		if s[k] != v {
			return false
		}
	}
	return true
}

// Label returns the metric's label called name.
func (m Metric) Label(name string) (Label, bool) {
	for _, l := range m.Labels {
		if l.Name == name {
			return l, true
		}
	}
	return Label{}, false
}

// LabelNames returns the metric's label names, sorted.
func (m Metric) LabelNames() []string {
	out := make([]string, 0, len(m.Labels))
	for _, l := range m.Labels {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}

// ReasonClasses partitions a closed label set into the classes a harness
// grades differently.
type ReasonClasses struct {
	ID        string              `json:"id"`
	Metric    string              `json:"metric"`
	Label     string              `json:"label"`
	Classes   map[string][]string `json:"classes"`
	CheckedBy string              `json:"checked_by"`
}

// ResourceAttribute is an OpenTelemetry resource attribute a pipeline may turn
// into a label.
type ResourceAttribute struct {
	ID         string   `json:"id"`
	Attribute  string   `json:"attribute"`
	Value      string   `json:"value"`
	Components []string `json:"components"`
	Notes      string   `json:"notes"`
	CheckedBy  string   `json:"checked_by"`
}

// LogLine is a marker-prefixed, one-JSON-object log line.
type LogLine struct {
	ID            string   `json:"id"`
	Component     string   `json:"component"`
	Marker        string   `json:"marker"`
	Fields        []string `json:"fields"`
	TimeFields    []string `json:"time_fields"`
	Notes         string   `json:"notes"`
	CapPerWindow  int      `json:"cap_per_window"`
	WindowSeconds int      `json:"window_seconds"`
	CheckedBy     string   `json:"checked_by"`
}

// EnvoyStat is an Envoy stat whose prefix the agent chooses.
type EnvoyStat struct {
	ID                string `json:"id"`
	StatPrefix        string `json:"stat_prefix"`
	Stat              string `json:"stat"`
	StoredNamePattern string `json:"stored_name_pattern"`
	Notes             string `json:"notes"`
	CheckedBy         string `json:"checked_by"`
}

// Name is one string a harness writes into a manifest or a URL.
type Name struct {
	ID        string `json:"id"`
	Value     string `json:"value"`
	CheckedBy string `json:"checked_by"`
}

// NotContract names something a harness uses that the product does not emit.
type NotContract struct {
	Name string `json:"name"`
	What string `json:"what"`
}

// Load parses the embedded contract. An unknown key is an error: a misspelt
// field would otherwise be a check that silently never runs.
func Load() (*Contract, error) {
	return parse(source)
}

func parse(data []byte) (*Contract, error) {
	c := &Contract{}
	if err := yaml.UnmarshalStrict(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	if problems := c.Validate(); len(problems) > 0 {
		return nil, fmt.Errorf("%s is not well formed:\n  %s", File, strings.Join(problems, "\n  "))
	}
	return c, nil
}

// entry is what every contract entry has in common.
type entry struct{ section, id, checkedBy string }

func (c *Contract) entries() []entry {
	var out []entry
	for _, m := range c.Metrics {
		out = append(out, entry{"metrics", m.ID, m.CheckedBy})
	}
	for _, r := range c.ReasonClasses {
		out = append(out, entry{"reason_classes", r.ID, r.CheckedBy})
	}
	for _, r := range c.ResourceAttributes {
		out = append(out, entry{"resource_attributes", r.ID, r.CheckedBy})
	}
	for _, l := range c.LogLines {
		out = append(out, entry{"log_lines", l.ID, l.CheckedBy})
	}
	for _, s := range c.EnvoyStats {
		out = append(out, entry{"envoy_stats", s.ID, s.CheckedBy})
	}
	for _, n := range c.Names {
		out = append(out, entry{"names", n.ID, n.CheckedBy})
	}
	for _, r := range c.Charts {
		out = append(out, entry{"charts", r.ID, r.CheckedBy})
	}
	return out
}

// IDs returns the id of every entry whose checked_by is target, sorted.
func (c *Contract) IDs(target string) []string {
	var out []string
	for _, e := range c.entries() {
		if e.checkedBy == target {
			out = append(out, e.id)
		}
	}
	slices.Sort(out)
	return out
}

// Targets returns every checked_by in the contract except ReviewOnly, sorted
// and without repeats.
func (c *Contract) Targets() []string {
	var out []string
	for _, e := range c.entries() {
		if e.checkedBy != ReviewOnly {
			out = append(out, e.checkedBy)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate returns what is wrong with the contract as a document, whatever
// the code says: a missing version, an entry without an id or a checked_by, an
// id used twice, a label that is neither closed nor open, a reason partition
// that does not cover its label's closed set exactly once.
func (c *Contract) Validate() []string {
	var problems []string
	if c.Version < 1 {
		problems = append(problems, fmt.Sprintf("version is %d: it is a positive integer", c.Version))
	}
	// A chart object has an id too (a chart test lists it), and it is held by
	// the test of its render: ids are unique across all of them.
	entries := c.entries()
	for _, r := range c.Charts {
		for _, o := range r.Objects {
			entries = append(entries, entry{"the objects of " + r.ID, o.ID, r.CheckedBy})
		}
	}
	problems = append(problems, validateEntries(entries)...)
	byID := map[string]Metric{}
	for _, m := range c.Metrics {
		byID[m.ID] = m
		problems = append(problems, m.validate()...)
	}
	for _, r := range c.ReasonClasses {
		problems = append(problems, r.validate(byID)...)
	}
	for _, l := range c.LogLines {
		problems = append(problems, l.validate()...)
	}
	for _, n := range c.Names {
		if n.Value == "" {
			problems = append(problems, fmt.Sprintf("%s has no value", n.ID))
		}
	}
	for _, r := range c.Charts {
		problems = append(problems, r.validate()...)
	}
	return problems
}

// validateEntries checks what every entry has: an id of its own and a
// checked_by.
func validateEntries(entries []entry) []string {
	var problems []string
	seen := map[string]string{}
	for _, e := range entries {
		if e.id == "" {
			problems = append(problems, fmt.Sprintf("an entry of %s has no id", e.section))
			continue
		}
		if seen[e.id] != "" {
			problems = append(problems, fmt.Sprintf("id %q is used twice (%s and %s)", e.id, seen[e.id], e.section))
		}
		seen[e.id] = e.section
		switch {
		case e.checkedBy == "":
			problems = append(problems, fmt.Sprintf("%s has no checked_by: name the Bazel test that holds it to the code, or %q", e.id, ReviewOnly))
		case e.checkedBy != ReviewOnly && !strings.HasPrefix(e.checkedBy, "//"):
			problems = append(problems, fmt.Sprintf("%s: checked_by %q is neither a Bazel label nor %q", e.id, e.checkedBy, ReviewOnly))
		}
	}
	return problems
}

func (m Metric) validate() []string {
	var problems []string
	if m.OTelName == "" || m.StoredName == "" {
		problems = append(problems, fmt.Sprintf("%s needs both otel_name and stored_name", m.ID))
	}
	if m.Type != TypeCounter && m.Type != TypeGauge {
		problems = append(problems, fmt.Sprintf("%s: type %q is neither %q nor %q", m.ID, m.Type, TypeCounter, TypeGauge))
	}
	for _, l := range m.Labels {
		if l.Name == "" {
			problems = append(problems, fmt.Sprintf("%s has a label with no name", m.ID))
		}
		if l.Open == (len(l.Values) > 0) {
			problems = append(problems, fmt.Sprintf("%s: label %q must have either `values` (a closed set) or `open: true`", m.ID, l.Name))
		}
		if v := yamlBoolean(l.Values); v != "" {
			problems = append(problems, fmt.Sprintf("%s: label %q has the value %q: an unquoted y, n, yes, no, on or off is a boolean in YAML, so quote the value", m.ID, l.Name, v))
		}
		problems = append(problems, m.validateWhen(l)...)
	}
	return problems
}

// validateWhen checks that a conditional label's condition names values of the
// metric's other closed labels.
func (m Metric) validateWhen(l Label) []string {
	var problems []string
	for _, k := range sortedKeys(l.When) {
		if on, ok := m.Label(k); !ok || k == l.Name || !slices.Contains(on.Values, l.When[k]) {
			problems = append(problems, fmt.Sprintf("%s: label %q is `when` %s=%s, and that is not a value of another closed label of the metric", m.ID, l.Name, k, l.When[k]))
		}
	}
	return problems
}

func (l LogLine) validate() []string {
	var problems []string
	if l.Marker == "" || len(l.Fields) == 0 {
		problems = append(problems, fmt.Sprintf("%s needs a marker and its fields", l.ID))
	}
	if v := yamlBoolean(l.Fields); v != "" {
		problems = append(problems, fmt.Sprintf("%s has the field %q: an unquoted y, n, yes, no, on or off is a boolean in YAML, so quote the name", l.ID, v))
	}
	for _, f := range l.TimeFields {
		if !slices.Contains(l.Fields, f) {
			problems = append(problems, fmt.Sprintf("%s: time field %q is not one of its fields", l.ID, f))
		}
	}
	return problems
}

func (r ReasonClasses) validate(metrics map[string]Metric) []string {
	m, ok := metrics[r.Metric]
	if !ok {
		return []string{fmt.Sprintf("%s: there is no metric with id %q", r.ID, r.Metric)}
	}
	label, ok := m.Label(r.Label)
	if !ok || label.Open {
		return []string{fmt.Sprintf("%s: metric %s has no closed label %q", r.ID, r.Metric, r.Label)}
	}
	var problems []string
	classOf := map[string]string{}
	for _, class := range sortedKeys(r.Classes) {
		for _, v := range r.Classes[class] {
			if !slices.Contains(label.Values, v) {
				problems = append(problems, fmt.Sprintf("%s: class %s holds %q, which is not a value of %s's label %q", r.ID, class, v, r.Metric, r.Label))
			}
			if classOf[v] != "" {
				problems = append(problems, fmt.Sprintf("%s: %q is in two classes (%s and %s)", r.ID, v, classOf[v], class))
			}
			classOf[v] = class
		}
	}
	for _, v := range label.Values {
		if classOf[v] == "" {
			problems = append(problems, fmt.Sprintf("%s: %s's %s=%q is in no class: a harness cannot tell how to grade it", r.ID, r.Metric, r.Label, v))
		}
	}
	return problems
}

// yamlBoolean returns the first of values that reads "true" or "false", or "".
// No metric label value or log field in this contract is called that, and the
// YAML 1.1 parser turns an unquoted `y` or `n` (a real field name of the
// prober's line) into exactly those strings.
func yamlBoolean(values []string) string {
	for _, v := range values {
		if v == "true" || v == "false" {
			return v
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// StoredName is the name a Prometheus-compatible store holds for an
// OpenTelemetry instrument of the given type with no unit: every character
// outside [a-zA-Z0-9_:] becomes an underscore, and a monotonic counter gets the
// suffix _total unless it has it already.
func StoredName(otelName, metricType string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == ':':
			return r
		}
		return '_'
	}, otelName)
	if metricType == TypeCounter && !strings.HasSuffix(name, "_total") {
		name += "_total"
	}
	return name
}
