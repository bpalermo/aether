package harnesscontract

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/udspath"
)

// self is this test's Bazel label, as the contract's checked_by spells it.
const self = "//test/harnesscontract:harnesscontract_test"

// TestContractLoads: the checked-in file parses strictly and is well formed.
func TestContractLoads(t *testing.T) {
	c := MustLoad(t)
	if c.Version < 1 {
		t.Fatalf("version = %d", c.Version)
	}
}

// TestNames holds the `names` entries to the Go constants the product itself
// uses for them. An entry whose constant lives in a package this one cannot
// import (an `internal` one) is checked by a test there instead.
func TestNames(t *testing.T) {
	MustLoad(t).CheckNames(t, self, map[string]string{
		"port.outbound_http":                            strconv.Itoa(mesh.ProxyOutboundPort),
		"port.outbound_l4":                              strconv.Itoa(mesh.ProxyL4OutboundPort),
		"mesh.default_domain":                           mesh.DefaultMeshDomain,
		"pod.label.managed":                             labels.LabelAetherManaged,
		"pod.annotation.endpoint_port":                  annotations.AnnotationEndpointPort,
		"pod.annotation.endpoint_ports":                 annotations.AnnotationEndpointPorts,
		"pod.annotation.endpoint_protocol":              annotations.AnnotationEndpointProtocol,
		"pod.annotation.endpoint_uds_socket":            annotations.AnnotationEndpointUDSSocket,
		"pod.annotation.capture_exclude_outbound_ports": annotations.AnnotationCaptureExcludeOutboundPorts,
		"service.label.mesh_service":                    labels.LabelMeshService,
		"csi.driver":                                    udspath.CSIDriver,
	}, linkedResourceAttribute)
}

// linkedResourceAttribute is the one entry outside `names` this test holds:
// the resource attribute the charts pass to the agent and to the prober.
const linkedResourceAttribute = "resource.node"

// TestResourceAttributeIsTiedToTheCharts: what this test holds of resource.node
// is the link. The entry's attribute is compared with a render by the chart
// test of each component's chart, and only because that component's container
// refers to the entry; so every component has such a container (Validate also
// refuses a container of any other name), and it was resolved to this entry.
func TestResourceAttributeIsTiedToTheCharts(t *testing.T) {
	c := MustLoad(t)
	entry := c.ResourceAttribute(t, linkedResourceAttribute)
	if entry.Attribute == "" || len(entry.Components) == 0 {
		t.Fatalf("%s: %s has no attribute or no components", File, entry.ID)
	}
	tied := map[string]bool{}
	for _, r := range c.Charts {
		for _, o := range r.Objects {
			for _, ct := range o.Containers {
				if !slices.Contains(ct.ResourceAttributes, entry.ID) {
					continue
				}
				if !ct.linked || !slices.ContainsFunc(ct.attributes, func(a ResourceAttribute) bool { return a.ID == entry.ID && a.Attribute == entry.Attribute }) {
					t.Errorf("%s: container %q of %s refers to %s and was not resolved to it", File, ct.Name, o.ID, entry.ID)
				}
				tied[ct.Name] = true
			}
		}
	}
	for _, component := range entry.Components {
		if !tied[component] {
			Errorf(t, "%s says the %s carries %s (%s), and no container %q under `charts` lists %s in its `resource_attributes`: no chart test compares it with a render.",
				File, component, entry.Attribute, entry.ID, component, entry.ID)
		}
	}
}

