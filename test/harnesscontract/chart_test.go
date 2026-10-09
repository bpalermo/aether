package harnesscontract

import (
	"slices"
	"strings"
	"testing"
)

// render is a small `helm template` output: a comment-only document, a
// DaemonSet, a Deployment whose strategy is a percentage, and a Secret whose
// data must never appear in a failure.
const render = `---
# Source: x/templates/notes.yaml
---
# Source: x/templates/ds.yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: aether-agent
  namespace: aether-system
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
  template:
    metadata:
      labels:
        app.kubernetes.io/name: aether-agent
    spec:
      initContainers:
        - name: cni-install
      containers:
        - name: agent
          env:
            - name: OTEL_RESOURCE_ATTRIBUTES
              value: "k8s.node.name=$(NODE_NAME)"
            - name: TOKEN
              value: hunter2
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: aether-edge
  namespace: aether-ingress
spec:
  strategy:
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 0
  template:
    metadata:
      labels: {app: edge}
    spec:
      containers:
        - name: envoy
---
apiVersion: v1
kind: Secret
metadata:
  name: webhook
data:
  tls.key: c2VjcmV0LWtleQ==
`

func agentObject() Object {
	return Object{
		ID: "o", Kind: "DaemonSet", Name: "aether-agent", Namespace: "aether-system",
		PodLabels:     map[string]string{"app.kubernetes.io/name": "aether-agent"},
		Containers:    []Container{{Name: "agent", EnvContains: map[string][]string{"OTEL_RESOURCE_ATTRIBUTES": {"k8s.node.name="}}}},
		RollingUpdate: &RollingUpdate{MaxSurge: "0", MaxUnavailable: "1"},
	}
}

func TestRenderCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Object)
		want string // "" = no problem
	}{
		"as rendered":          {func(*Object) {}, ""},
		"a renamed workload":   {func(o *Object) { o.Name = "aether-node-agent" }, "DaemonSet/aether-node-agent is not rendered (the render's DaemonSet objects: aether-agent)"},
		"another kind":         {func(o *Object) { o.Kind = "StatefulSet" }, "is not rendered (the render's StatefulSet objects: none)"},
		"another namespace":    {func(o *Object) { o.Namespace = "mesh" }, `is in namespace "aether-system", the contract says "mesh"`},
		"a renamed pod label":  {func(o *Object) { o.PodLabels = map[string]string{"app": "agent"} }, "has no pod label app=agent (its pod labels: app.kubernetes.io/name=aether-agent)"},
		"a renamed container":  {func(o *Object) { o.Containers[0].Name = "node-agent" }, `has no container "node-agent" (its containers: agent)`},
		"an init container":    {func(o *Object) { o.Containers[0] = Container{Name: "cni-install"} }, `has no container "cni-install"`},
		"another strategy":     {func(o *Object) { o.RollingUpdate.MaxSurge = "1" }, "rolls with maxSurge=0 maxUnavailable=1, the contract says maxSurge=1 maxUnavailable=1"},
		"an env without value": {func(o *Object) { o.Containers[0].EnvContains["OTEL_RESOURCE_ATTRIBUTES"] = []string{"k8s.pod.name="} }, `OTEL_RESOURCE_ATTRIBUTES does not contain "k8s.pod.name="`},
		"an env that is unset": {func(o *Object) { o.Containers[0].EnvContains = map[string][]string{"NODE": {"x"}} }, `NODE does not contain "x"`},
		"a percentage": {func(o *Object) {
			*o = Object{ID: "e", Kind: "Deployment", Name: "aether-edge", RollingUpdate: &RollingUpdate{MaxSurge: "25%", MaxUnavailable: "0"}}
		}, ""},
		"no strategy rendered": {func(o *Object) {
			*o = Object{ID: "s", Kind: "Secret", Name: "webhook", RollingUpdate: &RollingUpdate{MaxSurge: "1"}}
		}, "rolls with maxSurge=unset maxUnavailable=unset"},
	} {
		t.Run(name, func(t *testing.T) {
			o := agentObject()
			tc.edit(&o)
			got := strings.Join(Render{ID: "r", Objects: []Object{o}}.Check([]byte(render)), "\n")
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Errorf("Check() = %q, want it to contain %q", got, tc.want)
			}
			// Whatever is reported, no value of the render that could be a
			// secret is in it.
			for _, secret := range []string{"hunter2", "c2VjcmV0LWtleQ=="} {
				if strings.Contains(got, secret) {
					t.Errorf("Check() printed %q from the render: %q", secret, got)
				}
			}
		})
	}
}

func TestRenderCheck_ObjectRenderedTwice(t *testing.T) {
	twice := render + "---\nkind: Secret\nmetadata:\n  name: webhook\n"
	got := Render{ID: "r", Objects: []Object{{ID: "s", Kind: "Secret", Name: "webhook"}}}.Check([]byte(twice))
	if len(got) != 1 || !strings.Contains(got[0], "is rendered 2 times") {
		t.Errorf("Check() = %q", got)
	}
}

// A document that is not YAML is reported without its text.
func TestRenderCheck_UnreadableDocument(t *testing.T) {
	got := Render{ID: "r", Objects: []Object{agentObject()}}.Check([]byte("kind: [secret-looking-text\n"))
	if len(got) != 1 || !strings.Contains(got[0], "not YAML this check can read") || strings.Contains(got[0], "secret-looking-text") {
		t.Errorf("Check() = %q", got)
	}
}

func TestOwnedIDs(t *testing.T) {
	renders := []Render{{ID: "r", Objects: []Object{{ID: "r.a"}, {ID: "r.b"}}}}
	if got := OwnedIDs(renders, []string{"r", "r.a", "r.b"}); len(got) != 0 {
		t.Errorf("OwnedIDs() = %q, want nothing", got)
	}
	got := strings.Join(OwnedIDs(renders, []string{"r", "r.a", "r.gone"}), "\n")
	for _, want := range []string{`no longer has the chart entry "r.gone"`, `has the chart entry "r.b", and the test that renders its chart does not list it`} {
		if !strings.Contains(got, want) {
			t.Errorf("OwnedIDs() = %q, want it to contain %q", got, want)
		}
	}
}

func TestHelmArgs(t *testing.T) {
	r := Render{Release: "aether", Namespace: "aether-system", Set: map[string]string{"b.c": "true", "a": "1"}}
	want := []string{"template", "aether", "chart.tgz", "--namespace", "aether-system", "--set", "a=1", "--set", "b.c=true"}
	if got := r.HelmArgs("chart.tgz"); !slices.Equal(got, want) {
		t.Errorf("HelmArgs() = %q, want %q", got, want)
	}
}

func TestRendersOf(t *testing.T) {
	c := MustLoad(t)
	if got := c.RendersOf("aether"); len(got) < 2 {
		t.Errorf("the contract has %d renders of the aether chart, want the default one and the surge one", len(got))
	}
	if got := c.RendersOf("no-such-chart"); len(got) != 0 {
		t.Errorf("RendersOf(no-such-chart) = %v", got)
	}
}
