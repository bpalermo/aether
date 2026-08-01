package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	"github.com/bpalermo/aether/common/extensionfilter"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/anypb"
)

// ExtensionFilterAllowed reports whether name is an allow-listed escape-hatch filter
// (one the proxy build compiles in). Delegates to the shared allow-list
// (common/extensionfilter) so the agent and the controller webhook agree. The builder
// skips non-allow-listed names defensively; the webhook (M2) rejects them at admission.
func ExtensionFilterAllowed(name string) bool {
	return extensionfilter.Allowed(name)
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
// extra carries filters not referenced by any rule — the service-wide CHAIN-scope
// filters (025 M4), which are enabled at the vhost instead of a route but still need
// their default-disabled chain entry.
func CollectExtensionFilters(rules []GammaRoute, extra ...ExtensionFilter) []*http_connection_managerv3.HttpFilter {
	var (
		seen map[string]struct{}
		out  []*http_connection_managerv3.HttpFilter
	)
	all := rules
	if len(extra) > 0 {
		all = append(append([]GammaRoute{}, rules...), GammaRoute{ExtensionFilters: extra})
	}
	for _, r := range all {
		for _, ef := range r.ExtensionFilters {
			defaultConfig, ok := extensionfilter.DefaultConfig(ef.Name)
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
			out = append(out, extensionHTTPFilter(ef.Name, config.TypedConfig(defaultConfig)))
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

// ApplyServiceChainFilter enables the service-wide ALWAYS-ON extension filter on a
// service's capture vhost via vhost-level typed_per_filter_config (proposal 025 M4
// CHAIN scope). It applies to every route in the vhost — including the default route
// and routes added later — and a per-route ExtensionRef overrides it (Envoy resolves
// route-level typed_per_filter_config over vhost-level; most-specific wins). No-op on
// a nil filter or a non-allow-listed name.
func ApplyServiceChainFilter(vh *routev3.VirtualHost, ef *ExtensionFilter) {
	if vh == nil || ef == nil {
		return
	}
	m := extensionPerFilterConfig([]ExtensionFilter{*ef})
	if m == nil {
		return
	}
	if vh.TypedPerFilterConfig == nil {
		vh.TypedPerFilterConfig = m
		return
	}
	for k, v := range m {
		vh.TypedPerFilterConfig[k] = v
	}
}