// TestMeshDomainIsTiedToTheChart: the default mesh domain is compared with the
// Go default by TestNames, and with what a default install really passes only
// because a container of the aether chart's default render takes --mesh-domain
// from the entry.
func TestMeshDomainIsTiedToTheChart(t *testing.T) {
	const id, flag = "mesh.default_domain", "--mesh-domain"
	var tied []string
	for _, r := range MustLoad(t).RendersOf("aether") {
		if _, set := r.Set["meshDomain"]; set {
			continue
		}
		for _, o := range r.Objects {
			for _, ct := range o.Containers {
				if ct.Args[flag] == id && ct.args[flag] == mesh.DefaultMeshDomain {
					tied = append(tied, ct.Name)
				}
			}
		}
	}
	if !slices.Contains(tied, "agent") {
		Errorf(t, "%s: no render of the aether chart that leaves `meshDomain` at its default holds the agent container's %s to %s (containers that do: %v), so the chart's default could change and no test would fail.",
			File, flag, id, tied)
	}
}

// TestEveryCheckedByIsInTheSuite: a checked_by that names a test nobody runs
// under that name is a promise nothing checks. The package's `checks`
// test_suite is the list of the tests entries may name; this compares the two
// in both directions by reading the BUILD file.
func TestEveryCheckedByIsInTheSuite(t *testing.T) {
	build, err := os.ReadFile("BUILD.bazel")
	if err != nil {
		t.Fatalf("read BUILD.bazel (it is this test's data): %v", err)
	}
	suite := regexp.MustCompile(`(?s)test_suite\(\s*name = "checks",\s*tests = \[(.*?)\]`).FindSubmatch(build)
	if suite == nil {
		t.Fatal("BUILD.bazel has no test_suite named checks")
	}
	var inSuite []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(suite[1], -1) {
		label := string(m[1])
		if strings.HasPrefix(label, ":") {
			label = "//test/harnesscontract" + label
		}
		inSuite = append(inSuite, label)
	}
	slices.Sort(inSuite)
	targets := MustLoad(t).Targets()
	if missing, extra := diff(targets, inSuite); len(missing)+len(extra) > 0 {
		t.Errorf("%s names these tests in checked_by: %v\n//test/harnesscontract:checks runs these: %v\nonly in the contract: %v; only in the suite: %v",
			File, targets, inSuite, missing, extra)
	}
}

const minimal = `
version: 1
metrics:
  - id: m
    component: agent
    otel_name: a.b
    stored_name: a_b_total
    type: counter
    labels:
      - {name: reason, values: [x, w]}
    checked_by: //a:b
`

