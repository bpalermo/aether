// Package telemetry is the instrumentation-only OpenTelemetry API surface
// shared across Aether: attribute keys, span helpers, and gRPC stats handlers.
// Everything here is a no-op until a binary main installs real providers via
// common/telemetry/setup (or the CNI plugin's cni/internal/telemetry).
//
// This package must stay free of the OTel SDK, exporters, and Prometheus so
// that short-lived binaries (the CNI plugin) can instrument without linking
// provider/exporter machinery.
package telemetry

import "go.opentelemetry.io/otel/attribute"

// Shared OTel attribute keys, so the same pod/service identity attributes are
// queryable across spans emitted by the CNI plugin, agent, and registrar.
const (
	// AttrPodName is the Kubernetes pod name.
	AttrPodName = attribute.Key("aether.pod.name")
	// AttrPodNamespace is the Kubernetes namespace of the pod.
	AttrPodNamespace = attribute.Key("aether.pod.namespace")
	// AttrContainerID is the pod sandbox container ID from the CNI invocation.
	AttrContainerID = attribute.Key("aether.container.id")
	// AttrNodeName is the Kubernetes node name.
	AttrNodeName = attribute.Key("aether.node.name")
	// AttrClusterName is the Kubernetes cluster name.
	AttrClusterName = attribute.Key("aether.cluster.name")
	// AttrServiceName is the mesh service name an endpoint belongs to.
	AttrServiceName = attribute.Key("aether.service.name")
	// AttrSnapshotVersion is the xDS or registrar snapshot version.
	AttrSnapshotVersion = attribute.Key("aether.snapshot.version")
)
