// Package prober_test ties the names this chart writes on its own pods to the
// code that reads them (#1589). The chart spells the mesh label and the mesh
// annotations as literals; a rename in the Go constants would leave the prober
// unmeshed, or without its upstreams, while every pattern test still passed.
package prober_test

import (
	"sort"
	"strings"
	"testing"

	"aethermesh.dev/bazel/helm/rendertest"
	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/labels"
	"aethermesh.dev/test/harnesscontract"
)

// upstreamsAnnotation is config.aether.io/upstreams. Its Go constant is
// internal to the agent (agent/internal/xds/xdsconst), which a chart test
// cannot import; //agent/internal/xds/xdsconst:xdsconst_test holds that
// constant to the external-harness contract's entry, and this test holds the
// chart to the same entry.
func upstreamsAnnotation(t *testing.T) string {
	t.Helper()
	const id = "pod.annotation.upstreams"
	for _, n := range harnesscontract.MustLoad(t).Names {
		if n.ID == id {
			return n.Value
		}
	}
	t.Fatalf("%s has no entry %s", harnesscontract.File, id)
	return ""
}

// pod is what one workload's pod template must carry: the mesh label, and
// these mesh annotations (key -> value; "" when only the key is checked).
type pod struct {
	annotations map[string]string
}

func TestMeshNames(t *testing.T) {
	upstreams := upstreamsAnnotation(t)
	want := map[string]pod{
		"DaemonSet/release-name-prober": {annotations: map[string]string{upstreams: ""}},
		"Deployment/authz-echo":         {annotations: map[string]string{annotations.AnnotationEndpointPort: "8080"}},
		"Deployment/authz-canary":       {annotations: map[string]string{upstreams: "authz-echo"}},
	}

	// The default targets give the DaemonSet its upstreams annotation; the
	// canary's two Deployments are an option.
	objects := rendertest.Render(t, "--set", "authzCanary.enabled=true")
	workloads := rendertest.Workloads(objects)
	if len(workloads) != len(want) {
		t.Fatalf("the render holds %d workloads, want %d: add the new one to this test", len(workloads), len(want))
	}
	for _, w := range workloads {
		expect, ok := want[w.ID()]
		if !ok {
			t.Errorf("%s is not a workload this test knows: add it, with the mesh names its pods carry", w.ID())
			continue
		}
		meta := w.Template.Metadata
		if got := meta.Labels[labels.LabelAetherManaged]; got != "true" {
			t.Errorf("%s: pod label %s=%q, want \"true\" (labels.LabelAetherManaged): the pod would not be in the mesh. Pod labels: %v", w.ID(), labels.LabelAetherManaged, got, meta.Labels)
		}
		for key, value := range expect.annotations {
			got, ok := meta.Annotations[key]
			if !ok {
				t.Errorf("%s: no pod annotation %s. Pod annotations: %v", w.ID(), key, meta.Annotations)
			} else if value != "" && got != value {
				t.Errorf("%s: pod annotation %s=%q, want %q", w.ID(), key, got, value)
			}
		}
		// Closed: a mesh name on the pod that the code does not know is a
		// literal that drifted, or a new one this test has to learn.
		for _, key := range meshKeys(meta.Labels) {
			if key != labels.LabelAetherManaged {
				t.Errorf("%s: pod label %s is not a mesh label this test ties to a Go constant", w.ID(), key)
			}
		}
		for _, key := range meshKeys(meta.Annotations) {
			if _, ok := expect.annotations[key]; !ok {
				t.Errorf("%s: pod annotation %s is not one this test ties to a Go constant", w.ID(), key)
			}
		}
	}
}

// meshKeys returns the keys in the mesh's namespaces (aether.io and its
// subdomains), sorted.
func meshKeys(m map[string]string) []string {
	var keys []string
	for k := range m {
		prefix, _, _ := strings.Cut(k, "/")
		if prefix == "aether.io" || strings.HasSuffix(prefix, ".aether.io") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
