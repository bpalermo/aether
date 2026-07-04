package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	"github.com/bpalermo/aether/common/extensionfilter"
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
func CollectExtensionFilters(rules []GammaRoute) []*http_connection_managerv3.HttpFilter {
	var (
		seen map[string]struct{}
		out  []*http_connection_managerv3.HttpFilter
	)
	for _, r := range rules {
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
