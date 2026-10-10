package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"aethermesh.dev/test/harnesscontract"
)

// The udsecho entry of the real contract, against canned renders: no helm.
const udsecho = `---
kind: Deployment
metadata: {name: uds-echo, namespace: mesh-workloads}
spec:
  template:
    metadata: {labels: {app: uds-echo}}
---
kind: Deployment
metadata: {name: uds-cr-echo, namespace: mesh-workloads}
spec:
  template:
    metadata: {labels: {app: uds-cr-echo}}
`

func fake(out, errOut string, err error) renderFunc {
	return func(string, []string) ([]byte, []byte, error) { return []byte(out), []byte(errOut), err }
}

func TestRun(t *testing.T) {
	const (
		all  = "chart.udsecho,chart.udsecho.annotation_path,chart.udsecho.policy_path"
		self = "//test/harnesscontract:udsecho_chart_test"
	)
	for name, tc := range map[string]struct {
		chart  string
		target string
		ids    string
		render renderFunc
		code   int
		want   string
	}{
		"the render the contract describes": {"udsecho", self, all, fake(udsecho, "", nil), 0, "PASS: chart.udsecho (chart.tgz)"},
		"a renamed Deployment": {
			"udsecho", self, all, fake(strings.Replace(udsecho, "name: uds-cr-echo", "name: uds-policy-echo", 1), "", nil), 1,
			"Deployment/uds-cr-echo is not rendered (the render's Deployment objects: uds-echo, uds-policy-echo)",
		},
		"helm fails":                      {"udsecho", self, all, fake("kind: Secret\ndata: {k: c2VjcmV0}\n", "Error: boom", errors.New("exit status 1")), 1, "Error: boom"},
		"a chart the contract lacks":      {"no-such-chart", self, all, fake("", "", nil), 1, "has no entry under `charts`"},
		"a missing flag is a usage error": {"", self, all, fake("", "", nil), 2, "usage:"},
		"no target is a usage error":      {"udsecho", "", all, fake(udsecho, "", nil), 2, "usage:"},
		"an object the test does not list": {
			"udsecho", self, "chart.udsecho,chart.udsecho.annotation_path", fake(udsecho, "", nil), 1,
			`has the chart entry "chart.udsecho.policy_path", and the test that renders its chart does not list it`,
		},
		"an object removed from the contract": {
			"udsecho", self, all + ",chart.udsecho.client", fake(udsecho, "", nil), 1,
			`no longer has the chart entry "chart.udsecho.client"`,
		},
		// The contract names another test for this chart's render: the test
		// that renders the chart is the one that has to be named.
		"a render whose checked_by names another test": {
			"udsecho", "//test/harnesscontract:prober_chart_test", all, fake(udsecho, "", nil), 1,
			`chart.udsecho is a render of the chart "udsecho" and its checked_by does not name //test/harnesscontract:prober_chart_test`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(&stdout, &stderr, "helm", []string{"chart.tgz"}, tc.chart, tc.target, strings.Split(tc.ids, ","), tc.render)
			all := stdout.String() + stderr.String()
			if code != tc.code || !strings.Contains(all, tc.want) {
				t.Errorf("run() = %d, output %q; want %d and %q", code, all, tc.code, tc.want)
			}
			if code == 1 && !strings.Contains(all, harnesscontract.File) {
				t.Errorf("a failure does not name the contract file: %q", all)
			}
			if strings.Contains(all, "c2VjcmV0") {
				t.Errorf("run() printed the render: %q", all)
			}
		})
	}
}

// TestRunEveryPackaging: a chart published under two packagings is rendered
// from each, and one of them rendering something else fails the test however
// the other renders.
func TestRunEveryPackaging(t *testing.T) {
	const (
		all  = "chart.udsecho,chart.udsecho.annotation_path,chart.udsecho.policy_path"
		self = "//test/harnesscontract:udsecho_chart_test"
	)
	// Which package helm was given is the argument after the release name.
	byPackage := func(renders map[string]string) renderFunc {
		return func(_ string, args []string) ([]byte, []byte, error) { return []byte(renders[args[2]]), nil, nil }
	}
	renamed := strings.Replace(udsecho, "name: uds-cr-echo", "name: uds-policy-echo", 1)
	for name, tc := range map[string]struct {
		charts []string
		render renderFunc
		code   int
		want   []string
	}{
		"both render what the contract describes": {
			[]string{"a.tgz", "b.tgz"},
			byPackage(map[string]string{"a.tgz": udsecho, "b.tgz": udsecho}), 0,
			[]string{"PASS: chart.udsecho (a.tgz)", "PASS: chart.udsecho (b.tgz)"},
		},
		"the second renders something else": {
			[]string{"a.tgz", "b.tgz"},
			byPackage(map[string]string{"a.tgz": udsecho, "b.tgz": renamed}), 1,
			[]string{"PASS: chart.udsecho (a.tgz)", "Deployment/uds-cr-echo is not rendered", "helm template udsecho b.tgz "},
		},
		"the first renders something else": {
			[]string{"a.tgz", "b.tgz"},
			byPackage(map[string]string{"a.tgz": renamed, "b.tgz": udsecho}), 1,
			[]string{"PASS: chart.udsecho (b.tgz)", "Deployment/uds-cr-echo is not rendered", "helm template udsecho a.tgz "},
		},
		"an empty package path is a usage error": {[]string{"a.tgz", ""}, byPackage(nil), 2, []string{"usage:"}},
		"no package is a usage error":            {nil, byPackage(nil), 2, []string{"usage:"}},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(&stdout, &stderr, "helm", tc.charts, "udsecho", self, strings.Split(all, ","), tc.render)
			out := stdout.String() + stderr.String()
			if code != tc.code {
				t.Errorf("run() = %d, want %d; output %q", code, tc.code, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("run() output %q, want it to contain %q", out, want)
				}
			}
		})
	}
}
