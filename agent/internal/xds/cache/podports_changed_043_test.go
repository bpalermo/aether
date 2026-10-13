package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnPodPortsChanged_SaysWhenTheAnswerChanged: a route's status is judged
// by PodPortClasses, and the reconciler that writes it is told when that
// answer has changed (it was a 30 s timer). The case that matters most is the
// reconciler's own: its reconcile of a route event makes the route's services
// dependencies, and the refresh that then holds their rows comes AFTER it.
// The hook fires for that refresh, for a change of what the pods declare and
// for the rows going; it does not fire for a refresh that changes nothing,
// which would be a reconcile of every route on every endpoint event.
func TestOnPodPortsChanged_SaysWhenTheAnswerChanged(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	echo := func(class registryv1.PortProtocol) []a043Service {
		return []a043Service{{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: class}, ip: "10.0.9.1"}}
	}
	c := a043Cache(t, nil)
	fired := 0
	c.OnPodPortsChanged(func() { fired++ })
	load := func(rows []a043Service) int {
		t.Helper()
		before := fired
		// A registry scoped to the dependency set, as the agent's is.
		if _, dep := c.DependencySet()["demo/echo"]; !dep {
			rows = nil
		}
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(rows)))
		return fired - before
	}

	assert.Zero(t, load(echo(h)), "not a dependency: nothing is known of it, before or after")
	_, known := c.PodPortClasses(ctx, "demo/echo")
	require.False(t, known)

	// The reconcile of a route event: the route's backend is a dependency
	// from here on. Nothing has changed in the cache's answer yet.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/echo", Cluster: "echo.demo." + a043Mesh, Port: 8080, Weight: 1}},
	}}})
	_, known = c.PodPortClasses(ctx, "demo/echo")
	require.False(t, known, "control: the reconcile itself reads no registry")

	assert.Equal(t, 1, load(echo(h)), "the refresh that holds the rows of a route's service for the first time")
	classes, known := c.PodPortClasses(ctx, "demo/echo")
	require.True(t, known)
	require.Equal(t, h, classes[8080])

	assert.Zero(t, load(echo(h)), "a refresh that changes no port")
	assert.Equal(t, 1, load(echo(tcp)), "the pods now declare the port raw TCP")
	assert.Zero(t, load(echo(tcp)))
}
