package xdseval

import (
	"fmt"
	"maps"
	"slices"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// SameTreatment reports whether two routes treat a request alike: the same
// action (cluster, cluster selection, redirect, direct-response status), the
// same per-filter config, header mutations and every other route field. The
// routes' matches are not compared (they are how the request got there), nor
// a direct response's body (a diagnostic; the x-aether-error header is the
// contract and is compared). why says how they differ.
func SameTreatment(a, b *routev3.Route) (same bool, why string) {
	return SameTreatmentIn(nil, a, nil, b)
}

// SameTreatmentIn is SameTreatment for routes of virtual hosts with the
// typed_per_filter_config aVhost and bVhost: the filters each route runs and
// the config of each level they read (FilterLevels) must be the same, level
// by level, not merely amount to the same; every other route field as
// SameTreatment.
func SameTreatmentIn(aVhost map[string]*anypb.Any, a *routev3.Route, bVhost map[string]*anypb.Any, b *routev3.Route) (same bool, why string) {
	if a == nil || b == nil {
		if (a == nil) == (b == nil) {
			return true, ""
		}
		return false, fmt.Sprintf("one has no route: %v / %v", a, b)
	}
	la, lb := FilterLevels(aVhost, a), FilterLevels(bVhost, b)
	if !sameLevels(la, lb) {
		return false, fmt.Sprintf("\n  filters %v\n  filters %v", la, lb)
	}
	sa, sb := stripMatch(a), stripMatch(b)
	sa.TypedPerFilterConfig, sb.TypedPerFilterConfig = nil, nil
	if proto.Equal(sa, sb) {
		return true, ""
	}
	return false, fmt.Sprintf("\n  %v\n  %v", sa, sb)
}

// FilterLevels returns, per HTTP filter a route's per-filter config names
// (on the route or its virtual host), the configs the proxy hands that
// filter: [virtual host's, route's], either nil when that level has none; or
// nothing for a filter that does not run. A config at the virtual host and the
// same config on the route are told apart: the levels must be the same, not
// merely amount to the same.
//
//   - a route-level envoy.config.route.v3.FilterConfig with disabled set
//     disables the filter for the route, whatever its virtual host says, and
//     a virtual-host one does when the route says nothing
//     (RouteEntryImplBase::filterDisabled, source/common/router/config_impl.cc);
//   - otherwise each level's config is handed over (getAllPerFilterConfig).
//
// A filter neither level names is taken not to run: every filter aether
// configures per route sits in the chain disabled by default.
func FilterLevels(vhost map[string]*anypb.Any, r *routev3.Route) map[string][]*anypb.Any {
	out := map[string][]*anypb.Any{}
	names := map[string]bool{}
	for n := range vhost {
		names[n] = true
	}
	for n := range r.GetTypedPerFilterConfig() {
		names[n] = true
	}
	for n := range names {
		vh, vhOff := levelConfig(vhost[n])
		rt, rtOff := levelConfig(r.GetTypedPerFilterConfig()[n])
		if rtOff || (vhOff && rt == nil) {
			continue
		}
		if vh != nil || rt != nil {
			out[n] = []*anypb.Any{vh, rt}
		}
	}
	return out
}

// levelConfig reads one level's per-filter entry: its config (nil for none)
// and whether it disables the filter.
func levelConfig(a *anypb.Any) (cfg *anypb.Any, disabled bool) {
	if a == nil {
		return nil, false
	}
	fc := &routev3.FilterConfig{}
	if a.MessageIs(fc) {
		if a.UnmarshalTo(fc) != nil {
			return a, false
		}
		if fc.GetDisabled() {
			return nil, true
		}
		return fc.GetConfig(), false
	}
	return a, false
}

func sameLevels(a, b map[string][]*anypb.Any) bool {
	if !slices.Equal(slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b))) {
		return false
	}
	for n, la := range a {
		if !slices.EqualFunc(la, b[n], func(x, y *anypb.Any) bool { return proto.Equal(x, y) }) {
			return false
		}
	}
	return true
}

func stripMatch(r *routev3.Route) *routev3.Route {
	c := proto.Clone(r).(*routev3.Route)
	c.Match = nil
	if dr := c.GetDirectResponse(); dr != nil {
		dr.Body = nil
	}
	return c
}
