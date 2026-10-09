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
metadata: {name: uds-echo}
spec:
  template:
    metadata: {labels: {app: uds-echo}}
---
kind: Deployment
metadata: {name: uds-cr-echo}
spec:
  template:
    metadata: {labels: {app: uds-cr-echo}}
`

func fake(out, errOut string, err error) renderFunc {
	return func(string, []string) ([]byte, []byte, error) { return []byte(out), []byte(errOut), err }
}

func TestRun(t *testing.T) {
	for name, tc := range map[string]struct {
		chart  string
		render renderFunc
		code   int
		want   string
	}{
		"the render the contract describes": {"udsecho", fake(udsecho, "", nil), 0, "PASS: chart.udsecho"},
		"a renamed Deployment": {
			"udsecho", fake(strings.Replace(udsecho, "name: uds-cr-echo", "name: uds-policy-echo", 1), "", nil), 1,
			"Deployment/uds-cr-echo is not rendered (the render's Deployment objects: uds-echo, uds-policy-echo)",
		},
		"helm fails":                      {"udsecho", fake("kind: Secret\ndata: {k: c2VjcmV0}\n", "Error: boom", errors.New("exit status 1")), 1, "Error: boom"},
		"a chart the contract lacks":      {"no-such-chart", fake("", "", nil), 1, "has no entry under `charts`"},
		"a missing flag is a usage error": {"", fake("", "", nil), 2, "usage:"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(&stdout, &stderr, "helm", "chart.tgz", tc.chart, tc.render)
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
