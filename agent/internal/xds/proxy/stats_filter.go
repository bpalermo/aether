package proxy

import (
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	xdstypev3 "github.com/cncf/xds/go/xds/type/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// statsFilterName is the HTTP filter instance name (used as the filter's stat
	// prefix). The factory is resolved by the typed_config's proto type, not this
	// name — Envoy disables extension-lookup-by-name by default — but it matches
	// the registered factory name for clarity.
	statsFilterName = "aether.filters.http.aether_stats"
	// statsConfigTypeURL is the type URL of the native extension's config proto
	// (aether.filters.http.aether_stats.v3.Config), defined and compiled into the
	// custom Envoy in the //proxy workspace (proposal 012). The agent has no Go
	// binding for it, so the config travels as an xds.type.v3.TypedStruct: Envoy
	// resolves the C++ factory by this inner type and converts the Struct into the
	// Config proto.
	statsConfigTypeURL = "type.googleapis.com/aether.filters.http.aether_stats.v3.Config"
)

// statsFilterConfig is the per-pod identity passed to the aether_stats filter.
// The filter records aether.requests_total at stream completion, dimensioned by
// the local pod's identity plus the per-request remote service, response code,
// and response flags (proposal 007). One half of the identity is agent-injected
// (constant for the listener), the other is derived per request by the filter:
// outbound injects source_*, inbound injects destination_service.
type statsFilterConfig struct {
	Reporter           string
	SourceService      string
	SourcePod          string
	DestinationService string
	MeshDomain         string
	EmitPod            bool
}

// outboundStatsFilter builds the source-reported stats HTTP filter for a local
// pod's outbound HCM. The pod's identity (service account = mesh service name,
// pod name) is constant for this listener, so it travels in the per-instance
// config; the destination is derived per request from the routed cluster.
func outboundStatsFilter(cniPod *cniv1.CNIPod, meshDomain string) *http_connection_managerv3.HttpFilter {
	// source_pod is omitted from the emitted series by default (emit_pod=false)
	// to bound cardinality; the filter still receives it for future use.
	return statsFilter(statsFilterConfig{
		Reporter:      "source",
		SourceService: cniPod.GetServiceAccount(),
		SourcePod:     cniPod.GetName(),
		MeshDomain:    meshDomain,
		EmitPod:       false,
	})
}

// inboundStatsFilter builds the destination-reported stats HTTP filter for a
// local pod's inbound HCM. The local pod is the destination, so its service
// travels in the per-instance config; the filter derives the source per request
// by parsing the verified peer SVID's URI SAN (proposal 007 Phase 2). The mesh
// domain is not needed here — the source is path-parsed from the SPIFFE SAN, not
// stripped from a cluster name.
func inboundStatsFilter(cniPod *cniv1.CNIPod) *http_connection_managerv3.HttpFilter {
	return statsFilter(statsFilterConfig{
		Reporter:           "destination",
		DestinationService: cniPod.GetServiceAccount(),
		EmitPod:            false,
	})
}

// statsFilter builds the native aether_stats HTTP filter from the given
// per-instance config, carried as a TypedStruct so the agent needs no Go binding
// for the C++ extension's config proto. The extension must be compiled into the
// proxy (it is, via //:envoy AETHER_EXTENSIONS) — referencing an unregistered
// filter type makes Envoy reject the listener.
func statsFilter(cfg statsFilterConfig) *http_connection_managerv3.HttpFilter {
	// Keys are the proto field names of Config; Envoy converts this Struct into
	// the C++ Config message. structpb.NewStruct only fails on unsupported value
	// types — all values here are string/bool, so the error is unreachable.
	value, _ := structpb.NewStruct(map[string]any{
		"reporter":            cfg.Reporter,
		"source_service":      cfg.SourceService,
		"source_pod":          cfg.SourcePod,
		"destination_service": cfg.DestinationService,
		"mesh_domain":         cfg.MeshDomain,
		"emit_pod":            cfg.EmitPod,
	})
	return httpFilter(statsFilterName, &xdstypev3.TypedStruct{
		TypeUrl: statsConfigTypeURL,
		Value:   value,
	})
}
