// Package config provides Envoy configuration helpers for xDS resources.
// It contains utility functions for building common Envoy configuration patterns
// such as ADS config sources, protocol options, and SPIRE cluster configurations.
package config

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Every ConfigSource below must stay a DETERMINISTIC CONSTANT, and any repeated
// field inside one must be built literally rather than by ranging a Go map
// (issue #852). All citations are the pinned Envoy 1.40.0-dev.20260904.13144fb.
//
// Envoy does not key subscription sharing on the resource name. It keys it on
// MessageUtil::hash() of the whole message that carries the config source, so
// two consumers of one resource name share a subscription only while that hash
// agrees. The sites aether reaches:
//
//   - RDS. RouteConfigProviderManager::addDynamicProvider keys provider reuse on
//     hash(Rds) and matches on that hash ALONE — route_config_name appears only
//     in a RELEASE_ASSERT message, never in a lookup
//     (source/common/rds/route_config_provider_manager.h:47-67). Every meshed
//     pod's egress listener emits its own copy of the out_http Rds, and the
//     per-Gateway edge HTTP, HTTPS and QUIC listeners each emit their own copy of
//     one per-Gateway Rds. Those N copies collapse onto ONE provider, one route
//     table and one subscription purely because they hash alike.
//   - SDS. SecretManagerImpl keys secret providers on
//     StrCat(hash(sds_config_source), ".", config_name, warm)
//     (source/common/secret/secret_manager_impl.h:90-91).
//   - ECDS (subset.go's config_discovery) on StrCat(hash(config_source), ".", name),
//     and ODCDS (httpfilter.go) on hash(odcds_config), timeout included.
//
// What the hash is NOT sensitive to, which redraws the hazard from how #852 was
// first written. MessageUtil::hash is DeterministicProtoHash::hash on any normal
// build (source/common/protobuf/utility.cc:166-172) — a reflection walk, not a
// serialisation. Map entries are folded with ADDITION, explicitly so that map
// order cannot matter (source/common/protobuf/deterministic_hash.cc:78-100).
// Repeated NON-map fields are walked in order and ARE hash-significant (same
// file, the repeated branches of reflectionHashField). So:
//
//   - A map-typed field could not split a provider by serialisation order alone.
//     It remains a hazard one layer out, where go-control-plane versions a
//     snapshot resource by hashing its marshalled BYTES — that is #135's
//     mechanism — but both marshals that carry a ConfigSource today (TypedConfig
//     below, and cache MarshalResource) set Deterministic, which canonicalises
//     maps. Keep them unset anyway: the guard is cheap and the next emitter may
//     not be.
//   - The live Envoy-side hazards are a REPEATED field whose order varies
//     (ConfigSource.authorities; api_config_source's grpc_services,
//     initial_metadata, config_validators) and any genuine semantic divergence
//     between two call sites — resource_api_version set on one and not the other,
//     ads versus an equivalent api_config_source.
//
// And one field is provably exempt: addDynamicProvider RELEASES
// config_source.initial_fetch_timeout before hashing and restores it after, so
// two listeners differing only there still share one RDS provider. #839's field
// is the one addition that could never have split anything — but that
// normalisation is RDS-only. Scoped RDS, ECDS and ODCDS hash it in.
//
// The failure mode, if a future divergence does split a provider, has no alarm:
// the config is valid, Envoy does not NACK, nothing is logged, the proxy is
// healthy. What it costs, corrected against the source rather than assumed:
//
//   - NOT doubled RDS traffic, and on aether not even a redundant request.
//     ads:{} resolves to the single process-wide ADS mux, and the chart's
//     ads_config is api_type: DELTA_GRPC, so that mux is the delta one, whose
//     WatchMap refcounts watches per resource name: findAdditions reports a name
//     only when watch_interest_ had no entry for it
//     (source/extensions/config_subscription/grpc/watch_map.cc:314-328), and only
//     those names reach updateSubscriptionInterest
//     (new_grpc_mux_impl.cc:323-335). A second watch on a name already watched
//     costs zero wire bytes. (The SotW mux would instead dedupe
//     resource_names per request — grpc_mux_impl.cc:178-190 — and pay one
//     redundant DiscoveryRequest at addWatch, :298.)
//   - NOT a new stat series, where the two listeners share an HCM stat_prefix.
//     The subscription's scope is http.<hcm stat_prefix>.rds.<route_config_name>.
//     and the process-wide allocator dedupes by stat name
//     (source/common/stats/allocator.cc:299-316), so one series reads DOUBLE:
//     config_reload, update_success and update_attempt all +2 per push, while
//     version and update_time are set() and stay honest. That IS aether's case
//     for out_http and cap_http, whose HCM stat_prefixes are the constants
//     "outbound_http" and "capture_http". The per-Gateway edge listeners take
//     their stat_prefix from the listener name, so a split there would instead
//     produce two genuinely separate trees.
//   - The unambiguous signal is /config_dump: dumpRouteConfigs emits one
//     dynamic_route_configs entry per provider, so the route config appears
//     twice (source/common/rds/route_config_provider_manager.cc:35-54).
//   - The real cost is two parsed route tables per worker thread, a second
//     parse/build on every push, and independent warming — each provider owns its
//     own Init::Target and burns its own initial_fetch_timeout.
//
// //agent/internal/xds/config:config_test holds the contract: determinism_test.go
// pins byte-identity across independent constructions (which catches a
// map-ranged repeated field, the hazard that actually reaches Envoy) and the
// absence of any populated map field.

