// Package udsecho_test ties the names this chart writes on its own pods to the
// code that reads them (#1589). The chart spells the mesh label, the mesh
// annotations and the CSI driver of the socket volume as literals; a rename in
// the Go constants would leave the echo pods unmeshed, or without the volume
// their socket lives in, while every pattern test still passed.
package udsecho_test

import (
	"sort"
	"strings"
	"testing"

	"aethermesh.dev/bazel/helm/rendertest"
	"aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/udspath"
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

// pod is what one workload's pod template must carry besides the mesh label:
// these mesh annotations (key -> value), and, when csi is set, exactly one
// volume of the mesh's CSI driver.
type pod struct {
	annotations map[string]string
	csi         bool
}

func TestMeshNames(t *testing.T) {
	want := map[string]pod{
		"Deployment/uds-echo": {
			annotations: map[string]string{
				annotations.AnnotationEndpointPort:      "8080",
				annotations.AnnotationEndpointUDSSocket: "s/a.sock",
			},
			csi: true,
		},
		// The socket of this one comes from the EndpointPolicy, not from an
		// annotation.
		"Deployment/uds-cr-echo": {
			annotations: map[string]string{annotations.AnnotationEndpointPort: "8080"},
			csi:         true,
		},
		"Deployment/uds-client": {
			annotations: map[string]string{upstreamsAnnotation(t): "uds-echo,uds-cr-echo"},
		},
	}

	workloads := rendertest.Workloads(rendertest.Render(t))
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
			if got, ok := meta.Annotations[key]; !ok || got != value {
				t.Errorf("%s: pod annotation %s=%q (present: %t), want %q. Pod annotations: %v", w.ID(), key, got, ok, value, meta.Annotations)
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
		checkCSI(t, w, expect.csi)
	}
}

// checkCSI holds a pod's CSI volumes to udspath.CSIDriver, the driver name the
// agent resolves a socket request against and the node plugin registers: a
// volume of any other driver is not the mesh's carrier, and the agent refuses
// the pod's socket (`not_csi`).
func checkCSI(t *testing.T, w rendertest.Object, want bool) {
	t.Helper()
	var drivers []string
	for _, v := range w.Template.Spec.Volumes {
		if v.CSI != nil {
			drivers = append(drivers, v.CSI.Driver)
		}
	}
	switch {
	case !want && len(drivers) != 0:
		t.Errorf("%s: CSI volumes of %v, want none", w.ID(), drivers)
	case want && (len(drivers) != 1 || drivers[0] != udspath.CSIDriver):
		t.Errorf("%s: CSI volume drivers %v, want exactly one volume of %s (udspath.CSIDriver)", w.ID(), drivers, udspath.CSIDriver)
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
