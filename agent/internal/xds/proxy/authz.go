package proxy

import (
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	set_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_metadata/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// AuthzSidecarClusterName is the static bootstrap cluster the chart provisions for
// the node-local authorization sidecar (proposal 027): a Unix-socket pipe to the
// authz container in the aether-proxy pod. Always present/warm when the sidecar is
// enabled — never demand-scoped.
const AuthzSidecarClusterName = "authz_sidecar"

// ExtAuthzFilterName is the Envoy HTTP filter this entry configures.
const ExtAuthzFilterName = "envoy.filters.http.ext_authz"

// AuthzSidecarHTTPFilter builds the ext_authz HCM entry for the node-local authz
// sidecar (proposal 027 system half): FULL transport config — the per-route type
// (ExtAuthzPerRoute) cannot carry it — but `disabled: true`, so it has ZERO effect
// until a route/vhost opts in via typed_per_filter_config (the 025 enablement
// machinery). failureModeAllow=false is fail-closed: requests on opted-in routes
// are DENIED when the sidecar is unreachable.
func AuthzSidecarHTTPFilter(timeout time.Duration, failureModeAllow bool) *http_connection_managerv3.HttpFilter {
	cfg := &ext_authzv3.ExtAuthz{
		// Forward the aether.source dynamic-metadata namespace (the per-pod
		// set_metadata entry, SourceMetadataHTTPFilter) into every CheckRequest:
		// the authz service reads the calling workload's identity at
		// input.attributes.metadata_context.filter_metadata["aether.source"] —
		// orthogonal to the user-owned per-route context_extensions.
		MetadataContextNamespaces: []string{SourceMetadataNamespace},
		Services: &ext_authzv3.ExtAuthz_GrpcService{
			GrpcService: &corev3.GrpcService{
				TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: AuthzSidecarClusterName},
				},
				Timeout: durationpb.New(timeout),
			},
		},
		TransportApiVersion: corev3.ApiVersion_V3,
		FailureModeAllow:    failureModeAllow,
	}
	f := httpFilter(ExtAuthzFilterName, cfg)
	f.Disabled = true
	return f
}

// SourceMetadataNamespace is the dynamic-metadata namespace carrying the calling
// workload's identity on the egress paths (proposal 027 follow-up): per-pod
// system facts, kept separate from the user-owned context_extensions.
const SourceMetadataNamespace = "aether.source"

// setMetadataFilterName is Envoy's set_metadata HTTP filter.
const setMetadataFilterName = "envoy.filters.http.set_metadata"

// SourceMetadataHTTPFilter builds a per-pod set_metadata entry writing the pod's
// identity (SPIFFE ID, namespace, ServiceAccount, pod name) into the aether.source
// namespace. Emitted on the pod's egress (capture + outbound) chains when the chain
// carries ext_authz, so egress policy can discriminate WHICH workload is calling —
// the node proxy is shared, but the listeners (and thus these entries) are per-pod.
func SourceMetadataHTTPFilter(cniPod *cniv1.CNIPod, trustDomain string) *http_connection_managerv3.HttpFilter {
	md, _ := structpb.NewStruct(map[string]any{
		"spiffeId":       SpiffeIDFromPod(cniPod, trustDomain),
		"namespace":      cniPod.GetNamespace(),
		"serviceAccount": cniPod.GetServiceAccount(),
		"pod":            cniPod.GetName(),
	})
	cfg := &set_metadatav3.Config{
		Metadata: []*set_metadatav3.Metadata{{
			MetadataNamespace: SourceMetadataNamespace,
			Value:             md,
		}},
	}
	return httpFilter(setMetadataFilterName, cfg)
}

// HasExtAuthz reports whether the extension union carries the ext_authz system
// entry — the signal that egress chains need the source-metadata entry too.
func HasExtAuthz(filters []*http_connection_managerv3.HttpFilter) bool {
	for _, f := range filters {
		if f.GetName() == ExtAuthzFilterName {
			return true
		}
	}
	return false
}

// WithoutSourceMetadata returns the union minus any aether.source set_metadata
// entries (the inbound chains take this form).
func WithoutSourceMetadata(filters []*http_connection_managerv3.HttpFilter) []*http_connection_managerv3.HttpFilter {
	var out []*http_connection_managerv3.HttpFilter
	for _, f := range filters {
		if f.GetName() == setMetadataFilterName {
			continue
		}
		out = append(out, f)
	}
	return out
}
