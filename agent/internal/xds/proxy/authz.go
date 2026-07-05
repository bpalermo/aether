package proxy

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
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
