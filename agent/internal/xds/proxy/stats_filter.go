package proxy

import (
	"encoding/json"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	dynamic_modulesv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/dynamic_modules/v3"
	dynamic_modules_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// statsFilterName is the Envoy HTTP filter instance name (akin to Istio's
	// "istio.stats"); the filter is matched by its dynamic_modules typed_config.
	statsFilterName = "aether.stats"
	// statsModuleName is the dynamic module basename: the proxy loads
	// lib<name>.so from ENVOY_DYNAMIC_MODULES_SEARCH_PATH (the image volume).
	statsModuleName = "aether_stats"
	// statsFilterEntry is the filter_name the module dispatches on.
	statsFilterEntry = "stats"
)

// statsFilterConfig is the per-pod JSON passed as the dynamic module's
// filter_config. The module records aether_requests_total at stream completion,
// dimensioned by the local pod's identity plus the per-request remote service,
// response code, and cause flag (proposal 007). One half of the identity is
// agent-injected (constant for the listener), the other is derived per request
// by the module: outbound injects source_*, inbound injects destination_service.
type statsFilterConfig struct {
	Reporter           string `json:"reporter"`
	SourceService      string `json:"source_service"`
	SourcePod          string `json:"source_pod"`
	DestinationService string `json:"destination_service"`
	MeshDomain         string `json:"mesh_domain"`
	EmitPod            bool   `json:"emit_pod"`
}

// outboundStatsFilter builds the source-reported stats dynamic-module HTTP
// filter for a local pod's outbound HCM. The pod's identity (service account =
// mesh service name, pod name) is constant for this listener, so it travels in
// the per-instance filter_config; the destination is derived per request from
// the routed cluster.
func outboundStatsFilter(cniPod *cniv1.CNIPod, meshDomain string) *http_connection_managerv3.HttpFilter {
	// source_pod is omitted from the emitted series by default (emit_pod=false)
	// to bound cardinality; the module still receives it for future use.
	return statsFilter(statsFilterConfig{
		Reporter:      "source",
		SourceService: cniPod.GetServiceAccount(),
		SourcePod:     cniPod.GetName(),
		MeshDomain:    meshDomain,
		EmitPod:       false,
	})
}

// inboundStatsFilter builds the destination-reported stats dynamic-module HTTP
// filter for a local pod's inbound HCM. The local pod is the destination, so its
// service travels in the per-instance filter_config; the module derives the
// source per request by parsing the verified peer SVID's URI SAN (proposal 007
// Phase 2). The mesh domain is not needed here — the source is path-parsed from
// the SPIFFE SAN, not stripped from a cluster name.
func inboundStatsFilter(cniPod *cniv1.CNIPod) *http_connection_managerv3.HttpFilter {
	return statsFilter(statsFilterConfig{
		Reporter:           "destination",
		DestinationService: cniPod.GetServiceAccount(),
		EmitPod:            false,
	})
}

// statsFilter builds the aether_stats dynamic-module HTTP filter from the given
// per-instance config. The module .so must be mounted on the proxy (image
// volume) — referencing an absent dynamic module makes Envoy reject the
// listener. The chart mounts it unconditionally alongside attaching this filter.
func statsFilter(cfg statsFilterConfig) *http_connection_managerv3.HttpFilter {
	raw, _ := json.Marshal(cfg)
	return httpFilter(statsFilterName, &dynamic_modules_filterv3.DynamicModuleFilter{
		DynamicModuleConfig: &dynamic_modulesv3.DynamicModuleConfig{Name: statsModuleName},
		FilterName:          statsFilterEntry,
		FilterConfig:        config.TypedConfig(wrapperspb.String(string(raw))),
	})
}
