package services

// Pods as their manifests declare them, and the listings the registry builds
// from them: shared by the tests that drive the generator with pods that may
// disagree about everything.

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/registry/endpointmeta"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// declaredPod is a pod as its manifest declares it.
type declaredPod struct {
	ip          string
	annotations map[string]string
}

func (p declaredPod) String() string {
	return fmt.Sprintf("{protocol=%q port=%q ports=%q}",
		p.annotations[aetherannotations.AnnotationEndpointProtocol],
		p.annotations[aetherannotations.AnnotationEndpointPort],
		p.annotations[aetherannotations.AnnotationEndpointPorts])
}

func annotated(protocol, port, ports string) map[string]string {
	a := map[string]string{}
	if protocol != "" {
		a[aetherannotations.AnnotationEndpointProtocol] = protocol
	}
	if port != "" {
		a[aetherannotations.AnnotationEndpointPort] = port
	}
	if ports != "" {
		a[aetherannotations.AnnotationEndpointPorts] = ports
	}
	return a
}

// row builds the registry row both backends build from the annotations: they
// use these same parsers.
func (p declaredPod) row(t testing.TB) *registryv1.ServiceEndpoint {
	t.Helper()
	port, err := endpointmeta.Port(p.annotations)
	require.NoError(t, err)
	ports, err := endpointmeta.Ports(p.annotations, port)
	require.NoError(t, err)
	pp, err := endpointmeta.PortProtocols(p.annotations)
	require.NoError(t, err)
	return &registryv1.ServiceEndpoint{
		Ip: p.ip, ClusterName: "c", Port: uint32(port), Ports: ports, PortProtocols: pp,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "ns"},
	}
}

type protocolListings = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint

// listingsEveryClass is the listing of both backends: a pod under every class
// it serves a port of.
func listingsEveryClass(t testing.TB, pods []declaredPod) protocolListings {
	out := protocolListings{}
	for _, p := range pods {
		r := p.row(t)
		for _, proto := range endpointmeta.ProtocolsServed(r.GetPortProtocols()) {
			out[proto] = append(out[proto], r)
		}
	}
	return out
}

// desiredFor is the Service the generator wants for service ns/svc given its
// listings; ok is false when it wants none (no pod serves a port).
func desiredFor(t testing.TB, listings protocolListings) (desiredService, bool) {
	t.Helper()
	g := &Generator{Snapshot: snapshotOf(catalog{key("ns", "svc"): listings}), MeshPort: 18081, Log: slog.New(slog.DiscardHandler)}
	d, ok := g.buildDesiredServices(context.Background())[client.ObjectKey{Namespace: "ns", Name: "svc"}]
	return d, ok
}
