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
	// edgeTelemetryFilterName is the Envoy dynamic-modules HTTP filter type name.
	edgeTelemetryFilterName = "envoy.filters.http.dynamic_modules"
	// edgeTelemetryModuleName is the dynamic module basename: the proxy loads
	// lib<name>.so from ENVOY_DYNAMIC_MODULES_SEARCH_PATH (the image volume).
	edgeTelemetryModuleName = "aether_telemetry"
	// edgeTelemetryFilterEntry is the filter_name the module dispatches on.
	edgeTelemetryFilterEntry = "edge"
)

// edgeTelemetryConfig is the per-pod JSON passed as the dynamic module's
// filter_config. The module records aether_requests_total at stream completion,
// dimensioned by this source identity plus the per-request destination service,
// response code, and cause flag (proposal 007).
type edgeTelemetryConfig struct {
	Reporter      string `json:"reporter"`
	SourceService string `json:"source_service"`
	SourcePod     string `json:"source_pod"`
	MeshDomain    string `json:"mesh_domain"`
	EmitPod       bool   `json:"emit_pod"`
}

// outboundEdgeTelemetryHTTPFilter builds the source-reported edge-telemetry
// dynamic-module HTTP filter for a local pod's outbound HCM. The pod's identity
// (service account = mesh service name, pod name) is constant for this listener,
// so it travels in the per-instance filter_config; the destination is derived
// per request from the routed cluster.
//
// Only attach this when edge telemetry is enabled AND the module .so is mounted
// on the proxy — referencing an absent dynamic module makes Envoy reject the
// listener config.
func outboundEdgeTelemetryHTTPFilter(cniPod *cniv1.CNIPod, meshDomain string) *http_connection_managerv3.HttpFilter {
	// source_pod is omitted from the emitted series by default (emit_pod=false)
	// to bound cardinality; the module still receives it for future use.
	cfg, _ := json.Marshal(edgeTelemetryConfig{
		Reporter:      "source",
		SourceService: cniPod.GetServiceAccount(),
		SourcePod:     cniPod.GetName(),
		MeshDomain:    meshDomain,
		EmitPod:       false,
	})
	return httpFilter(edgeTelemetryFilterName, &dynamic_modules_filterv3.DynamicModuleFilter{
		DynamicModuleConfig: &dynamic_modulesv3.DynamicModuleConfig{Name: edgeTelemetryModuleName},
		FilterName:          edgeTelemetryFilterEntry,
		FilterConfig:        config.TypedConfig(wrapperspb.String(string(cfg))),
	})
}
