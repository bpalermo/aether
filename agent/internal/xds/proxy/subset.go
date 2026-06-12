package proxy

import (
	"regexp"
	"sort"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

const (
	// SubsetHeadersFilterName is both the outbound HCM filter name and the
	// ECDS resource name carrying its header_to_metadata config. One shared
	// resource serves every pod's outbound listener on the node: when the
	// subset-key vocabulary changes, the agent pushes a new extension config
	// and Envoy swaps it in place — no listener drain, no connection resets.
	SubsetHeadersFilterName = "aether.subset-headers"

	// subsetHeaderIP / subsetHeaderPod pin a request to a single endpoint by
	// IP or pod name. Always mapped (no vocabulary needed); their selectors
	// are NO_FALLBACK — pin or fail, never silently land elsewhere.
	subsetHeaderIP  = "x-aether-ip"
	subsetHeaderPod = "x-aether-pod"
	// subsetHeaderPrefix maps provider-defined subset keys: a pod annotated
	// metadata.endpoint.aether.io/version=v2 is selectable with
	// x-aether-subset-version: v2.
	subsetHeaderPrefix = "x-aether-subset-"

	// headerToMetadataTypeURL is the ECDS type allow-list entry for the
	// header_to_metadata filter config.
	headerToMetadataTypeURL = "type.googleapis.com/envoy.extensions.filters.http.header_to_metadata.v3.Config"
)

// subsetKeyPattern restricts provider-defined subset keys: they become HTTP
// header suffixes and envoy.lb metadata keys, so only lowercase DNS-label
// shaped names are accepted.
var subsetKeyPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// reservedSubsetKeys are the built-in envoy.lb keys that cannot be redefined
// by endpoint metadata annotations: ip/pod have fixed headers, and
// cluster/namespace are registration facts, not routing dimensions.
var reservedSubsetKeys = map[string]struct{}{
	subsetIPKey:           {},
	subsetPodNameKey:      {},
	subsetClusterKey:      {},
	subsetPodNamespaceKey: {},
}

// ValidSubsetKey reports whether a provider-defined metadata key may be used
// as a subset routing dimension (well-formed and not reserved).
func ValidSubsetKey(key string) bool {
	if _, reserved := reservedSubsetKeys[key]; reserved {
		return false
	}
	return subsetKeyPattern.MatchString(key)
}

// SortSubsetKeys sorts and dedupes a subset key set. Selector and rule order
// is part of the CDS/ECDS resource bytes, and the delta-xDS cache decides
// "changed" by hashing those bytes — unsorted map-iteration order would make
// every snapshot hash as a full config change (see the transport-socket-match
// ordering incident).
func SortSubsetKeys(keys map[string]struct{}) []string {
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	return sorted
}

// subsetHeadersHttpFilter returns the outbound HCM's header_to_metadata
// filter, configured via ECDS (one shared resource for the whole node). The
// default config — applied without warming, before the first ECDS push lands
// — carries the always-on ip/pod pinning rules, so listeners never block on
// discovery and pinning works from the first request.
func subsetHeadersHttpFilter() *http_connection_managerv3.HttpFilter {
	return &http_connection_managerv3.HttpFilter{
		Name: SubsetHeadersFilterName,
		ConfigType: &http_connection_managerv3.HttpFilter_ConfigDiscovery{
			ConfigDiscovery: &corev3.ExtensionConfigSource{
				ConfigSource:                     config.XDSConfigSourceADS(),
				DefaultConfig:                    config.TypedConfig(buildSubsetHeadersConfig(nil)),
				ApplyDefaultConfigWithoutWarming: true,
				TypeUrls:                         []string{headerToMetadataTypeURL},
			},
		},
	}
}

// BuildSubsetHeadersExtension wraps the node's subset-header mapping as the
// ECDS TypedExtensionConfig resource. keys is the (sorted, validated) union
// of provider-defined subset keys across the node's dependency set: the
// provider's vocabulary travels to its consumers via the control plane, so
// consumers declare nothing.
func BuildSubsetHeadersExtension(keys []string) *corev3.TypedExtensionConfig {
	return &corev3.TypedExtensionConfig{
		Name:        SubsetHeadersFilterName,
		TypedConfig: config.TypedConfig(buildSubsetHeadersConfig(keys)),
	}
}

// buildSubsetHeadersConfig builds the header_to_metadata config: the fixed
// ip/pod pinning rules plus one x-aether-subset-<key> rule per provider key.
func buildSubsetHeadersConfig(keys []string) *header_to_metadatav3.Config {
	rules := []*header_to_metadatav3.Config_Rule{
		subsetHeaderRule(subsetHeaderIP, subsetIPKey),
		subsetHeaderRule(subsetHeaderPod, subsetPodNameKey),
	}
	for _, key := range keys {
		rules = append(rules, subsetHeaderRule(subsetHeaderPrefix+key, key))
	}
	return &header_to_metadatav3.Config{RequestRules: rules}
}

// subsetHeaderRule maps a request header into envoy.lb dynamic metadata,
// where the subset load balancer reads its match criteria. The header is
// left on the request (visible to the callee, useful in traces).
func subsetHeaderRule(header, metadataKey string) *header_to_metadatav3.Config_Rule {
	return &header_to_metadatav3.Config_Rule{
		Header: header,
		OnHeaderPresent: &header_to_metadatav3.Config_KeyValuePair{
			MetadataNamespace: envoyFilterMetadataSubsetNamespace,
			Key:               metadataKey,
			Type:              header_to_metadatav3.Config_STRING,
		},
	}
}
