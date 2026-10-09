package cmd

import (
	"context"
	"testing"

	"aethermesh.dev/common/telemetry/setup"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTelemetryResourceHasNoHostName pins that the controller's telemetry resource
// carries no host.name (#1596).
//
// The controller is a Deployment on the pod network, so the hostname the OTel host
// detector reads is the POD's name. A collector that derives a node label from
// host.name ahead of k8s.node.name then labels every controller series with a pod
// (#1041, where the prober had the same resource). The pod is already named by
// k8s.pod.name, which the chart sets, so nothing is lost by leaving host.name out.
//
// It reads the package's own cfg, the one the command builds its logger, meter and
// tracer providers from, so a default that drops the choice fails here.
func TestTelemetryResourceHasNoHostName(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.pod.name=aether-controller-0,k8s.namespace.name=aether-system")
	t.Setenv("OTEL_SERVICE_NAME", "")

	res, err := setup.NewResource(context.Background(), cfg.Telemetry(name, Version))
	require.NoError(t, err)

	got, _ := res.Set().Value("service.name")
	assert.Equal(t, name, got.AsString(), "this is the controller's resource")
	host, has := res.Set().Value("host.name")
	assert.False(t, has, "host.name = %q: on the pod network that is the pod name, not a host", host.AsString())
	pod, _ := res.Set().Value("k8s.pod.name")
	assert.Equal(t, "aether-controller-0", pod.AsString(), "the pod is named by k8s.pod.name, from the chart")
}
