package xdseval

import (
	"fmt"
	"regexp"
	"strings"

	"aethermesh.dev/common/extensionfilter"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Answer is what a route table does with a request: the virtual host it
// selected, and the route list that applies (the virtual host's routes, or
// the RouteList its matcher ended in), with the virtual host's own
// typed_per_filter_config beside it.
type Answer struct {
	Vhost       string
	Routes      []*routev3.Route
	VhostConfig map[string]*anypb.Any
	// NoVhost: no virtual host takes the Host (the proxy answers 404 NR).
	NoVhost bool
	// NoRoutes: the virtual host's matcher ended in no match (404 NR too).
	NoRoutes bool
}

// Table is a route table indexed for virtual-host selection.
type Table struct {
	rc     *routev3.RouteConfiguration
	exact  map[string]*routev3.VirtualHost
	suffix map[string]*routev3.VirtualHost
	prefix map[string]*routev3.VirtualHost
	star   *routev3.VirtualHost
}

// NewTable indexes rc.
func NewTable(rc *routev3.RouteConfiguration) *Table {
	t := &Table{rc: rc, exact: map[string]*routev3.VirtualHost{}, suffix: map[string]*routev3.VirtualHost{}, prefix: map[string]*routev3.VirtualHost{}}
	for _, vh := range rc.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			d = strings.ToLower(d)
			switch {
			case d == "*":
				t.star = vh
			case strings.HasPrefix(d, "*"):
				t.suffix[d[1:]] = vh
			case strings.HasSuffix(d, "*"):
				t.prefix[d[:len(d)-1]] = vh
			default:
				t.exact[d] = vh
			}
		}
	}
	return t
}

// Pick returns the virtual host host selects, nil for none: an exact domain,
// then the longest "*suffix" wildcard, then the longest "prefix*" wildcard,
// then "*", all compared lower-cased; a wildcard needs at least one character
// in place of its "*".
func (t *Table) Pick(host string) *routev3.VirtualHost {
	host = strings.ToLower(host)
	if vh, ok := t.exact[host]; ok {
		return vh
	}
	for n := len(host) - 1; n > 0; n-- {
		if vh, ok := t.suffix[host[len(host)-n:]]; ok {
			return vh
		}
	}
	for n := len(host) - 1; n >= 0; n-- {
		if vh, ok := t.prefix[host[:n]]; ok {
			return vh
		}
	}
	return t.star
}

// Resolve selects the virtual host of rc for r the way the proxy does
// (Table.Pick), and evaluates its matcher.
func Resolve(rc *routev3.RouteConfiguration, r Request) (Answer, error) {
	return NewTable(rc).Resolve(r)
}

// Resolve is the package-level Resolve on an indexed table.
func (t *Table) Resolve(r Request) (Answer, error) {
	vh := t.Pick(r.Host)
	if vh == nil {
		return Answer{NoVhost: true}, nil
	}
	a := Answer{Vhost: vh.GetName(), VhostConfig: vh.GetTypedPerFilterConfig()}
	if vh.GetMatcher() == nil {
		a.Routes = vh.GetRoutes()
		return a, nil
	}
	action, err := EvalMatcher(vh.GetMatcher(), func(input *xdscorev3.TypedExtensionConfig) (string, bool, error) {
		return reqInput(input, r)
	})
	if err != nil {
		return Answer{}, err
	}
	if action == nil {
		a.NoRoutes = true
		return a, nil
	}
	list := &routev3.RouteList{}
	if err := action.GetTypedConfig().UnmarshalTo(list); err != nil {
		return Answer{}, fmt.Errorf("virtual host %s: matcher action %q is not a RouteList: %w", vh.GetName(), action.GetName(), err)
	}
	a.Routes = list.GetRoutes()
	return a, nil
}

// PickVirtualHost returns the virtual host of rc that host selects, nil for
// none (Table.Pick).
func PickVirtualHost(rc *routev3.RouteConfiguration, host string) *routev3.VirtualHost {
	return NewTable(rc).Pick(host)
}

