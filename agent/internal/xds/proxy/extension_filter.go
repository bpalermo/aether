package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// allowedExtensionFilters is the set of Envoy HTTP filters the proxy-extension escape
// hatch (proposal 025) may reference: filters the aether proxy build actually compiles
// in. A payload for any other filter would proto-validate yet NACK at runtime, so the
// admission webhook (M2) rejects it and the builder skips it. Each entry yields an
// EMPTY default config of the filter's type — carried on the default-disabled HCM
// entry so Envoy accepts it; the real config rides the route's typed_per_filter_config.
//
// Keep this in lockstep with the proxy build's compiled extensions (proposals 010/011).
// header_to_metadata is already compiled in (subsetHeadersHttpFilter uses it).
var allowedExtensionFilters = map[string]func() proto.Message{
	"envoy.filters.http.header_to_metadata": func() proto.Message { return &header_to_metadatav3.Config{} },
	// header_mutation: add/remove request+response headers, configurable per-route via
	// typed_per_filter_config (HeaderMutationPerRoute) — the canonical escape-hatch demo
	// (its effect is a directly-observable header). Compiled into the proxy build
	// (proxy/bazel/extension_config/extensions_build_config.bzl).
	"envoy.filters.http.header_mutation": func() proto.Message { return &header_mutationv3.HeaderMutation{} },
}

// ExtensionFilterAllowed reports whether name is an allow-listed escape-hatch filter
// (i.e. one the proxy build compiles in). The admission webhook (M2) rejects refs to
// anything else; the builder skips them defensively.
func ExtensionFilterAllowed(name string) bool {
	_, ok := allowedExtensionFilters[name]
	return ok
}

// extensionHTTPFilter builds the HCM entry for an escape-hatch filter: present in the
// chain but DISABLED by default, so it is inert on routes that don't reference it. A
// route's typed_per_filter_config (the ExtensionFilter.Config) re-enables + configures
// it — Envoy `typed_per_filter_config` can only OVERRIDE a filter already in the
// chain, never ADD one, so the filter MUST be here for the per-route config to apply.
// The default config is an empty Any of the filter's type (behaviour-neutral; never
// used since every referencing route overrides it).
func extensionHTTPFilter(name string, defaultConfig *anypb.Any) *http_connection_managerv3.HttpFilter {
	return &http_connection_managerv3.HttpFilter{
		Name:       name,
		Disabled:   true,
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{TypedConfig: defaultConfig},
	}
}

// CollectExtensionFilters returns the default-disabled HCM http_filter entries for the
// union of allow-listed extension filters referenced by the given rules (deduped,
// stable order). Returns nil when none — the common case adds nothing to the chain.
// Non-allow-listed names are skipped (the webhook rejects them at admission).
func CollectExtensionFilters(rules []GammaRoute) []*http_connection_managerv3.HttpFilter {
	var (
		seen map[string]struct{}
		out  []*http_connection_managerv3.HttpFilter
	)
	for _, r := range rules {
		for _, ef := range r.ExtensionFilters {
			builder, ok := allowedExtensionFilters[ef.Name]
			if !ok {
				continue
			}
			if _, dup := seen[ef.Name]; dup {
				continue
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[ef.Name] = struct{}{}
			out = append(out, extensionHTTPFilter(ef.Name, config.TypedConfig(builder())))
		}
	}
	return out
}

// extensionPerFilterConfig builds a route's typed_per_filter_config map from its
// escape-hatch filters: each allow-listed, non-nil filter's opaque Config keyed by its
// Envoy filter name. This is what re-enables + configures the (otherwise disabled) HCM
// filter for this route. Returns nil when there is nothing to attach (no allocation in
// the common no-filter case).
func extensionPerFilterConfig(filters []ExtensionFilter) map[string]*anypb.Any {
	var m map[string]*anypb.Any
	for _, ef := range filters {
		if ef.Config == nil || !ExtensionFilterAllowed(ef.Name) {
			continue
		}
		if m == nil {
			m = make(map[string]*anypb.Any, len(filters))
		}
		m[ef.Name] = ef.Config
	}
	return m
}
