package xdseval

import (
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/proto"
)

// SameTreatment reports whether two routes treat a request alike: the same
// action (cluster, cluster selection, redirect, direct-response status), the
// same per-filter config, header mutations and every other route field. The
// routes' matches are not compared (they are how the request got there), nor
// a direct response's body (a diagnostic; the x-aether-error header is the
// contract and is compared). why says how they differ.
func SameTreatment(a, b *routev3.Route) (same bool, why string) {
	if a == nil || b == nil {
		if (a == nil) == (b == nil) {
			return true, ""
		}
		return false, fmt.Sprintf("one has no route: %v / %v", a, b)
	}
	sa, sb := stripMatch(a), stripMatch(b)
	if proto.Equal(sa, sb) {
		return true, ""
	}
	return false, fmt.Sprintf("\n  %v\n  %v", sa, sb)
}

func stripMatch(r *routev3.Route) *routev3.Route {
	c := proto.Clone(r).(*routev3.Route)
	c.Match = nil
	if dr := c.GetDirectResponse(); dr != nil {
		dr.Body = nil
	}
	return c
}