// TestParseRejects: what makes a contract unusable as a document is refused
// when it is loaded, so no check runs against a file that means something
// other than it says.
func TestParseRejects(t *testing.T) {
	if _, err := parse([]byte(minimal)); err != nil {
		t.Fatalf("the minimal contract does not load: %v", err)
	}
	for name, tc := range map[string]struct{ from, to, want string }{
		"an unknown key":             {"component: agent", "componnent: agent", "unknown field"},
		"no version":                 {"version: 1", "version: 0", "positive integer"},
		"no checked_by":              {"checked_by: //a:b", "notes: x", "no checked_by"},
		"a checked_by that is prose": {"checked_by: //a:b", "checked_by: somebody", "neither a Bazel label"},
		"an unknown type":            {"type: counter", "type: histogram", "neither"},
		"a label with no set":        {"{name: reason, values: [x, w]}", "{name: reason}", "either `values`"},
		"a label both closed and open": {
			"{name: reason, values: [x, w]}", "{name: reason, values: [x], open: true}", "either `values`",
		},
		"an unquoted y among the values": {"{name: reason, values: [x, w]}", "{name: reason, values: [x, y]}", "is a boolean in YAML"},
		"an unquoted n among the fields": {
			"checked_by: //a:b", "checked_by: //a:b\nlog_lines:\n  - {id: l, marker: M, fields: [t, n], checked_by: review-only}", "is a boolean in YAML",
		},
		"a label conditional on nothing the metric has": {
			"{name: reason, values: [x, w]}", "{name: reason, values: [x, w], when: {pin: unpinned}}", "is not a value of another closed label",
		},
		"an id used twice": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: m, value: v, checked_by: review-only}", "used twice",
		},
		"a reason in no class": {
			"checked_by: //a:b", "checked_by: //a:b\nreason_classes:\n  - {id: r, metric: m, label: reason, classes: {one: [x]}, checked_by: review-only}", "is in no class",
		},
		"a reason in two classes": {
			"checked_by: //a:b", "checked_by: //a:b\nreason_classes:\n  - {id: r, metric: m, label: reason, classes: {one: [x, w], two: [w]}, checked_by: review-only}", "in two classes",
		},
		"a class of an unknown value": {
			"checked_by: //a:b", "checked_by: //a:b\nreason_classes:\n  - {id: r, metric: m, label: reason, classes: {one: [x, w, z]}, checked_by: review-only}", "not a value of",
		},
		"a class of an unknown metric": {
			"checked_by: //a:b", "checked_by: //a:b\nreason_classes:\n  - {id: r, metric: nope, label: reason, classes: {one: [x, w]}, checked_by: review-only}", "no metric with id",
		},
		"a time field that is not a field": {
			"checked_by: //a:b", "checked_by: //a:b\nlog_lines:\n  - {id: l, marker: M, fields: [t], time_fields: [u], checked_by: review-only}", "is not one of its fields",
		},
		"a name with no value": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: n, checked_by: review-only}", "has no value",
		},
		"an aether render that would generate a key": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: aether, release: r, namespace: n, objects: [{id: o, kind: K, name: x}], checked_by: review-only}", "generated private key",
		},
		"two chart objects with one id": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x}, {id: o, kind: K, name: z}], checked_by: review-only}", `id "o" is used twice`,
		},
		"a chart object with the id of another entry": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: m, kind: K, name: x}], checked_by: review-only}", `id "m" is used twice`,
		},
		"a container tied to a resource attribute the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, resource_attributes: [nope]}]}], checked_by: review-only}", "`resource_attributes` has no entry with that id",
		},
		"a resource attribute of a component whose container is not tied to it": {
			"checked_by: //a:b", "checked_by: //a:b\nresource_attributes:\n  - {id: ra, attribute: k, components: [agent, prober], checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, resource_attributes: [ra]}]}], checked_by: review-only}",
			"ra lists the components [agent, prober], and the chart containers that refer to it are [agent]",
		},
		"a resource attribute tied to a container that is not a component": {
			"checked_by: //a:b", "checked_by: //a:b\nresource_attributes:\n  - {id: ra, attribute: k, components: [agent], checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, resource_attributes: [ra]}, {name: proxy, resource_attributes: [ra]}]}], checked_by: review-only}",
			"the chart containers that refer to it are [agent, proxy]",
		},
		"an argument taken from a name the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, args: {--mesh-domain: nope}}]}], checked_by: review-only}", "`names` has no entry with that id",
		},
		"an argument key that is not a flag": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: nm, value: v, checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, args: {mesh-domain: n}}]}], checked_by: review-only}", "is not a flag",
		},
		"an unquoted n as an id": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: n, value: v, checked_by: review-only}", `an entry of names has the id "false"`,
		},
		"an id of two words": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: a fields, value: v, checked_by: review-only}", `an entry of names has the id "a fields": an id is one word`,
		},
		"an id with a quote": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: 'a\"b', value: v, checked_by: review-only}", `an id is one word`,
		},
		"a field listed twice": {
			"checked_by: //a:b", "checked_by: //a:b\nlog_lines:\n  - {id: l, marker: M, fields: [t, tier, t], checked_by: review-only}", "it says these more than once (a field, a component, a container or a host path listed twice): l fields t",
		},
		"a container listed twice": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent}, {name: agent}]}], checked_by: review-only}", "more than once (a field, a component, a container or a host path listed twice): o containers agent",
		},
		"an empty checked_by list":    {"checked_by: //a:b", "checked_by: []", "no checked_by"},
		"a test named twice":          {"checked_by: //a:b", "checked_by: [//a:b, //a:b]", "checked_by names //a:b twice"},
		"review-only beside a test":   {"checked_by: //a:b", "checked_by: [//a:b, review-only]", "and a test at once"},
		"prose in a checked_by list":  {"checked_by: //a:b", "checked_by: [//a:b, somebody]", `checked_by "somebody" is neither a Bazel label`},
		"a checked_by that is a map":  {"checked_by: //a:b", "checked_by: {a: b}", "checked_by is a Bazel label, a list of them"},
		"a checked_by list of a list": {"checked_by: //a:b", "checked_by: [[//a:b]]", "checked_by is a Bazel label, a list of them"},
		"an object with a name and a name_from": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: nm, value: v, checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, name_from: nm}], checked_by: review-only}", "has a name or a name_from but not both",
		},
		"a host path made of a name the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, host_paths: [\"/plugins/<nope>\"]}], checked_by: review-only}", "o refers to \"nope\" under `host_paths` or `webhooks`, and `names` has no entry with that id",
		},
		"a host path that is no pattern": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, host_paths: [/plugins/x]}], checked_by: review-only}", "o refers to \"/plugins/x\" under `host_paths` or `webhooks`",
		},
		"a webhook held to nothing": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, webhooks: {hook: {}}}], checked_by: review-only}", "o: the webhook \"hook\" is held to nothing",
		},
		"a webhook held to a name the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, webhooks: {hook: {objects: nope}}}], checked_by: review-only}", "o refers to \"nope\" under `host_paths` or `webhooks`",
		},
		"an object named by an entry the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name_from: nope}], checked_by: review-only}", "o takes its name from \"nope\", and `names` has no entry with that id",
		},
		"a container held to a code attribute the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, code_resource_attributes: [nope]}]}], checked_by: review-only}", "`resource_attributes` has no entry with that id",
		},
		"an attribute the chart both gives and must not give": {
			"checked_by: //a:b", "checked_by: //a:b\nresource_attributes:\n  - {id: ra, attribute: k, components: [agent], checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, resource_attributes: [ra], code_resource_attributes: [ra]}]}], checked_by: review-only}",
			"it is one or the other",
		},
		"a code attribute of a component whose container is not held to it": {
			"checked_by: //a:b", "checked_by: //a:b\nresource_attributes:\n  - {id: ra, attribute: k, components: [agent, prober], checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, code_resource_attributes: [ra]}]}], checked_by: review-only}",
			"ra lists the components [agent, prober], and the chart containers that refer to it are [agent]",
		},
		"an argument pattern with a name the contract lacks": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: nm, value: v, checked_by: review-only}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x, containers: [{name: agent, args: {--egress: \"<nm>:<nope>\"}}]}], checked_by: review-only}",
			"takes --egress from \"nope\", and `names` has no entry with that id",
		},
		"an entry a chart test compares with a render and does not name": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: nm, value: v, checked_by: //a:b}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name_from: nm}], checked_by: //t:chart}",
			"nm is compared with a render by //t:chart (a render that test holds refers to it), and its checked_by does not name that test",
		},
		"an entry that names a chart test with nothing to compare it with": {
			"checked_by: //a:b", "checked_by: //a:b\nnames:\n  - {id: nm, value: v, checked_by: [//a:b, //t:chart]}\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, objects: [{id: o, kind: K, name: x}], checked_by: //t:chart}",
			"nm: checked_by names the chart test //t:chart, and no render that test holds refers to the entry",
		},
		"a render of nothing": {
			"checked_by: //a:b", "checked_by: //a:b\ncharts:\n  - {id: c, chart: x, release: r, namespace: n, checked_by: review-only}", "lists no object",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(minimal, tc.from) {
				t.Fatalf("the minimal contract has no %q to replace", tc.from)
			}
			_, err := parse([]byte(strings.Replace(minimal, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parse error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestCheckedByList: an entry several tests hold names them all, and is among
// the entries of each: every one of them has to list it.
func TestCheckedByList(t *testing.T) {
	one, err := parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := one.Metrics[0].CheckedBy; !slices.Equal(got, CheckedBy{"//a:b"}) {
		t.Errorf("a checked_by written as one label is %q", got)
	}
	c, err := parse([]byte(strings.Replace(minimal, "checked_by: //a:b", "checked_by: [//a:b, //c:d]\nnames:\n  - {id: nm, value: v, checked_by: //c:d}\n  - {id: r, value: v, checked_by: review-only}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string][]string{"//a:b": {"m"}, "//c:d": {"m", "nm"}, "//e:f": nil, ReviewOnly: {"r"}} {
		if got := c.IDs(target); !slices.Equal(got, want) {
			t.Errorf("IDs(%s) = %q, want %q", target, got, want)
		}
	}
	if got, want := c.Targets(), []string{"//a:b", "//c:d"}; !slices.Equal(got, want) {
		t.Errorf("Targets() = %q, want %q", got, want)
	}
	// Each of the two tests holds m: one that does not list it has an entry
	// assigned to it and no check.
	for _, target := range []string{"//a:b", "//c:d"} {
		r := &recorder{TB: t}
		c.Owns(r, target, "nm")
		r.want(t, `assigns the entry "m" to `+target)
	}
	r := &recorder{TB: t}
	c.Owns(r, "//a:b", "m")
	c.Owns(r, "//c:d", "m", "nm")
	r.want(t)
	// And a name two tests hold is compared with the code by each.
	r = &recorder{TB: t}
	shared := &Contract{Version: 1, Names: []Name{{ID: "n", Value: "old", CheckedBy: CheckedBy{"//a:b", "//c:d"}}}}
	shared.CheckNames(r, "//c:d", map[string]string{"n": "new"})
	r.want(t, `says n is "old", the code says "new"`)
}

func TestStoredName(t *testing.T) {
	for _, tc := range []struct{ otel, kind, want string }{
		{"aether.agent.identity.cluster_unpinned", TypeCounter, "aether_agent_identity_cluster_unpinned_total"},
		{"aether_probe_requests_total", TypeCounter, "aether_probe_requests_total"},
		{"aether.agent.snapshot.tls_clusters", TypeGauge, "aether_agent_snapshot_tls_clusters"},
		{"a-b/c", TypeGauge, "a_b_c"},
	} {
		if got := StoredName(tc.otel, tc.kind); got != tc.want {
			t.Errorf("StoredName(%q, %s) = %q, want %q", tc.otel, tc.kind, got, tc.want)
		}
	}
}

// recorder is a testing.TB that keeps what a check reports instead of failing,
// so the checks themselves can be shown to fail.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) want(t *testing.T, substrings ...string) {
	t.Helper()
	all := strings.Join(r.errors, "\n")
	for _, s := range substrings {
		if !strings.Contains(all, s) {
			t.Errorf("the check reported %q, want it to contain %q", all, s)
		}
	}
	if len(substrings) == 0 && len(r.errors) > 0 {
		t.Errorf("the check reported %q, want nothing", all)
	}
	// Every failure tells the reader which file to change, and the rule.
	for _, e := range r.errors {
		if !strings.Contains(e, File) || !strings.Contains(e, "bumps the contract's `version`") {
			t.Errorf("a failure does not name the contract file and the rule: %q", e)
		}
	}
}

// TestChecksCanFail: each helper the owning tests call reports the difference
// it exists for, with the contract file and the rule in the message.
func TestChecksCanFail(t *testing.T) {
	c, err := parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Metrics[0]
	ok := []Series{{"reason": "x"}, {"reason": "w"}}

	t.Run("a metric as the contract says", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, ok)
		r.want(t)
	})
	t.Run("a renamed metric", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, "", nil)
		r.want(t, "no instrument is registered under that name")
	})
	t.Run("another type", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeGauge, ok)
		r.want(t, "the instrument is a gauge")
	})
	t.Run("a stored name that is not the translation", func(t *testing.T) {
		r := &recorder{TB: t}
		wrong := m
		wrong.StoredName = "a_b"
		wrong.CheckMetric(r, TypeCounter, ok)
		r.want(t, "the OTLP translation of that name and type is a_b_total")
	})
	t.Run("a value the contract lacks", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, append(slices.Clone(ok), Series{"reason": "z"}))
		r.want(t, "in the code only: [z]")
	})
	t.Run("a value the code no longer emits", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, ok[:1])
		r.want(t, "in the contract only: [w]")
	})
	t.Run("a label the contract lacks", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, []Series{{"reason": "x", "node": "n"}, {"reason": "w"}})
		r.want(t, `the label "node"`)
	})
	t.Run("one series without a label the others carry", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, []Series{{"reason": "x"}, {"reason": "w"}, {}})
		r.want(t, `a series without the label "reason"`, "every series carries it")
	})
	t.Run("a conditional label", func(t *testing.T) {
		gauge := Metric{ID: "g", OTelName: "g", StoredName: "g", Type: TypeGauge, Labels: []Label{
			{Name: "pin", Values: []string{"pinned", "unpinned"}},
			{Name: "reason", Values: []string{"x"}, When: map[string]string{"pin": "unpinned"}},
		}}
		r := &recorder{TB: t}
		gauge.CheckMetric(r, TypeGauge, []Series{{"pin": "pinned"}, {"pin": "unpinned", "reason": "x"}})
		r.want(t)
		gauge.CheckMetric(r, TypeGauge, []Series{{"pin": "pinned", "reason": "x"}, {"pin": "unpinned", "reason": "x"}})
		r.want(t, `a series with the label "reason"`, "only the series with pin=unpinned carry it")
		r = &recorder{TB: t}
		gauge.CheckMetric(r, TypeGauge, []Series{{"pin": "pinned"}, {"pin": "unpinned"}, {"pin": "unpinned", "reason": "x"}})
		r.want(t, `a series without the label "reason"`)
		// The pinned series losing `pin` while the unpinned ones keep it.
		r = &recorder{TB: t}
		gauge.CheckMetric(r, TypeGauge, []Series{{}, {"pin": "unpinned", "reason": "x"}})
		r.want(t, `a series without the label "pin"`)
	})
	t.Run("a label no series carries", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, []Series{{}})
		r.want(t, "no series of a.b carries it")
	})
	t.Run("nothing recorded", func(t *testing.T) {
		r := &recorder{TB: t}
		m.CheckMetric(r, TypeCounter, nil)
		r.want(t, "recorded nothing")
	})
	t.Run("an entry removed from the contract", func(t *testing.T) {
		r := &recorder{TB: t}
		c.Owns(r, "//a:b", "m", "gone")
		r.want(t, `no longer has the entry "gone"`)
	})
	t.Run("an entry the test does not know", func(t *testing.T) {
		r := &recorder{TB: t}
		c.Owns(r, "//a:b")
		r.want(t, `assigns the entry "m" to //a:b`)
	})
	t.Run("a name with another value", func(t *testing.T) {
		r := &recorder{TB: t}
		named := &Contract{Version: 1, Names: []Name{{ID: "n", Value: "old", CheckedBy: CheckedBy{"//a:b"}}}}
		named.CheckNames(r, "//a:b", map[string]string{"n": "new"})
		r.want(t, `says n is "old", the code says "new"`)
	})
	t.Run("log fields", func(t *testing.T) {
		l := LogLine{ID: "l", Fields: []string{"t", "tier"}}
		r := &recorder{TB: t}
		l.CheckFields(r, []string{"t", "tier"})
		r.want(t)
		l.CheckFields(r, []string{"t", "level"})
		r.want(t, "in the contract only: [tier]", "in the code only: [level]")
	})
}
