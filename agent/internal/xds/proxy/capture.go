package proxy

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	original_dstv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/original_dst/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// CaptureHTTPRouteName is the RDS route configuration the per-pod transparent-capture
// listeners use (proposal 018, Phase 3a). It maps cluster.local authorities (the
// generated mesh-Service names) to the registry-backed service clusters.
const CaptureHTTPRouteName = "cap_http"

const (
	listenerFilterOriginalDstName   = "envoy.filters.listener.original_dst"
	listenerFilterHTTPInspectorName = "envoy.filters.listener.http_inspector"
)

// CaptureListenerName returns the per-pod transparent-capture listener name.
func CaptureListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("capture_%s", cniPod.GetName())
}

// GenerateCaptureListener builds the per-pod transparent-capture listener (proposal
// 018, Phase 3a): bound to capturePort inside the pod netns, it accepts the outbound
// traffic the CNI redirects from a mesh ClusterIP:meshPort, recovers the original
// destination, and routes by cluster.local authority (the request Host) to the
// service's registry-backed EDS cluster — the same SPIRE-mTLS egress path the
// explicit :18081 listener serves. HTTP only for now; the TCP-over-mTLS floor is a
// follow-up. Default off (no listener is generated unless transparent capture is on).
func GenerateCaptureListener(cniPod *cniv1.CNIPod, capturePort uint32, meshDomain string, emitStatsPod bool) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: CaptureListenerName(cniPod),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					// 0.0.0.0: the CNI REDIRECTs the original ClusterIP dst to this
					// port on the pod's loopback/interface; the bind must catch it.
					Address: defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: capturePort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		// original_dst recovers the pre-REDIRECT ClusterIP:meshPort (SO_ORIGINAL_DST);
		// http_inspector lets the AUTO codec detect HTTP/1 vs HTTP/2 on the captured
		// stream. use_original_dst keeps the recovered destination on the connection.
		ListenerFilters:               buildCaptureListenerFilters(),
		UseOriginalDst:                wrapperspb.Bool(true),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		StatPrefix:                    fmt.Sprintf("capture_%s", cniPod.GetName()),
		TrafficDirection:              corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildCaptureHTTPFilterChain(cniPod, meshDomain, emitStatsPod),
		},
	}, nil
}

// buildCaptureListenerFilters: recover the original destination, then sniff HTTP.
func buildCaptureListenerFilters() []*listenerv3.ListenerFilter {
	return []*listenerv3.ListenerFilter{
		listenerFilter(listenerFilterOriginalDstName, &original_dstv3.OriginalDst{}),
		listenerFilter(listenerFilterHTTPInspectorName, &http_inspectorv3.HttpInspector{}),
	}
}

// buildCaptureHTTPFilterChain mirrors the outbound HTTP chain but serves the capture
// route table (cluster.local authorities) over RDS. Same readiness/subset/on-demand/
// stats filters and per-source mTLS egress path.
func buildCaptureHTTPFilterChain(cniPod *cniv1.CNIPod, meshDomain string, emitStatsPod bool) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager("capture_http", ReporterSource, cniPod.GetName(), cniPod.GetNamespace(), nil)

	prefix := []*http_connection_managerv3.HttpFilter{
		readinessHttpFilter(),
		subsetHeadersHttpFilter(),
		onDemandHttpFilter(),
		outboundStatsFilter(cniPod, meshDomain, emitStatsPod),
	}
	hcm.HttpFilters = append(prefix, hcm.HttpFilters...)

	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: CaptureHTTPRouteName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	return &listenerv3.FilterChain{
		Name: fmt.Sprintf("capture_%s", cniPod.GetName()),
		Filters: []*listenerv3.Filter{
			buildNetworkNamespaceFilterState(),
			buildHTTPConnectionManagerFilter(hcm),
		},
	}
}

// BuildCaptureRouteConfiguration builds the transparent-capture route table: the
// given cluster.local authority -> service-cluster virtual hosts (built by the agent
// from the generated mesh Services, scoped to the node dependency set), plus the
// same on-demand catch-all the outbound listener uses. A captured request for a mesh
// authority (<svc>.<meshDomain>) with no scoped vhost — a cold or off-node service —
// then resolves via ODCDS (proposal 004 cold path) instead of a dead 404; anything
// outside the mesh domain still 404s. This makes capture symmetric with outbound:
// without it, a service whose pods all leave this node drops from the dependency set
// and its scoped vhost vanishes, leaving it stuck 404 until something re-reconciles.
func BuildCaptureRouteConfiguration(vhosts []*routev3.VirtualHost, meshDomain string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name:         CaptureHTTPRouteName,
		VirtualHosts: append(vhosts, buildOnDemandCatchAllVirtualHost(meshDomain)),
	}
}