// Effective returns the routes of a with the virtual host's
// typed_per_filter_config in force on each route, as the proxy applies the
// two levels (extensionfilter.CombineLevels). A route on which they cannot be
// combined is reported as an error.
func (a Answer) Effective() ([]*routev3.Route, error) {
	out := make([]*routev3.Route, 0, len(a.Routes))
	for _, r := range a.Routes {
		c := proto.Clone(r).(*routev3.Route)
		for name, cfg := range a.VhostConfig {
			combined, err := extensionfilter.CombineLevels(name, cfg, c.GetTypedPerFilterConfig()[name])
			if err != nil {
				return nil, err
			}
			if c.TypedPerFilterConfig == nil {
				c.TypedPerFilterConfig = map[string]*anypb.Any{}
			}
			c.TypedPerFilterConfig[name] = combined
		}
		out = append(out, c)
	}
	return out, nil
}

// SameAnswer reports whether a and b give a request the same treatment: the
// same effective route list, or both no route at all. why says how they
// differ.
func SameAnswer(a, b Answer) (same bool, why string, err error) {
	if a.NoVhost || a.NoRoutes || b.NoVhost || b.NoRoutes {
		if (a.NoVhost || a.NoRoutes) == (b.NoVhost || b.NoRoutes) {
			return true, "", nil
		}
		return false, fmt.Sprintf("one has no route: %+v vs %+v", a.NoVhost || a.NoRoutes, b.NoVhost || b.NoRoutes), nil
	}
	ra, err := a.Effective()
	if err != nil {
		return false, "", err
	}
	rb, err := b.Effective()
	if err != nil {
		return false, "", err
	}
	if len(ra) != len(rb) {
		return false, fmt.Sprintf("%d routes (vhost %s) vs %d routes (vhost %s)", len(ra), a.Vhost, len(rb), b.Vhost), nil
	}
	for i := range ra {
		if !proto.Equal(ra[i], rb[i]) {
			return false, fmt.Sprintf("route %d differs (vhost %s vs %s):\n  %v\n  %v", i, a.Vhost, b.Vhost, ra[i], rb[i]), nil
		}
	}
	return true, "", nil
}

// FirstRoute returns the first of routes that r matches (path and request
// header matchers), nil for none.
func FirstRoute(routes []*routev3.Route, r Request) (*routev3.Route, error) {
	path := r.Path
	if path == "" && !r.NoPath {
		path = "/"
	}
	for _, rt := range routes {
		ok, err := routeMatches(rt.GetMatch(), path, r)
		if err != nil {
			return nil, err
		}
		if ok {
			return rt, nil
		}
	}
	return nil, nil
}

func routeMatches(m *routev3.RouteMatch, path string, r Request) (bool, error) {
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if ok, err := pathMatches(m, p); err != nil || !ok {
		return false, err
	}
	for _, h := range m.GetHeaders() {
		ok, err := headerMatches(h, r)
		if err != nil || !ok {
			return false, err
		}
	}
	if ok, err := filterStateMatches(m, r); err != nil || !ok {
		return false, err
	}
	if len(m.GetQueryParameters()) > 0 || m.GetGrpc() != nil {
		return false, Unsupported{"a route match on query or gRPC"}
	}
	// A runtime fraction (the invalid backendRef's share) is a draw. The model
	// takes the share whenever it is not empty: both tables compared are given
	// the same draw, which is all a comparison needs.
	if rf := m.GetRuntimeFraction(); rf != nil && rf.GetDefaultValue().GetNumerator() == 0 {
		return false, nil
	}
	return true, nil
}

func pathMatches(m *routev3.RouteMatch, p string) (bool, error) {
	switch ps := m.GetPathSpecifier().(type) {
	case *routev3.RouteMatch_Prefix:
		return strings.HasPrefix(p, ps.Prefix), nil
	case *routev3.RouteMatch_Path:
		return p == ps.Path, nil
	case *routev3.RouteMatch_PathSeparatedPrefix:
		return p == ps.PathSeparatedPrefix || strings.HasPrefix(p, strings.TrimSuffix(ps.PathSeparatedPrefix, "/")+"/"), nil
	case *routev3.RouteMatch_SafeRegex:
		re, err := regexp.Compile("^(?:" + ps.SafeRegex.GetRegex() + ")$")
		if err != nil {
			return false, err
		}
		return re.MatchString(p), nil
	default:
		return false, Unsupported{fmt.Sprintf("path specifier %T", ps)}
	}
}

