package xdsconst

import (
	"testing"

	"aethermesh.dev/test/harnesscontract"
)

// TestExternalHarnessContract holds the annotation a harness outside this
// repository writes on its own load generators' pods to the contract
// (test/harnesscontract/external-harness.yaml). It is checked here because
// this package is internal to the agent.
func TestExternalHarnessContract(t *testing.T) {
	harnesscontract.MustLoad(t).CheckNames(t, "//agent/internal/xds/xdsconst:xdsconst_test", map[string]string{
		"pod.annotation.upstreams": AnnotationConfigUpstreams,
	})
}
