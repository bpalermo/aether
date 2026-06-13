// Package proxy provides functions to generate Envoy resource types
// (listeners, clusters, endpoints, routes, filter chains) from pod and
// service registry data. It encapsulates the details of building Envoy
// configurations for transparent traffic interception and service discovery.
package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// appClusterPrefix marks the per-pod cluster that forwards decrypted inbound
	// traffic to the pod's own application on loopback.
	appClusterPrefix = "app_"
	// appLoopbackAddress is the loopback address the application listens on inside
	// the pod's network namespace.
	appLoopbackAddress = "127.0.0.1"
	// defaultAppHealthPath is the readiness path the app cluster health-checks when
	// the pod does not specify one.
	defaultAppHealthPath = "/"
	// healthProbeClusterPrefix marks the per-pod health-probe clusters (active HC
	// of the app, separate from the app_<pod> delivery cluster).
	healthProbeClusterPrefix = "health_"
	// appHealthCheckHost is the Host header sent on the app readiness HTTP health
	// check (HTTP/1.1 requires a Host).
	appHealthCheckHost = "localhost"
)

// AppHealthPathFromPod returns the HTTP path used to health-check the pod's
// application, taken from the endpoint.aether.io/health-path annotation, defaulting
// to "/" when unset.
func AppHealthPathFromPod(cniPod *cniv1.CNIPod) string {
	if p, ok := cniPod.GetAnnotations()[constants.AnnotationEndpointHealthPath]; ok && p != "" {
		return p
	}
	return defaultAppHealthPath
}

// AppClusterName returns the name of the per-pod, per-port application cluster
// that the inbound filter chain for that port forwards decrypted traffic to.
// It is unique per (pod, port) so each SNI-selected chain routes to its own
// loopback port.
func AppClusterName(cniPod *cniv1.CNIPod, port uint16) string {
	return fmt.Sprintf("%s%s_%d", appClusterPrefix, cniPod.GetName(), port)
}

// AppPortsFromPod returns the full set of application ports the pod serves
// (multi-port routing, proposal 005): the endpoint.aether.io/ports annotation
// unioned with the default port (AppPortFromPod), sorted and de-duplicated.
// A pod without the annotation yields just {default port} — today's shape.
func AppPortsFromPod(cniPod *cniv1.CNIPod) []uint16 {
	set := map[uint16]struct{}{AppPortFromPod(cniPod): {}}
	if raw, ok := cniPod.GetAnnotations()[constants.AnnotationEndpointPorts]; ok && raw != "" {
		for _, part := range strings.Split(raw, ",") {
			t := strings.TrimSpace(part)
			if t == "" {
				continue
			}
			if p, err := strconv.ParseUint(t, 10, 16); err == nil {
				set[uint16(p)] = struct{}{}
			}
		}
	}
	ports := make([]uint16, 0, len(set))
	for p := range set {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}

// AppPortFromPod returns the port the pod's application listens on, taken from
// the endpoint.aether.io/port annotation, defaulting to the standard endpoint
// port when the annotation is absent or invalid.
func AppPortFromPod(cniPod *cniv1.CNIPod) uint16 {
	s, ok := cniPod.GetAnnotations()[constants.AnnotationEndpointPort]
	if !ok {
		return constants.DefaultEndpointPort
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return constants.DefaultEndpointPort
	}
	return uint16(port)
}

// NewAppCluster builds a STATIC cluster that forwards decrypted inbound traffic
// to the pod's own application at 127.0.0.1:<port>. The upstream connection is
// bound into the pod's network namespace so the loopback address reaches the
// application container, not the agent. This is the only cleartext hop in the
// mesh: it is intra-pod (Envoy -> app on loopback), never pod-to-pod. The app
// is assumed to speak HTTP/1.1, so no explicit HTTP/2 protocol options are set.
func NewAppCluster(name, netns string, port uint16) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                          name,
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpointv3.LbEndpoint{
						{
							HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
								Endpoint: &endpointv3.Endpoint{
									Address: &corev3.Address{
										Address: &corev3.Address_SocketAddress{
											SocketAddress: &corev3.SocketAddress{
												Protocol: corev3.SocketAddress_TCP,
												Address:  appLoopbackAddress,
												PortSpecifier: &corev3.SocketAddress_PortValue{
													PortValue: uint32(port),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		UpstreamBindConfig: &corev3.BindConfig{
			SourceAddress: &corev3.SocketAddress{
				Address: appLoopbackAddress,
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: 0,
				},
				NetworkNamespaceFilepath: netns,
			},
		},
		// Collapse every per-pod app cluster into one cluster.app.* stats block
		// (cardinality round 2): the Envoy->app loopback hop gets node-aggregate
		// visibility (connect failures, rq totals) at O(1) instead of either
		// O(pods) stats or a blanket exclusion. Nothing reads app cluster stats
		// per-cluster (verified: no ClusterMinHealthyPercentages references).
		AltStatName: "app",
	}
}

// IsPerPodClusterName reports whether the cluster name belongs to a per-pod
// cluster (app_<pod> delivery or health_<pod> probe), as opposed to a
// registry-derived service cluster.
func IsPerPodClusterName(name string) bool {
	return strings.HasPrefix(name, appClusterPrefix) || strings.HasPrefix(name, healthProbeClusterPrefix)
}

// HealthProbeClusterName returns the name of the per-pod health-probe cluster.
func HealthProbeClusterName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("%s%s", healthProbeClusterPrefix, cniPod.GetName())
}

// NewAppHealthProbeCluster builds a per-pod cluster that only exists to actively
// health-check the pod's application at 127.0.0.1:<port> (delegated liveness). It
// is NOT referenced by any route — the agent scrapes its host health from the
// proxy admin and reflects it into the registry. The active HC must live on this
// separate cluster, not on app_<pod>: an active HC removes failing/pending hosts
// from load balancing, which would gate (break) the real delivery path through
// app_<pod> at startup and whenever the probe fails.
func NewAppHealthProbeCluster(name, netns string, port uint16, healthPath string) *clusterv3.Cluster {
	c := NewAppCluster(name, netns, port)
	// MUST stay per-pod (clear the inherited collapse): the health_check
	// filter answers per-pod readiness by reading THIS cluster's
	// membership_healthy/membership_total gauges (see the 2026-06-11 stats
	// outage). A shared alt_stat_name would merge every pod's membership into
	// one gauge and one unhealthy pod would fail every pod's readiness.
	c.AltStatName = ""
	c.HealthChecks = []*corev3.HealthCheck{
		{
			Timeout:            durationpb.New(1 * time.Second),
			Interval:           durationpb.New(5 * time.Second),
			HealthyThreshold:   wrapperspb.UInt32(1),
			UnhealthyThreshold: wrapperspb.UInt32(2),
			// This cluster never carries routed traffic (see above), so without
			// these Envoy applies its no-traffic HC cadence (default 60s) to every
			// check after the immediate first one — measured as a 30-62s
			// endpoint-promotion delay per new pod (e2e 2026-06-11): the first
			// probe races app startup, loses, and the retry waits out the
			// no-traffic interval. The healthy variant likewise delayed *demotion*
			// of a dying app by up to 60s. Probing localhost is cheap; keep the
			// configured cadence regardless of traffic.
			NoTrafficInterval:        durationpb.New(5 * time.Second),
			NoTrafficHealthyInterval: durationpb.New(5 * time.Second),
			HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
				HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
					Host:            appHealthCheckHost,
					Path:            healthPath,
					CodecClientType: typev3.CodecClientType_HTTP1,
				},
			},
		},
	}
	return c
}
