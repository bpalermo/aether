package cache

import (
	"testing"

	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
)

// testCallerNamespace is the namespace the table resolvers of these tests
// (a043Resolve, spResolve) take the request to come from: a short name's
// virtual host selects its routes by it. A test that asks as another
// namespace sets it for its own duration with callingFrom.
var testCallerNamespace = a043NS

// callingFrom resolves as a caller in ns until the test ends. "" is a
// connection that carries no namespace at all.
func callingFrom(t *testing.T, ns string) {
	t.Helper()
	prev := testCallerNamespace
	testCallerNamespace = ns
	t.Cleanup(func() { testCallerNamespace = prev })
}

// vhostRoutes returns the routes a request from testCallerNamespace is
// matched against in vh: its route list, or, for a short name's virtual host,
// the arm of that namespace (the no-match arm when it has none).
func vhostRoutes(t *testing.T, vh *routev3.VirtualHost) []*routev3.Route {
	t.Helper()
	if vh.GetMatcher() == nil {
		return vh.GetRoutes()
	}
	return matcherArm(t, vh.GetMatcher(), testCallerNamespace)
}

// asCalled returns vh as a request from testCallerNamespace sees it: itself,
// or, for a short name's virtual host, a virtual host holding the routes of
// that namespace's arm. Tests written against "the virtual host of a name"
// read its routes through this, so that they hold for a short name too.
func asCalled(t *testing.T, vh *routev3.VirtualHost) *routev3.VirtualHost {
	t.Helper()
	if vh == nil || vh.GetMatcher() == nil {
		return vh
	}
	return &routev3.VirtualHost{Name: vh.GetName(), Domains: vh.GetDomains(), Routes: vhostRoutes(t, vh)}
}

// matcherArm returns the route list a short name's matcher selects for a
// caller in ns: the refusal of a connection that carries no namespace ("");
// else the arm of the namespace, or, when it has none, the table's answer for
// a name that is not the mesh's.
func matcherArm(t *testing.T, m *xdsmatcherv3.Matcher, ns string) []*routev3.Route {
	t.Helper()
	on := m.GetOnNoMatch()
	if ns != "" {
		byCaller := shortByCaller(t, m)
		on = byCaller.GetOnNoMatch()
		if arm, ok := byCaller.GetMatcherTree().GetExactMatchMap().GetMap()[ns]; ok {
			on = arm
		}
	}
	require.NotNil(t, on, "a short name's matcher has neither an arm for %q nor a no-match arm: the request would reach nothing", ns)
	var list routev3.RouteList
	require.NoError(t, on.GetAction().GetTypedConfig().UnmarshalTo(&list))
	return list.GetRoutes()
}

// shortByCaller returns the inner matcher of a short name's virtual host: the
// one reached when the connection carries a caller namespace.
func shortByCaller(t *testing.T, m *xdsmatcherv3.Matcher) *xdsmatcherv3.Matcher {
	t.Helper()
	fields := m.GetMatcherList().GetMatchers()
	require.Len(t, fields, 1, "a short name's matcher asks one thing first: is the caller's namespace there")
	inner := fields[0].GetOnMatch().GetMatcher()
	require.NotNil(t, inner)
	return inner
}

// matcherNamespaces returns the namespaces a short name's virtual host has an
// arm for.
func matcherNamespaces(vh *routev3.VirtualHost) []string {
	var out []string
	fields := vh.GetMatcher().GetMatcherList().GetMatchers()
	if len(fields) != 1 {
		return nil
	}
	for ns := range fields[0].GetOnMatch().GetMatcher().GetMatcherTree().GetExactMatchMap().GetMap() {
		out = append(out, ns)
	}
	return out
}