// filterStateMatches: RouteMatch.filter_state reads the stream's filter
// state, which falls back to the connection's (FilterStateImpl's parent):
// Request.FilterState.
func filterStateMatches(m *routev3.RouteMatch, r Request) (bool, error) {
	for _, fs := range m.GetFilterState() {
		if fs.GetStringMatch() == nil {
			return false, Unsupported{"a filter-state route match other than a string match"}
		}
		v, ok := r.FilterState[fs.GetKey()]
		if !ok {
			return false, nil
		}
		hit, err := typeStringMatch(fs.GetStringMatch(), v)
		if err != nil || !hit {
			return false, err
		}
	}
	return true, nil
}

func headerMatches(h *routev3.HeaderMatcher, r Request) (bool, error) {
	name := strings.ToLower(h.GetName())
	v, present := r.Headers[name]
	if name == ":authority" || name == "host" {
		v, present = r.Host, true
	}
	var hit bool
	switch {
	case h.GetPresentMatch():
		hit = present
	case h.GetStringMatch() != nil:
		if !present {
			hit = false
			break
		}
		ok, err := typeStringMatch(h.GetStringMatch(), v)
		if err != nil {
			return false, err
		}
		hit = ok
	default:
		return false, Unsupported{"a header matcher other than present or string_match"}
	}
	return hit != h.GetInvertMatch(), nil
}

func typeStringMatch(sm *matcherv3.StringMatcher, v string) (bool, error) {
	cmp := func(s string) string { return s }
	if sm.GetIgnoreCase() {
		cmp = strings.ToLower
	}
	switch p := sm.GetMatchPattern().(type) {
	case *matcherv3.StringMatcher_Exact:
		return cmp(v) == cmp(p.Exact), nil
	case *matcherv3.StringMatcher_Prefix:
		return strings.HasPrefix(cmp(v), cmp(p.Prefix)), nil
	case *matcherv3.StringMatcher_Suffix:
		return strings.HasSuffix(cmp(v), cmp(p.Suffix)), nil
	case *matcherv3.StringMatcher_Contains:
		return strings.Contains(cmp(v), cmp(p.Contains)), nil
	case *matcherv3.StringMatcher_SafeRegex:
		re, err := regexp.Compile("^(?:" + p.SafeRegex.GetRegex() + ")$")
		if err != nil {
			return false, err
		}
		return re.MatchString(v), nil
	}
	return false, Unsupported{"a header string pattern"}
}

// Outcome is the end of a request through one route: a direct response
// (status, body, x-aether-error) or a cluster.
type Outcome struct {
	Status uint32
	Body   string
	Reason string
	// Cluster is the static cluster; ClusterHeader the header that names it
	// (the on-demand route); Weighted lists weighted clusters.
	Cluster       string
	ClusterHeader string
	Weighted      []string
	Redirect      bool
}

// OutcomeOf summarizes a route.
func OutcomeOf(rt *routev3.Route) Outcome {
	var o Outcome
	if rt == nil {
		o.Status = 404
		return o
	}
	if dr := rt.GetDirectResponse(); dr != nil {
		o.Status = dr.GetStatus()
		o.Body = dr.GetBody().GetInlineString()
		for _, h := range rt.GetResponseHeadersToAdd() {
			if h.GetHeader().GetKey() == "x-aether-error" {
				o.Reason = h.GetHeader().GetValue()
			}
		}
		return o
	}
	if rt.GetRedirect() != nil {
		o.Redirect = true
		return o
	}
	ra := rt.GetRoute()
	o.Cluster = ra.GetCluster()
	o.ClusterHeader = ra.GetClusterHeader()
	for _, w := range ra.GetWeightedClusters().GetClusters() {
		o.Weighted = append(o.Weighted, w.GetName())
	}
	return o
}
