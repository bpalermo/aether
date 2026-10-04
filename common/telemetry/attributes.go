// Package telemetry is the instrumentation-only OpenTelemetry API surface
// shared across Aether: attribute keys, span helpers, and gRPC stats handlers.
// Everything here is a no-op until a binary main installs real providers via
// common/telemetry/setup.
//
// This package must stay free of the OTel SDK, exporters, and Prometheus so
// that binaries can instrument without linking provider/exporter machinery.
// The CNI plugin links none of it at all (#1166): it forwards its timings to
// the agent over the CNI gRPC socket instead.
package telemetry

import "go.opentelemetry.io/otel/attribute"

// Shared OTel attribute keys, so the same pod/service identity attributes are
// queryable across spans emitted by the agent and the registrar.
const (
	// AttrPodName is the Kubernetes pod name.
	AttrPodName = attribute.Key("aether.pod.name")
	// AttrPodNamespace is the Kubernetes namespace of the pod.
	AttrPodNamespace = attribute.Key("aether.pod.namespace")
	// AttrContainerID is the pod sandbox container ID from the CNI invocation.
	AttrContainerID = attribute.Key("aether.container.id")
	// AttrSnapshotVersion is the xDS or registrar snapshot version.
	AttrSnapshotVersion = attribute.Key("aether.snapshot.version")
)