// XDSConfigSourceADS creates a ConfigSource that uses ADS (Aggregated Discovery Service)
// for dynamic configuration updates.
//
// Constant by construction, and must stay that way — see the determinism
// contract above. Its callers are the cap_http and per-Gateway edge RDS sources,
// every ADS-delivered SDS reference, the ODCDS source and the subset-headers
// ECDS source, all of which are shared across many consumers on one proxy.
func XDSConfigSourceADS() *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
	}
}

// XDSConfigSourceADSWithInitialFetch is XDSConfigSourceADS with an EXPLICIT
// initial_fetch_timeout.
//
// What the field actually does, because the name invites the opposite reading
// (issue #817): it bounds how long the owning listener stays WARMING for the
// first config on this subscription. When it expires Envoy gives up waiting and
// moves the listener to active ANYWAY — with whatever it has, which for a route
// config that never arrived is nothing at all. So it is a fail-OPEN bound, not a
// gate: setting it can never stop a listener going active with an unresolved
// route table, it only decides how long the control plane is given first.
//
// 0 means "wait forever", which is NOT a safe alternative here: an agent that is
// permanently unable to publish would wedge the egress listener in warming for
// the life of the process, turning a degraded route table into a total
// data-plane outage on that node (and the proxy would never report Ready).
//
// Envoy's own default is 15s, so passing 15s is a PIN rather than a behaviour
// change — it makes the value protocol-visible, assertable in
// //test/envoy_validate, and immune to an upstream default drift.
//
// Keep d a CONSTANT even though, for RDS specifically, it is the one field Envoy
// normalises out of the provider-reuse hash (see the determinism contract above
// XDSConfigSourceADS, issue #852): that exemption is RDS-only, and this helper is
// not promised to stay an RDS-only helper.
func XDSConfigSourceADSWithInitialFetch(d time.Duration) *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{},
		InitialFetchTimeout:   durationpb.New(d),
	}
}

// SDSConfigSourceFromCluster creates a ConfigSource that fetches secrets over a
// gRPC SDS stream to a named (static, bootstrap-defined) cluster. The edge
// proxy uses it to point its transport sockets at the SPIRE Agent's native
// Envoy SDS API (served on the Workload API socket), so Envoy fetches its own
// SVID and trust bundle straight from SPIRE — no agent-side SPIRE bridge. The
// node proxy keeps the ADS source (XDSConfigSourceADS) because it multiplexes
// many workload identities the agent delivers as snapshot secrets.
//
// Hashed like the rest (see the determinism contract above XDSConfigSourceADS):
// SecretManagerImpl keys a secret provider on the hash of this message plus the
// secret name, so every edge transport socket naming the same secret shares one
// SDS subscription only while this returns identical bytes for identical
// clusterName. The grpc_services list is a repeated field built literally, not
// from a map, which is what keeps that true. GrpcService's own subtree does
// reach map fields — google_grpc.channel_args.args is the nearest one — and they
// must stay unset.
func SDSConfigSourceFromCluster(clusterName string) *corev3.ConfigSource {
	return &corev3.ConfigSource{
		ResourceApiVersion: corev3.ApiVersion_V3,
		ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
			ApiConfigSource: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices: []*corev3.GrpcService{
					{
						TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
							EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: clusterName},
						},
					},
				},
			},
		},
	}
}

// TypedConfig wraps a protobuf message as a Google Any type.
// This is used to package Envoy extension configurations for transport in xDS messages.
//
// The inner message is marshalled DETERMINISTICALLY. An Any freezes its payload
// bytes at construction: the enclosing resource's later marshal (go-control-plane's
// MarshalResource, which does set Deterministic) re-encodes the Any's `value` field
// verbatim and cannot canonicalise what is inside it. So any map field in an
// extension config — structpb.Struct.fields is the one aether actually ships, in the
// aether_stats TypedStruct on every per-pod HCM — would otherwise serialise in Go's
// randomised map order and give the enclosing Listener/Cluster/Route a different
// hash on every rebuild of identical config. That is incident #135's mechanism
// (issue #772, races report S6) reaching an emitted resource through an Any rather
// than through a repeated field. See agent/internal/xds/cache/ordering.go.
func TypedConfig(config proto.Message) *anypb.Any {
	c := &anypb.Any{}
	if err := anypb.MarshalFrom(c, config, proto.MarshalOptions{Deterministic: true}); err != nil {
		return nil
	}
	return c
}
