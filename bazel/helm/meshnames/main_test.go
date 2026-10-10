package main

import (
	"strings"
	"testing"

	"aethermesh.dev/bazel/helm/render"
)

// chart renders three workloads the way the udsecho chart writes them, with
// one substitution per %s so a test can break a single name.
func chart(t *testing.T, replace ...string) []render.Object {
	t.Helper()
	text := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
spec:
  template:
    metadata:
      labels:
        app: echo
        aether.io/managed: "true"
      annotations:
        endpoint.aether.io/port: "8080"
        endpoint.aether.io/uds-socket: "s/a.sock"
    spec:
      containers:
        - name: app
      volumes:
        - name: s
          csi: {driver: csi.aether.io}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: client
spec:
  template:
    metadata:
      labels:
        aether.io/managed: "true"
      annotations:
        config.aether.io/upstreams: "echo"
    spec:
      containers:
        - name: curl
`
	text = strings.NewReplacer(replace...).Replace(text)
	objects, err := render.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

// listed is what a BUILD file says of the chart above: the workloads, and what
// each carries.
type listed struct{ workloads, carries []string }

var asWritten = listed{
	workloads: []string{"Deployment/echo", "Deployment/client"},
	carries: []string{
		"Deployment/echo=endpoint_port=8080",
		"Deployment/echo=uds_socket=s/a.sock",
		"Deployment/echo=csi",
		"Deployment/client=upstreams",
	},
}

func wanted(t *testing.T, l listed) map[string]want {
	t.Helper()
	symbols, err := annotationSymbols()
	if err != nil {
		t.Fatal(err)
	}
	w, err := parseWorkloads(l.workloads, l.carries, symbols)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCheckPasses(t *testing.T) {
	if problems := check(chart(t), wanted(t, asWritten)); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
}

// Each case breaks one name in the chart, or one line of the list, and names a
// fragment of the problem that must be reported.
func TestCheckFindsADriftedName(t *testing.T) {
	cases := []struct {
		name    string
		replace []string
		listed  *listed
		problem string
	}{
		{name: "mesh label", replace: []string{"aether.io/managed", "aether.io/manged"}, problem: "would not be in the mesh"},
		{name: "mesh label value", replace: []string{`aether.io/managed: "true"`, `aether.io/managed: "yes"`}, problem: "would not be in the mesh"},
		{name: "annotation key", replace: []string{"endpoint.aether.io/uds-socket", "endpoint.aether.io/uds-sock"}, problem: "no pod annotation endpoint.aether.io/uds-socket"},
		{name: "annotation key, unknown one left behind", replace: []string{"endpoint.aether.io/uds-socket", "endpoint.aether.io/uds-sock"}, problem: "endpoint.aether.io/uds-sock is not one"},
		{name: "annotation value", replace: []string{`"8080"`, `"9090"`}, problem: `want "8080"`},
		{name: "upstreams key", replace: []string{"config.aether.io/upstreams", "config.aether.io/upstream"}, problem: "no pod annotation config.aether.io/upstreams"},
		{name: "CSI driver", replace: []string{"csi.aether.io", "csi.aether.dev"}, problem: "want exactly one volume of csi.aether.io"},
		{name: "CSI volume nobody listed", listed: &listed{workloads: asWritten.workloads, carries: []string{"Deployment/echo=endpoint_port", "Deployment/echo=uds_socket", "Deployment/client=upstreams"}}, problem: "the test lists none"},
		{name: "workload nobody listed", listed: &listed{workloads: asWritten.workloads[:1], carries: asWritten.carries[:3]}, problem: "Deployment/client is a workload the test does not list"},
		{name: "listed workload not rendered", listed: &listed{workloads: append([]string{"Deployment/gone"}, asWritten.workloads...), carries: asWritten.carries}, problem: "Deployment/gone is listed and the chart does not render it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := asWritten
			if c.listed != nil {
				l = *c.listed
			}
			problems := strings.Join(check(chart(t, c.replace...), wanted(t, l)), "\n")
			if !strings.Contains(problems, c.problem) {
				t.Fatalf("problems do not mention %q:\n%s", c.problem, problems)
			}
		})
	}
}

func TestParseWorkloadsRejects(t *testing.T) {
	symbols := map[string]string{"upstreams": "config.aether.io/upstreams"}
	for _, l := range []listed{
		{workloads: []string{"Deployment/a=upstreams"}},
		{workloads: []string{""}},
		{workloads: []string{"Deployment/a", "Deployment/a"}},
		{workloads: []string{"Deployment/a"}, carries: []string{"Deployment/a=nope"}},
		{workloads: []string{"Deployment/a"}, carries: []string{"Deployment/a"}},
		{workloads: []string{"Deployment/a"}, carries: []string{"Deployment/b=upstreams"}},
	} {
		if _, err := parseWorkloads(l.workloads, l.carries, symbols); err == nil {
			t.Errorf("parseWorkloads(%q, %q) returned no error", l.workloads, l.carries)
		}
	}

	// A value may hold a comma and an equals sign.
	w, err := parseWorkloads([]string{"Deployment/a"}, []string{"Deployment/a=upstreams=x,y=z"}, symbols)
	if err != nil || w["Deployment/a"].annotations["config.aether.io/upstreams"] != "x,y=z" {
		t.Errorf("parseWorkloads kept %v (err %v), want the value x,y=z", w, err)
	}
}

func TestRunNeedsItsFlags(t *testing.T) {
	if err := run([]string{"--helm", "helm"}); err == nil {
		t.Fatal("run with no chart and no workload returned no error")
	}
}
