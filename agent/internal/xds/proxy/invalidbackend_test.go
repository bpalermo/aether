package proxy

import (
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInvalidBackend_EveryMatchOfTheRuleRefuses: a rule's backends belong to
// every match of the rule. With one valid and one invalid backendRef, EACH
// match gets the 500 route for the invalid share, followed by its own route to
// the valid backend alone. (Deciding it inside the loop over the matches
// refused on the first match only: the second saw the backends already
// filtered and sent everything to the valid one.)
func TestInvalidBackend_EveryMatchOfTheRuleRefuses(t *testing.T) {
	rule := GammaRoute{
		Matches: []GammaMatch{{Prefix: "/a"}, {Prefix: "/b"}, {Exact: "/c"}},
		Backends: []GammaBackend{
			{Service: "ns/ok", Cluster: "ok.ns.mesh:8080", Weight: 3, Port: 8080},
			{Service: "ns/bad", Cluster: "bad.ns.mesh", Weight: 1, Port: 7777, Invalid: true},
		},
	}
	vh := BuildServiceVirtualHost("parent", "parent.ns.mesh", []string{"parent.ns.mesh"}, []GammaRoute{rule})

	type pair struct{ refused, routed *routev3.Route }
	byMatch := map[string]*pair{}
	var order []string
	for _, r := range vh.GetRoutes() {
		key := r.GetMatch().GetPathSeparatedPrefix() + "|" + r.GetMatch().GetPath()
		if key == "|" {
			continue // the virtual host's fallback
		}
		if byMatch[key] == nil {
			byMatch[key] = &pair{}
			order = append(order, key)
		}
		switch {
		case r.GetDirectResponse() != nil:
			require.Nil(t, byMatch[key].routed, "%s: the 500 stands before the rule's own route", key)
			byMatch[key].refused = r
		default:
			byMatch[key].routed = r
		}
	}
	require.Len(t, order, 3, "three matches")
	for _, key := range order {
		p := byMatch[key]
		if assert.NotNilf(t, p.refused, "%s: no 500 route for the invalid backendRef's share", key) {
			assert.Equal(t, uint32(500), p.refused.GetDirectResponse().GetStatus())
			assert.Equal(t, uint32(250_000), p.refused.GetMatch().GetRuntimeFraction().GetDefaultValue().GetNumerator(), key)
			assert.Equal(t, InvalidBackendBody, p.refused.GetDirectResponse().GetBody().GetInlineString())
		}
		if assert.NotNilf(t, p.routed, "%s: no route to the valid backend", key) {
			assert.Equal(t, "ok.ns.mesh:8080", p.routed.GetRoute().GetCluster(), "%s: the valid backend alone", key)
			assert.Nil(t, p.routed.GetMatch().GetRuntimeFraction(), "%s: the rule's own route takes what the 500 did not", key)
		}
	}

	// The input rule is not written to: its backends are still both there.
	assert.Len(t, rule.Backends, 2)
	assert.True(t, rule.Backends[1].Invalid)
}

// TestInvalidBackend_NoValidBackendRefusesEverything: with no valid
// backendRef the rule answers 500 for every request, whatever the weights
// (weight 0 included), and never falls back to the parent's own cluster. A
// redirect rule has no backend to judge and is left alone.
func TestInvalidBackend_NoValidBackendRefusesEverything(t *testing.T) {
	for _, weight := range []uint32{1, 0} {
		vh := BuildServiceVirtualHost("parent", "parent.ns.mesh", []string{"parent.ns.mesh"}, []GammaRoute{{
			Matches:  []GammaMatch{{Prefix: "/a"}},
			Backends: []GammaBackend{{Service: "ns/bad", Cluster: "bad.ns.mesh", Weight: weight, Port: 7777, Invalid: true}},
		}})
		first := vh.GetRoutes()[0]
		assert.Equal(t, uint32(500), first.GetDirectResponse().GetStatus(), "weight %d", weight)
		assert.Nil(t, first.GetMatch().GetRuntimeFraction(), "weight %d: every request", weight)
		for _, r := range vh.GetRoutes()[:len(vh.GetRoutes())-1] {
			assert.Nil(t, r.GetRoute(), "weight %d: the rule routes nothing", weight)
		}
	}
	vh := BuildServiceVirtualHost("parent", "parent.ns.mesh", []string{"parent.ns.mesh"}, []GammaRoute{{
		Matches:  []GammaMatch{{Prefix: "/r"}},
		Redirect: &GammaRedirect{Hostname: "example.org"},
		Backends: []GammaBackend{{Service: "ns/bad", Cluster: "bad.ns.mesh", Weight: 1, Invalid: true}},
	}})
	assert.NotNil(t, vh.GetRoutes()[0].GetRedirect())
}
