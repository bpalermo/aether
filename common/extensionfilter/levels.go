package extensionfilter

import (
	"fmt"

	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// A filter's per-route config can sit at two levels of a route table: on the
// virtual host (a service-wide HTTPFilter is enabled there) and on a route (an
// HTTPRoute rule's ExtensionRef). What the proxy does when BOTH are set is the
// filter's own choice, and the allow-listed filters do not agree:
//
//   - header_to_metadata, rbac: the MOST SPECIFIC level is the whole config.
//     The route's replaces the virtual host's (both call
//     resolveMostSpecificPerFilterConfig).
//   - header_mutation: EVERY level applies, MOST specific first: the route's
//     mutations, then the virtual host's. getAllPerFilterConfig returns the
//     levels least specific first and the filter reverses them unless
//     most_specific_header_mutations_wins is set, which aether never sets
//     (source/extensions/filters/http/header_mutation/header_mutation.cc,
//     maybeInitializeRouteConfigs). So with OVERWRITE on one header the
//     virtual host's value is the one left.
//   - ext_authz: the levels are MERGED (FilterConfigPerRoute(less, more),
//     source/extensions/filters/http/ext_authz/ext_authz.cc):
//     context_extensions are the union, the route's entry winning a key;
//     emit_client_span is the route's when set, else the virtual host's;
//     disabled and the rest of check_settings are the route's; a service
//     override (grpc_service, http_service) is the route's ONLY: "Do not
//     inherit overrides from less specific configuration".
//
// header_to_metadata and rbac read resolveMostSpecificPerFilterConfig
// (header_to_metadata_filter.cc, rbac_filter.cc). All at the pinned Envoy
// (189f198), and measured on it for the ones that combine (agent/test/mtlspool). It matters wherever one place has to
// stand for both levels: a short name's virtual host is shared by several
// namespaces, so a service's virtual-host config has nowhere to go but onto
// each of its routes (agent/internal/xds/proxy/shortname.go), and a route that
// names the same filter must then carry what the two levels amount to.
// Keeping only the route's entry there ran a short-name request without the
// service-wide header_mutation or ext_authz context.
//
// EVERY allow-listed filter has an entry (TestEveryAllowedFilterSaysHowItsLevelsCombine):
// a filter added to the allow-list without one does not build.
var levels = map[string]LevelSemantics{
	HeaderToMetadataFilterName: MostSpecificLevelWins,
	RBACFilterName:             MostSpecificLevelWins,
	HeaderMutationFilterName:   EveryLevelApplies,
	ExtAuthzFilterName:         LevelsAreMerged,
}

// HeaderMutationFilterName is the header-mutation filter.
const HeaderMutationFilterName = "envoy.filters.http.header_mutation"

// LevelSemantics says how the proxy combines a filter's virtual-host config
// and its route config when a request matches both.
type LevelSemantics int

const (
	// MostSpecificLevelWins: the route's config is the whole config.
	MostSpecificLevelWins LevelSemantics = iota + 1
	// EveryLevelApplies: the virtual host's config applies, then the route's.
	EveryLevelApplies
	// LevelsAreMerged: one config is made of the two, field by field.
	LevelsAreMerged
)

// LevelSemanticsOf returns how name combines its levels; false for a filter
// that is not allow-listed.
func LevelSemanticsOf(name string) (LevelSemantics, bool) {
	s, ok := levels[name]
	return s, ok
}

// CombineLevels returns the ONE per-route config that has the effect of
// vhost on the virtual host plus route on the route, for an allow-listed
// filter. It writes to neither argument. An error means it cannot be said
// what the two levels amount to (a filter that is not allow-listed, or a
// config that does not decode): the caller must not guess, because dropping
// either level can drop an authorization.
func CombineLevels(name string, vhost, route *anypb.Any) (*anypb.Any, error) {
	if vhost == nil {
		return route, nil
	}
	if route == nil {
		return vhost, nil
	}
	switch levels[name] {
	case MostSpecificLevelWins:
		return route, nil
	case EveryLevelApplies:
		return combineHeaderMutation(vhost, route)
	case LevelsAreMerged:
		return combineExtAuthz(vhost, route)
	default:
		return nil, fmt.Errorf("filter %q is not allow-listed: how its config levels combine is not known", name)
	}
}

// combineHeaderMutation: every list of mutations, the route's entries first
// and the virtual host's after them, the order the filter applies the levels
// in.
func combineHeaderMutation(vhost, route *anypb.Any) (*anypb.Any, error) {
	var vh, rt header_mutationv3.HeaderMutationPerRoute
	if err := vhost.UnmarshalTo(&vh); err != nil {
		return nil, fmt.Errorf("header_mutation, virtual-host level: %w", err)
	}
	if err := route.UnmarshalTo(&rt); err != nil {
		return nil, fmt.Errorf("header_mutation, route level: %w", err)
	}
	out := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{}}
	// proto.Merge appends repeated fields: the route's, then the virtual host's.
	if rt.GetMutations() != nil {
		proto.Merge(out.Mutations, rt.GetMutations())
	}
	if vh.GetMutations() != nil {
		proto.Merge(out.Mutations, vh.GetMutations())
	}
	return packCombined(out)
}

