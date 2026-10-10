package harnesscontract

import (
	_ "embed"
	"reflect"
	"slices"
	"strings"
	"testing"
)

//go:embed external-harness.lock.yaml
var lockSource []byte

// TestVersionBump: the contract makes every promise the lock holds for its
// version, unchanged, and the lock holds every promise the contract makes. A
// promise that left or changed is a version bump (README.md, "The rule"); the
// failure says which entry and which key.
func TestVersionBump(t *testing.T) {
	for _, problem := range MustLoad(t).CheckLockFile(lockSource) {
		Errorf(t, "%s", problem)
	}
}

// TestCheckLockFile: the lock is held to the form the test prints, one promise
// to a line. scripts/check-harness-contract-bump.sh reads the lock at a pull
// request's base line by line; a lock that says the same in another YAML
// shape would read there as holding no promise, and promises could then leave
// with their entries and no version bump.
func TestCheckLockFile(t *testing.T) {
	c := mustParse(t, full)
	if got := c.CheckLockFile(c.Lock()); len(got) != 0 {
		t.Fatalf("CheckLockFile() of the lock the contract writes = %q", got)
	}
	lock := lockOf(t, c)
	var inline, indented []string
	for _, name := range sortedKeys(lock.Promises) {
		inline = append(inline, strings.TrimSpace(lockLine(name, lock.Promises[name])))
		indented = append(indented, "  "+lockLine(name, lock.Promises[name]))
	}
	for name, text := range map[string]string{
		"an inline map":              "version: 3\npromises: {" + strings.Join(inline, ", ") + "}\n",
		"another indentation":        "version: 3\npromises:\n" + strings.Join(indented, "\n") + "\n",
		"the lines in another order": strings.Replace(string(c.Lock()), lockLine("c chart", lock.Promises["c chart"])+"\n", "", 1) + lockLine("c chart", lock.Promises["c chart"]) + "\n",
		"no header":                  string(lock.render()),
	} {
		t.Run(name, func(t *testing.T) {
			// The same promises, so the comparison with the contract passes...
			parsed, err := ParseLock([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.CheckLock(parsed); len(got) != 0 {
				t.Fatalf("CheckLock() = %q: the case is meant to hold the same promises", got)
			}
			// ...and the file is refused for its form.
			got := strings.Join(c.CheckLockFile([]byte(text)), "\n")
			if !strings.Contains(got, "is not written the way this test prints it") || !strings.Contains(got, LockFile) {
				t.Errorf("CheckLockFile() = %q", got)
			}
		})
	}
	// A lock that is wrong is reported for what is wrong, not for its form.
	stale := strings.Join(mustParse(t, edit(t, full, "{id: dom, value: v,", "{id: dom, value: v9,")).CheckLockFile(c.Lock()), "\n")
	if !strings.Contains(stale, bumpNeeded) || strings.Contains(stale, "is not written the way") {
		t.Errorf("CheckLockFile() = %q", stale)
	}
	if got := c.CheckLockFile([]byte("version: [")); len(got) != 1 || !strings.Contains(got[0], LockFile) {
		t.Errorf("CheckLockFile() of a file that is not YAML = %q", got)
	}
}

// full is a contract with every key of every kind of entry set.
const full = `
version: 3
metrics:
  - id: m
    component: agent
    otel_name: a.b
    stored_name: a_b_total
    type: counter
    labels:
      - {name: pin, values: [pinned, unpinned]}
      - {name: reason, values: [x, w], when: {pin: unpinned}}
      - {name: pod, open: true}
    notes: prose
    checked_by: //a:b
reason_classes:
  - {id: rc, metric: m, label: reason, classes: {one: [x, w]}, checked_by: review-only}
resource_attributes:
  - {id: ra, attribute: k8s.node.name, components: [agent], notes: prose, checked_by: review-only}
  - {id: sn, attribute: service.name, value: svc, components: [prober], checked_by: review-only}
log_lines:
  - id: l
    component: prober
    marker: "M "
    fields: [t, tier, err]
    time_fields: [t]
    notes: prose
    cap_per_window: 20
    window_seconds: 60
    checked_by: //a:b
envoy_stats:
  - {id: es, stat_prefix: p_, stat: tcp.p_<c>.x, stored_name_pattern: envoy_.*, notes: prose, checked_by: //a:b}
names:
  - {id: dom, value: v, checked_by: //a:b}
  - {id: port, value: "18081", checked_by: //a:b}
  - {id: ann, value: example.io/contract-version, checked_by: //a:b}
charts:
  - id: c
    chart: x
    release: r
    namespace: ns
    set: {a.b: "true"}
    objects:
      - id: c.o
        kind: DaemonSet
        name: workload
        namespace: ns
        pod_labels: {app: agent}
        containers:
          - name: agent
            env_contains: {VAR: ["k="]}
            resource_attributes: [ra]
            args: {--domain: dom, --egress: "127.0.0.1:<port>"}
          - name: prober
            code_resource_attributes: [sn]
        rolling_update: {maxSurge: "0", maxUnavailable: "1"}
        host_paths: ["/plugins/<dom>"]
        contract_version_annotation: ann
      - id: c.d
        kind: CSIDriver
        name_from: dom
      - id: c.w
        kind: MutatingWebhookConfiguration
        webhooks: {hook.x: {objects: dom}}
    checked_by: review-only
`

// edit replaces from, which the text must hold, once.
func edit(t *testing.T, text, from, to string) string {
	t.Helper()
	if !strings.Contains(text, from) {
		t.Fatalf("the contract has no %q to replace", from)
	}
	return strings.Replace(text, from, to, 1)
}

func mustParse(t *testing.T, text string) *Contract {
	t.Helper()
	c, err := parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func lockOf(t *testing.T, c *Contract) *Lock {
	t.Helper()
	lock, err := ParseLock(c.Lock())
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

const (
	bumpNeeded   = "bump `version` to 4"
	noBumpNeeded = "A new promise needs no version bump"
)

// TestCheckLock_SameVersion: against the lock of the unchanged contract, at
// the same version, what README.md calls compatible passes or asks for a line
// in the lock, and everything else asks for a version bump.
func TestCheckLock_SameVersion(t *testing.T) {
	type change struct{ from, to string }
	for name, tc := range map[string]struct {
		changes []change
		// want is what the failure holds. None: the change is no promise at all.
		want []string
	}{
		"nothing": {},
		// Not promises.
		"a description":                  {changes: []change{{"notes: prose", "notes: other prose"}}},
		"one more test holds the entry":  {changes: []change{{"checked_by: //a:b", "checked_by: [//a:b, //c:d]"}}},
		"the order of a closed set":      {changes: []change{{"values: [x, w]", "values: [w, x]"}}},
		"the order of a line's fields":   {changes: []change{{"fields: [t, tier, err]", "fields: [err, t, tier]"}}},
		"the order within a class":       {changes: []change{{"one: [x, w]", "one: [w, x]"}}},
		"the order of a metric's labels": {changes: []change{{"      - {name: pod, open: true}\n", ""}, {"    labels:\n", "    labels:\n      - {name: pod, open: true}\n"}}},
		// A tie moved to another entry that says the same: what is deployed and
		// what a harness reads are as they were, so the only news is the entry.
		// TestTies, not the lock, holds which entry a tie names.
		"a host path made of another entry with the same value": {
			changes: []change{{"names:\n", "names:\n  - {id: dom2, value: v, checked_by: //a:b}\n"}, {`host_paths: ["/plugins/<dom>"]`, `host_paths: ["/plugins/<dom2>"]`}},
			want:    []string{noBumpNeeded, `"dom2 value": "`},
		},
		"a name taken from another entry with the same value": {
			changes: []change{{"names:\n", "names:\n  - {id: dom2, value: v, checked_by: //a:b}\n"}, {"name_from: dom", "name_from: dom2"}},
			want:    []string{noBumpNeeded, `"dom2 value": "`},
		},
		"an argument taken from another entry with the same value": {
			changes: []change{{"names:\n", "names:\n  - {id: dom2, value: v, checked_by: //a:b}\n"}, {"--domain: dom,", "--domain: dom2,"}},
			want:    []string{noBumpNeeded, `"dom2 value": "`},
		},
		"a webhook held to another entry with the same value": {
			changes: []change{{"names:\n", "names:\n  - {id: dom2, value: v, checked_by: //a:b}\n"}, {"webhooks: {hook.x: {objects: dom}}", "webhooks: {hook.x: {objects: dom2}}"}},
			want:    []string{noBumpNeeded, `"dom2 value": "`},
		},
		"a resource attribute given through another entry of the same attribute": {
			changes: []change{{"resource_attributes:\n", "resource_attributes:\n  - {id: ra2, attribute: k8s.node.name, components: [agent], checked_by: review-only}\n"}, {"            resource_attributes: [ra]\n", "            resource_attributes: [ra2]\n"}},
			want:    []string{noBumpNeeded, `"ra2 attribute": "`},
		},
		"a code attribute withheld through another entry of the same attribute": {
			changes: []change{{"resource_attributes:\n", "resource_attributes:\n  - {id: sn2, attribute: service.name, value: svc, components: [prober], checked_by: review-only}\n"}, {"code_resource_attributes: [sn]", "code_resource_attributes: [sn2]"}},
			want:    []string{noBumpNeeded, `"sn2 attribute": "`},
		},
		// And the other way: the same entry, another attribute, is another thing
		// deployed.
		"a container given another attribute through the same entry": {
			changes: []change{{"{id: ra, attribute: k8s.node.name,", "{id: ra, attribute: k8s.node,"}},
			want:    []string{`containers agent resource_attributes k8s.node.name is gone`, bumpNeeded},
		},
		// The same promise to a harness, so no bump; that the tie is gone is
		// TestTies's to say, on the checked-in contract.
		"a name written out instead of taken from the entry that has it": {changes: []change{{"name_from: dom", "name: v"}}},

		// New promises: a line in the lock, no bump.
		"a new entry":           {changes: []change{{"names:\n", "names:\n  - {id: n2, value: v2, checked_by: //a:b}\n"}}, want: []string{noBumpNeeded, `"n2 value": "`}},
		"a new field of a line": {changes: []change{{"fields: [t, tier, err]", "fields: [t, tier, err, extra]"}}, want: []string{noBumpNeeded, `"l fields extra": "`}},
		"a new time field":      {changes: []change{{"fields: [t, tier, err]", "fields: [t, tier, err, at]"}, {"time_fields: [t]", "time_fields: [t, at]"}}, want: []string{noBumpNeeded, `"l fields at": "`}},
		"a key an entry did not have": {
			changes: []change{{"attribute: k8s.node.name,", "attribute: k8s.node.name, value: w,"}},
			want:    []string{noBumpNeeded, `"ra value": "`},
		},
		"a new pod label": {changes: []change{{"pod_labels: {app: agent}", "pod_labels: {app: agent, tier: mesh}"}}, want: []string{noBumpNeeded, `"c.o pod_labels tier": "`}},
		"a new container": {
			changes: []change{{"          - name: prober\n", "          - name: sidecar\n          - name: prober\n"}},
			want:    []string{noBumpNeeded, `"c.o containers sidecar": "`},
		},
		"a new object": {
			changes: []change{{"      - id: c.d\n", "      - {id: c.e, kind: Deployment, name: edge}\n      - id: c.d\n"}},
			want:    []string{noBumpNeeded, `"c.e kind": "`, `"c.e name": "`, `"c.e render": "`},
		},
		"something more a container is held to": {
			changes: []change{{`env_contains: {VAR: ["k="]}`, `env_contains: {VAR: ["k=", "j="]}`}},
			want:    []string{noBumpNeeded, `"c.o containers agent env_contains VAR j=": "`},
		},

		// Promises broken: a bump.
		"an entry removed": {
			changes: []change{{"  - {id: es, stat_prefix: p_, stat: tcp.p_<c>.x, stored_name_pattern: envoy_.*, notes: prose, checked_by: //a:b}\n", ""}},
			want:    []string{`no longer has the entry "es", which it promised at version 3`, bumpNeeded},
		},
		"an entry renamed": {changes: []change{{"id: es,", "id: es2,"}}, want: []string{`no longer has the entry "es"`, bumpNeeded}},
		"a value changed": {
			changes: []change{{"{id: dom, value: v,", "{id: dom, value: v9,"}},
			want: []string{
				`the entry "dom" no longer promises what it did at version 3: value changed`,
				// And what the charts are held to through it.
				`the entry "c.d" no longer promises what it did at version 3: name changed`,
				`the entry "c.o" no longer promises what it did at version 3: containers agent args --domain changed; host_paths /plugins/v is gone`,
				`the entry "c.w" no longer promises what it did at version 3: webhooks hook.x changed`,
				bumpNeeded,
			},
		},
		"a value added to a closed set": {
			changes: []change{{"values: [x, w]", "values: [x, w, z]"}, {"one: [x, w]", "one: [x, w, z]"}},
			want:    []string{`the entry "m" no longer promises what it did at version 3: labels changed`, `the entry "rc" no longer promises what it did at version 3: classes changed`, bumpNeeded},
		},
		"a value removed from a closed set": {
			changes: []change{{"values: [x, w]", "values: [x]"}, {"one: [x, w]", "one: [x]"}},
			want:    []string{`"m" no longer promises what it did at version 3: labels changed`, bumpNeeded},
		},
		"a label added":   {changes: []change{{"    labels:\n", "    labels:\n      - {name: node, open: true}\n"}}, want: []string{`"m" no longer promises what it did at version 3: labels changed`, bumpNeeded}},
		"a label removed": {changes: []change{{"      - {name: pod, open: true}\n", ""}}, want: []string{`"m" no longer promises what it did at version 3: labels changed`, bumpNeeded}},
		"a closed label opened": {
			changes: []change{{"{name: pin, values: [pinned, unpinned]}", "{name: pin, open: true}"}, {", when: {pin: unpinned}", ""}},
			want:    []string{`"m" no longer promises what it did at version 3: labels changed`, bumpNeeded},
		},
		"a label no longer conditional": {changes: []change{{", when: {pin: unpinned}", ""}}, want: []string{`labels changed`, bumpNeeded}},
		"a metric renamed": {
			changes: []change{{"otel_name: a.b", "otel_name: a.c"}, {"stored_name: a_b_total", "stored_name: a_c_total"}},
			want:    []string{`"m" no longer promises what it did at version 3: otel_name changed; stored_name changed`, bumpNeeded},
		},
		"another type":               {changes: []change{{"type: counter", "type: gauge"}}, want: []string{"type changed", bumpNeeded}},
		"another component":          {changes: []change{{"component: prober", "component: agent"}}, want: []string{`"l" no longer promises what it did at version 3: component changed`, bumpNeeded}},
		"the classes regrouped":      {changes: []change{{"classes: {one: [x, w]}", "classes: {one: [x], two: [w]}"}}, want: []string{`"rc" no longer promises what it did at version 3: classes changed`, bumpNeeded}},
		"an attribute renamed":       {changes: []change{{"attribute: k8s.node.name", "attribute: k8s.node"}}, want: []string{`"ra" no longer promises what it did at version 3: attribute changed`, bumpNeeded}},
		"an attribute's value":       {changes: []change{{"value: svc", "value: other"}}, want: []string{`"sn" no longer promises what it did at version 3: value changed`, bumpNeeded}},
		"a field of a line removed":  {changes: []change{{"fields: [t, tier, err]", "fields: [t, tier]"}}, want: []string{`"l" no longer promises what it did at version 3: fields err is gone`, bumpNeeded}},
		"a field of a line renamed":  {changes: []change{{"fields: [t, tier, err]", "fields: [t, tier, error]"}}, want: []string{`fields err is gone`, bumpNeeded}},
		"a field becomes a time":     {changes: []change{{"time_fields: [t]", "time_fields: [t, tier]"}}, want: []string{`"l" no longer promises what it did at version 3: fields tier changed`, bumpNeeded}},
		"a field is no longer one":   {changes: []change{{"    time_fields: [t]\n", ""}}, want: []string{`fields t changed`, bumpNeeded}},
		"another marker":             {changes: []change{{`marker: "M "`, `marker: "N "`}}, want: []string{`marker changed`, bumpNeeded}},
		"another cap":                {changes: []change{{"cap_per_window: 20", "cap_per_window: 10"}}, want: []string{`cap_per_window changed`, bumpNeeded}},
		"a cap no longer promised":   {changes: []change{{"    cap_per_window: 20\n", ""}}, want: []string{`cap_per_window is gone`, bumpNeeded}},
		"another window":             {changes: []change{{"window_seconds: 60", "window_seconds: 30"}}, want: []string{`window_seconds changed`, bumpNeeded}},
		"another stat prefix":        {changes: []change{{"stat_prefix: p_", "stat_prefix: q_"}}, want: []string{`"es" no longer promises what it did at version 3: stat_prefix changed`, bumpNeeded}},
		"another stat":               {changes: []change{{"stat: tcp.p_<c>.x", "stat: tcp.p_<c>.y"}}, want: []string{`stat changed`, bumpNeeded}},
		"another stored pattern":     {changes: []change{{"stored_name_pattern: envoy_.*", "stored_name_pattern: envoy_.+"}}, want: []string{`stored_name_pattern changed`, bumpNeeded}},
		"a render with another pair": {changes: []change{{`set: {a.b: "true"}`, `set: {a.b: "false"}`}}, want: []string{`"c" no longer promises what it did at version 3: set changed`, bumpNeeded}},
		"a render with one more pair": {
			changes: []change{{`set: {a.b: "true"}`, `set: {a.b: "true", c: d}`}},
			want:    []string{`"c" no longer promises what it did at version 3: set changed`, bumpNeeded},
		},
		"a render with no pair":      {changes: []change{{"    set: {a.b: \"true\"}\n", ""}}, want: []string{`set changed`, bumpNeeded}},
		"another release":            {changes: []change{{"release: r", "release: s"}}, want: []string{`release changed`, bumpNeeded}},
		"another chart":              {changes: []change{{"chart: x", "chart: y"}}, want: []string{`chart changed`, bumpNeeded}},
		"another install namespace":  {changes: []change{{"    namespace: ns\n    set", "    namespace: other\n    set"}}, want: []string{`"c" no longer promises what it did at version 3: namespace changed`, bumpNeeded}},
		"an object renamed":          {changes: []change{{"name: workload", "name: workload2"}}, want: []string{`"c.o" no longer promises what it did at version 3: name changed`, bumpNeeded}},
		"an object of another kind":  {changes: []change{{"kind: DaemonSet", "kind: Deployment"}}, want: []string{`kind changed`, bumpNeeded}},
		"an object removed":          {changes: []change{{"      - id: c.d\n        kind: CSIDriver\n        name_from: dom\n", ""}}, want: []string{`no longer has the entry "c.d"`, bumpNeeded}},
		"an object's namespace":      {changes: []change{{"        namespace: ns\n", ""}}, want: []string{`"c.o" no longer promises what it did at version 3: namespace is gone`, bumpNeeded}},
		"another strategy":           {changes: []change{{`maxSurge: "0"`, `maxSurge: "1"`}}, want: []string{`"c.o" no longer promises what it did at version 3: rolling_update changed`, bumpNeeded}},
		"a strategy no longer given": {changes: []change{{"        rolling_update: {maxSurge: \"0\", maxUnavailable: \"1\"}\n", ""}}, want: []string{`rolling_update is gone`, bumpNeeded}},
		"a pod label's value":        {changes: []change{{"pod_labels: {app: agent}", "pod_labels: {app: node}"}}, want: []string{`pod_labels app changed`, bumpNeeded}},
		"a pod label removed":        {changes: []change{{"        pod_labels: {app: agent}\n", ""}}, want: []string{`pod_labels app is gone`, bumpNeeded}},
		"a container removed": {
			changes: []change{{"          - name: prober\n            code_resource_attributes: [sn]\n", ""}},
			want:    []string{`"c.o" no longer promises what it did at version 3: containers prober is gone; containers prober code_resource_attributes service.name is gone`, bumpNeeded},
		},
		"a container no longer held to its environment": {changes: []change{{"            env_contains: {VAR: [\"k=\"]}\n", ""}}, want: []string{`containers agent env_contains VAR k= is gone`, bumpNeeded}},
		"a container no longer given an attribute":      {changes: []change{{"            resource_attributes: [ra]\n", ""}, {"components: [agent], ", ""}}, want: []string{`containers agent resource_attributes k8s.node.name is gone`, `components agent is gone`, bumpNeeded}},
		"an argument with another value":                {changes: []change{{`"127.0.0.1:<port>"`, `"localhost:<port>"`}}, want: []string{`containers agent args --egress changed`, bumpNeeded}},
		"an argument no longer given":                   {changes: []change{{`--domain: dom, `, ``}}, want: []string{`containers agent args --domain is gone`, bumpNeeded}},
		"a host path no longer held":                    {changes: []change{{"        host_paths: [\"/plugins/<dom>\"]\n", ""}}, want: []string{`host_paths /plugins/v is gone`, bumpNeeded}},
		"a host path of another shape": {
			changes: []change{{`host_paths: ["/plugins/<dom>"]`, `host_paths: ["/plugin/<dom>"]`}},
			want:    []string{`host_paths /plugins/v is gone`, bumpNeeded},
		},
		"one more host path": {
			changes: []change{{`host_paths: ["/plugins/<dom>"]`, `host_paths: ["/plugins/<dom>", "/registry/<dom>"]`}},
			want:    []string{noBumpNeeded, `"c.o host_paths /registry/v": "`},
		},
		"a webhook selecting by another name":  {changes: []change{{"webhooks: {hook.x: {objects: dom}}", "webhooks: {hook.x: {objects: port}}"}}, want: []string{`"c.w" no longer promises what it did at version 3: webhooks hook.x changed`, bumpNeeded}},
		"a webhook held by its other selector": {changes: []change{{"webhooks: {hook.x: {objects: dom}}", "webhooks: {hook.x: {namespaces: dom}}"}}, want: []string{`"c.w" no longer promises what it did at version 3: webhooks hook.x changed`, bumpNeeded}},
		"a webhook no longer held":             {changes: []change{{"webhooks: {hook.x: {objects: dom}}", "webhooks: {hook.y: {objects: dom}}"}}, want: []string{`webhooks hook.x is gone`, bumpNeeded}},
		"a name for an object that had none":   {changes: []change{{"kind: MutatingWebhookConfiguration\n", "kind: MutatingWebhookConfiguration\n        name: hooks\n"}}, want: []string{noBumpNeeded, `"c.w name": "`}},
		// Where a harness reads the contract's version. The key is the promise:
		// under another key a harness finds nothing, and one that finds nothing
		// cannot tell an old mesh from a renamed annotation.
		"the version annotation under another key": {changes: []change{{"value: example.io/contract-version", "value: example.io/contract"}}, want: []string{`"ann" no longer promises what it did at version 3: value changed`, `"c.o" no longer promises what it did at version 3: contract_version_annotation changed`, bumpNeeded}},
		"an object no longer carries the version":  {changes: []change{{"        contract_version_annotation: ann\n", ""}}, want: []string{`"c.o" no longer promises what it did at version 3: contract_version_annotation is gone`, bumpNeeded}},
		"one more object carries the version":      {changes: []change{{"        name_from: dom\n", "        name_from: dom\n        contract_version_annotation: ann\n"}}, want: []string{noBumpNeeded, `"c.d contract_version_annotation": "`}},
		"the version annotation named by another entry with the same value": {
			changes: []change{{"names:\n", "names:\n  - {id: ann2, value: example.io/contract-version, checked_by: //a:b}\n"}, {"contract_version_annotation: ann\n", "contract_version_annotation: ann2\n"}},
			want:    []string{noBumpNeeded, `"ann2 value": "`},
		},
		// A broken promise and a new one in the same change: the bump comes
		// first, and nothing offers the lines that would hide it.
		"a new entry beside a removed one": {
			changes: []change{{"id: es,", "id: es2,"}, {"names:\n", "names:\n  - {id: n2, value: v2, checked_by: //a:b}\n"}},
			want:    []string{`no longer has the entry "es"`, bumpNeeded},
		},
	} {
		t.Run(name, func(t *testing.T) {
			lock := lockOf(t, mustParse(t, full))
			text := full
			for _, c := range tc.changes {
				text = edit(t, text, c.from, c.to)
			}
			changed := mustParse(t, text)
			got := strings.Join(changed.CheckLock(lock), "\n")
			if len(tc.want) == 0 && got != "" {
				t.Fatalf("CheckLock() = %q, want nothing: this is not a promise", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("CheckLock() = %q, want it to contain %q", got, want)
				}
			}
			asksForBump := strings.Contains(got, bumpNeeded)
			asksForLine := strings.Contains(got, noBumpNeeded)
			if len(tc.want) > 0 && asksForBump == asksForLine {
				t.Errorf("CheckLock() = %q: it asks for a bump or for a line in the lock, never both and never neither", got)
			}
			// A failure that asks for a bump never holds a digest: the lines
			// that would make the test pass without the bump are not on offer.
			if asksForBump {
				for _, digest := range changed.digests() {
					if strings.Contains(got, digest) {
						t.Errorf("CheckLock() asks for a bump and prints the digest %s: %q", digest, got)
					}
				}
			}
			if !asksForLine {
				return
			}
			// The lines it prints are the ones that settle it.
			_, lines, _ := strings.Cut(got, "\n\n")
			settled, err := ParseLock([]byte(string(lockOf(t, mustParse(t, full)).render()) + lines))
			if err != nil {
				t.Fatal(err)
			}
			if again := changed.CheckLock(settled); len(again) != 0 {
				t.Errorf("with the printed lines added, CheckLock() = %q", again)
			}
		})
	}
}

// render writes a lock as the file holds it, without the header.
func (l *Lock) render() []byte {
	var b strings.Builder
	b.WriteString("version: 3\npromises:\n")
	for _, name := range sortedKeys(l.Promises) {
		b.WriteString(lockLine(name, l.Promises[name]) + "\n")
	}
	return []byte(b.String())
}

// TestCheckLock_Versions: a bump is by one, and the lock moves with it as a
// whole.
func TestCheckLock_Versions(t *testing.T) {
	old := lockOf(t, mustParse(t, full))
	breaking := edit(t, full, "{id: dom, value: v,", "{id: dom, value: v9,")

	t.Run("the bump the broken promise asked for", func(t *testing.T) {
		bumped := mustParse(t, edit(t, breaking, "version: 3", "version: 4"))
		got := strings.Join(bumped.CheckLock(old), "\n")
		for _, want := range []string{"is at version 4", "still holds the promises of version 3", "Replace that file with this"} {
			if !strings.Contains(got, want) {
				t.Errorf("CheckLock() = %q, want it to contain %q", got, want)
			}
		}
		// What it prints is the new lock, header and all.
		_, file, _ := strings.Cut(got, "\n\n")
		if file != string(bumped.Lock()) || !strings.HasPrefix(file, "# The promises") {
			t.Fatalf("CheckLock() printed %q, want the lock of the bumped contract", file)
		}
		replaced, err := ParseLock([]byte(file))
		if err != nil {
			t.Fatal(err)
		}
		if replaced.Version != 4 {
			t.Errorf("the printed lock is for version %d, want 4", replaced.Version)
		}
		if again := bumped.CheckLock(replaced); len(again) != 0 {
			t.Errorf("with the lock replaced, CheckLock() = %q", again)
		}
	})
	t.Run("a bump with the lock left behind and nothing else changed", func(t *testing.T) {
		got := strings.Join(mustParse(t, edit(t, full, "version: 3", "version: 4")).CheckLock(old), "\n")
		if !strings.Contains(got, "Replace that file with this") {
			t.Errorf("CheckLock() = %q", got)
		}
	})
	t.Run("a bump by two", func(t *testing.T) {
		got := strings.Join(mustParse(t, edit(t, full, "version: 3", "version: 5")).CheckLock(old), "\n")
		if !strings.Contains(got, "went from version 3 to 5: a bump is by one") {
			t.Errorf("CheckLock() = %q", got)
		}
	})
	t.Run("a version that goes back", func(t *testing.T) {
		got := mustParse(t, edit(t, full, "version: 3", "version: 2")).CheckLock(old)
		if len(got) != 1 || !strings.Contains(got[0], "is at version 2") || !strings.Contains(got[0], "never goes back") {
			t.Errorf("CheckLock() = %q", got)
		}
	})
	t.Run("a digest edited by hand", func(t *testing.T) {
		c := mustParse(t, full)
		lock := lockOf(t, c)
		lock.Promises["dom value"] = "0000000000000000"
		got := strings.Join(c.CheckLock(lock), "\n")
		if !strings.Contains(got, `the entry "dom" no longer promises what it did at version 3: value changed`) {
			t.Errorf("CheckLock() = %q", got)
		}
	})
}

// TestLock: the lock a contract writes is one it accepts, and holds one line
// per promise.
func TestLock(t *testing.T) {
	c := mustParse(t, full)
	lock := lockOf(t, c)
	if lock.Version != 3 || len(lock.Promises) != len(c.Promises()) || len(lock.Promises) == 0 {
		t.Fatalf("the lock is for version %d with %d promises; the contract is at 3 with %d", lock.Version, len(lock.Promises), len(c.Promises()))
	}
	if got := c.CheckLock(lock); len(got) != 0 {
		t.Errorf("CheckLock() = %q", got)
	}
	// The same promise under another name is another digest: a line cannot be
	// copied from one entry to the next.
	seen := map[string]string{}
	for name, digest := range lock.Promises {
		if other, ok := seen[digest]; ok {
			t.Errorf("%q and %q have the digest %s", name, other, digest)
		}
		seen[digest] = name
	}
}

func TestParseLockRejects(t *testing.T) {
	for name, text := range map[string]string{
		"an unknown key":         "version: 1\npromise: {}\n",
		"a promise listed twice": "version: 1\npromises:\n  \"a b\": \"1\"\n  \"a b\": \"2\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLock([]byte(text)); err == nil || !strings.Contains(err.Error(), LockFile) {
				t.Errorf("ParseLock() error = %v, want one naming %s", err, LockFile)
			}
		})
	}
}

// TestEveryKeyIsAPromise: every key an entry can have is promised under some
// name, or is one of the three that promise nothing. A key added to an entry
// type is a promise unless it is added to notPromised, and `full` has to set
// it for this to pass.
func TestEveryKeyIsAPromise(t *testing.T) {
	// Where a key is promised under another word of the name.
	under := map[string]string{
		"Render.objects":      "render", // every object says which render it is of
		"Object.name_from":    "name",   // the name, wherever it is taken from
		"LogLine.time_fields": "fields", // what a field is
		"Container.name":      "containers",
	}
	words := map[string]bool{}
	for name := range mustParse(t, full).Promises() {
		for _, w := range strings.Fields(name) {
			words[w] = true
		}
	}
	for _, entry := range []any{Metric{}, ReasonClasses{}, ResourceAttribute{}, LogLine{}, EnvoyStat{}, Name{}, Render{}, Object{}, Container{}} {
		typ := reflect.TypeOf(entry)
		for i := range typ.NumField() {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if key == "" || slices.Contains(notPromised, key) {
				continue
			}
			if word, ok := under[typ.Name()+"."+key]; ok {
				key = word
			}
			if !words[key] {
				t.Errorf("no promise of the contract `full` is named after the key %q of %s: a harness could lose what it says and the version would not have to move. Promise it in Promises (and set it in `full`), or add it to notPromised.", key, typ.Name())
			}
		}
	}
}