// packCombined packs a combined config DETERMINISTICALLY. The result goes
// into a route table, whose bytes are its version: a map field marshalled in
// Go's random order (ext_authz's context extensions) would give the same
// config different bytes from one build to the next, and every build a new
// table for the proxy to load.
func packCombined(m proto.Message) (*anypb.Any, error) {
	out := &anypb.Any{}
	if err := anypb.MarshalFrom(out, m, proto.MarshalOptions{Deterministic: true}); err != nil {
		return nil, err
	}
	return out, nil
}

// combineExtAuthz mirrors FilterConfigPerRoute(less_specific, more_specific)
// of the ext_authz filter.
func combineExtAuthz(vhost, route *anypb.Any) (*anypb.Any, error) {
	var vh, rt ext_authzv3.ExtAuthzPerRoute
	if err := vhost.UnmarshalTo(&vh); err != nil {
		return nil, fmt.Errorf("ext_authz, virtual-host level: %w", err)
	}
	if err := route.UnmarshalTo(&rt); err != nil {
		return nil, fmt.Errorf("ext_authz, route level: %w", err)
	}
	// disabled is the route's: a route that disables the check disables it,
	// and nothing else of either level is read.
	if rt.GetDisabled() {
		return route, nil
	}
	// The rest of check_settings is the route's (with none, the defaults).
	settings := &ext_authzv3.CheckSettings{}
	if rt.GetCheckSettings() != nil {
		settings = proto.Clone(rt.GetCheckSettings()).(*ext_authzv3.CheckSettings)
	}
	merged := map[string]string{}
	for k, v := range vh.GetCheckSettings().GetContextExtensions() {
		merged[k] = v
	}
	for k, v := range rt.GetCheckSettings().GetContextExtensions() {
		merged[k] = v
	}
	settings.ContextExtensions = nil
	if len(merged) > 0 {
		settings.ContextExtensions = merged
	}
	// emit_client_span: the route's when it sets it, else the virtual
	// host's. By field name, so this holds whichever version of the API is
	// linked. The service override is NOT inherited: the route's or none.
	if less := vh.GetCheckSettings(); less != nil {
		inheritUnset(settings.ProtoReflect(), less.ProtoReflect(), "emit_client_span")
	}
	return packCombined(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: settings}})
}

// inheritUnset copies each named message field from src to dst when dst does
// not set it and src does. A name the message does not have is skipped.
func inheritUnset(dst, src protoreflect.Message, names ...protoreflect.Name) {
	for _, name := range names {
		fd := dst.Descriptor().Fields().ByName(name)
		if fd == nil || dst.Has(fd) || !src.Has(fd) {
			continue
		}
		dst.Set(fd, protoreflect.ValueOfMessage(proto.Clone(src.Get(fd).Message().Interface()).ProtoReflect()))
	}
}
