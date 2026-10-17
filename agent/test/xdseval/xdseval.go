// Package xdseval evaluates the parts of Envoy configuration the agent's tests
// reason about, the way the pinned proxy does: which filter chain of a
// listener a connection selects (filter_chain_matcher, or the legacy
// filter_chain_match), and what a route table answers one request (virtual
// host selection, the virtual host's matcher, the first matching route).
//
// It is a model, and models only what the agent generates. Anything it does
// not model is reported (Unsupported) rather than guessed at, so a test built
// on it fails instead of passing on a wrong answer.
package xdseval

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	typematcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Unsupported is the error of a construct the model does not cover.
type Unsupported struct{ What string }

func (u Unsupported) Error() string { return "xdseval: not modelled: " + u.What }

// InputFunc returns the value of one matcher input, and false when the input
// has no data (Envoy's absl::monostate).
type InputFunc func(input *xdscorev3.TypedExtensionConfig) (string, bool, error)

// EvalMatcher evaluates a generic matcher the way Envoy's MatchTree does: an
// exact map hands a key it does not hold, AND a child that does not match, to
// its on_no_match; a list tries its field matchers in order and moves on when
// a matched predicate's on_match does not match. It returns the action the
// match ends in, or nil for no match.
func EvalMatcher(m *xdsmatcherv3.Matcher, in InputFunc) (*xdscorev3.TypedExtensionConfig, error) {
	if m == nil {
		return nil, nil
	}
	var (
		a   *xdscorev3.TypedExtensionConfig
		err error
	)
	switch {
	case m.GetMatcherTree() != nil:
		a, err = evalTree(m.GetMatcherTree(), in)
	case m.GetMatcherList() != nil:
		a, err = evalList(m.GetMatcherList(), in)
	default:
		return nil, Unsupported{"a matcher with neither a tree nor a list"}
	}
	if err != nil || a != nil {
		return a, err
	}
	return evalOnMatch(m.GetOnNoMatch(), in)
}

// evalTree is an exact map: the child of the input's value, nil when there is
// none or it does not match.
func evalTree(t *xdsmatcherv3.Matcher_MatcherTree, in InputFunc) (*xdscorev3.TypedExtensionConfig, error) {
	if t.GetExactMatchMap() == nil {
		return nil, Unsupported{"a matcher tree that is not an exact_match_map"}
	}
	v, ok, err := in(t.GetInput())
	if err != nil || !ok {
		return nil, err
	}
	child, found := t.GetExactMatchMap().GetMap()[v]
	if !found {
		return nil, nil
	}
	return evalOnMatch(child, in)
}

// evalList is a field-matcher list: the first matched predicate whose
// on_match matches.
func evalList(l *xdsmatcherv3.Matcher_MatcherList, in InputFunc) (*xdscorev3.TypedExtensionConfig, error) {
	for _, fm := range l.GetMatchers() {
		hit, err := evalPredicate(fm.GetPredicate(), in)
		if err != nil {
			return nil, err
		}
		if !hit {
			continue
		}
		a, err := evalOnMatch(fm.GetOnMatch(), in)
		if err != nil || a != nil {
			return a, err
		}
	}
	return nil, nil
}

func evalOnMatch(om *xdsmatcherv3.Matcher_OnMatch, in InputFunc) (*xdscorev3.TypedExtensionConfig, error) {
	switch {
	case om == nil:
		return nil, nil
	case om.GetKeepMatching():
		return nil, Unsupported{"keep_matching"}
	case om.GetMatcher() != nil:
		return EvalMatcher(om.GetMatcher(), in)
	default:
		return om.GetAction(), nil
	}
}

func evalPredicate(p *xdsmatcherv3.Matcher_MatcherList_Predicate, in InputFunc) (bool, error) {
	switch {
	case p.GetSinglePredicate() != nil:
		sp := p.GetSinglePredicate()
		if sp.GetValueMatch() == nil {
			return false, Unsupported{"a custom_match predicate"}
		}
		v, ok, err := in(sp.GetInput())
		if err != nil || !ok {
			return false, err
		}
		return MatchString(sp.GetValueMatch(), v)
	case p.GetOrMatcher() != nil:
		return evalAll(p.GetOrMatcher().GetPredicate(), in, true)
	case p.GetAndMatcher() != nil:
		return evalAll(p.GetAndMatcher().GetPredicate(), in, false)
	case p.GetNotMatcher() != nil:
		hit, err := evalPredicate(p.GetNotMatcher(), in)
		return !hit, err
	}
	return false, Unsupported{"an empty predicate"}
}

// evalAll is an or (any) or an and (not any) of predicates.
func evalAll(preds []*xdsmatcherv3.Matcher_MatcherList_Predicate, in InputFunc, any bool) (bool, error) {
	for _, q := range preds {
		hit, err := evalPredicate(q, in)
		if err != nil {
			return false, err
		}
		if hit == any {
			return any, nil
		}
	}
	return !any, nil
}

// MatchString evaluates an xds StringMatcher.
func MatchString(sm *xdsmatcherv3.StringMatcher, v string) (bool, error) {
	cmp := func(s string) string { return s }
	if sm.GetIgnoreCase() {
		cmp = strings.ToLower
	}
	switch p := sm.GetMatchPattern().(type) {
	case *xdsmatcherv3.StringMatcher_Exact:
		return cmp(v) == cmp(p.Exact), nil
	case *xdsmatcherv3.StringMatcher_Prefix:
		return strings.HasPrefix(cmp(v), cmp(p.Prefix)), nil
	case *xdsmatcherv3.StringMatcher_Suffix:
		return strings.HasSuffix(cmp(v), cmp(p.Suffix)), nil
	case *xdsmatcherv3.StringMatcher_Contains:
		return strings.Contains(cmp(v), cmp(p.Contains)), nil
	case *xdsmatcherv3.StringMatcher_SafeRegex:
		re, err := fullMatchRegexp(p.SafeRegex.GetRegex())
		if err != nil {
			return false, err
		}
		return re.MatchString(v), nil
	}
	return false, Unsupported{"a string matcher pattern"}
}

// Conn is a connection as the capture listener's selection sees it.
type Conn struct {
	IP   string
	Port uint32
	// SNI is the TLS server name, "" for none.
	SNI string
	// Transport is "raw_buffer" or "tls".
	Transport string
	// App are the application protocols the inspectors reported.
	App []string
}

// SelectChain returns the name of the chain l selects for c: by its
// filter_chain_matcher when it has one, else by its chains'
// filter_chain_match. It returns the default chain's name when nothing
// matches, and "" when there is no default chain (the connection is closed).
// A matcher action naming a chain the listener does not have also closes the
// connection, and returns "".
func SelectChain(l *listenerv3.Listener, c Conn) (string, error) {
	if l.GetFilterChainMatcher() == nil {
		return selectLegacy(l, c), nil
	}
	a, err := EvalMatcher(l.GetFilterChainMatcher(), func(input *xdscorev3.TypedExtensionConfig) (string, bool, error) {
		return connInput(input, c)
	})
	if err != nil {
		return "", err
	}
	if a == nil {
		return l.GetDefaultFilterChain().GetName(), nil
	}
	sv := &wrapperspb.StringValue{}
	if err := a.GetTypedConfig().UnmarshalTo(sv); err != nil {
		return "", fmt.Errorf("filter chain action %q: %w", a.GetName(), err)
	}
	for _, fc := range l.GetFilterChains() {
		if fc.GetName() == sv.GetValue() {
			return sv.GetValue(), nil
		}
	}
	return "", nil
}

func connInput(input *xdscorev3.TypedExtensionConfig, c Conn) (string, bool, error) {
	msg, err := input.GetTypedConfig().UnmarshalNew()
	if err != nil {
		return "", false, err
	}
	switch msg.(type) {
	case *network_inputsv3.DestinationIPInput:
		ip := net.ParseIP(c.IP)
		if ip == nil {
			return "", false, nil
		}
		return ip.String(), true, nil
	case *network_inputsv3.DestinationPortInput:
		return strconv.Itoa(int(c.Port)), true, nil
	case *network_inputsv3.ServerNameInput:
		return c.SNI, c.SNI != "", nil
	case *network_inputsv3.TransportProtocolInput:
		return c.Transport, c.Transport != "", nil
	case *network_inputsv3.ApplicationProtocolInput:
		if len(c.App) == 0 {
			return "", false, nil
		}
		q := make([]string, 0, len(c.App))
		for _, a := range c.App {
			q = append(q, "'"+a+"'")
		}
		return strings.Join(q, ","), true, nil
	}
	return "", false, Unsupported{"listener matcher input " + input.GetTypedConfig().GetTypeUrl()}
}

// selectLegacy is Envoy's filter_chain_match selection
// (FilterChainManagerImpl::findFilterChain), as far as the capture listener
// used it: destination port, then the MOST SPECIFIC destination prefix only,
// then the server name, then the transport protocol, then the application
// protocols. No tier backtracks.
func selectLegacy(l *listenerv3.Listener, c Conn) string {
	tier := legacyByPort(l.GetFilterChains(), c.Port)
	tier = legacyByPrefix(tier, net.ParseIP(c.IP))
	tier = legacyNarrow(tier, func(m *listenerv3.FilterChainMatch) (bool, bool) {
		return c.SNI != "" && slices.Contains(m.GetServerNames(), c.SNI), len(m.GetServerNames()) == 0
	})
	tier = legacyNarrow(tier, func(m *listenerv3.FilterChainMatch) (bool, bool) {
		return m.GetTransportProtocol() == c.Transport, m.GetTransportProtocol() == ""
	})
	tier = legacyNarrow(tier, func(m *listenerv3.FilterChainMatch) (bool, bool) {
		hit := false
		for _, a := range c.App {
			hit = hit || slices.Contains(m.GetApplicationProtocols(), a)
		}
		return hit, len(m.GetApplicationProtocols()) == 0
	})
	if len(tier) > 0 {
		return tier[0].GetName()
	}
	return l.GetDefaultFilterChain().GetName()
}

// legacyByPort: the chains naming the port, else those naming none.
func legacyByPort(chains []*listenerv3.FilterChain, port uint32) []*listenerv3.FilterChain {
	return legacyNarrow(chains, func(m *listenerv3.FilterChainMatch) (bool, bool) {
		return m.GetDestinationPort().GetValue() == port, m.GetDestinationPort() == nil
	})
}

// legacyNarrow keeps the chains whose match is specific to the connection
// (the first result of f), else those that do not constrain the criterion
// (the second).
func legacyNarrow(chains []*listenerv3.FilterChain, f func(*listenerv3.FilterChainMatch) (specific, any bool)) []*listenerv3.FilterChain {
	var specific, anyOf []*listenerv3.FilterChain
	for _, fc := range chains {
		s, a := f(fc.GetFilterChainMatch())
		switch {
		case s:
			specific = append(specific, fc)
		case a:
			anyOf = append(anyOf, fc)
		}
	}
	if len(specific) > 0 {
		return specific
	}
	return anyOf
}

// legacyByPrefix keeps the chains whose longest prefix containing ip is the
// longest of all, or the chains with no prefix when none contains it.
func legacyByPrefix(chains []*listenerv3.FilterChain, ip net.IP) []*listenerv3.FilterChain {
	longest := func(fc *listenerv3.FilterChain) int {
		best := -1
		for _, pr := range fc.GetFilterChainMatch().GetPrefixRanges() {
			_, cidr, err := net.ParseCIDR(fmt.Sprintf("%s/%d", pr.GetAddressPrefix(), pr.GetPrefixLen().GetValue()))
			if err == nil && cidr.Contains(ip) && int(pr.GetPrefixLen().GetValue()) > best {
				best = int(pr.GetPrefixLen().GetValue())
			}
		}
		return best
	}
	best := -1
	for _, fc := range chains {
		best = max(best, longest(fc))
	}
	var out []*listenerv3.FilterChain
	for _, fc := range chains {
		l := longest(fc)
		if (best >= 0 && l == best) || (best < 0 && len(fc.GetFilterChainMatch().GetPrefixRanges()) == 0) {
			out = append(out, fc)
		}
	}
	return out
}

// Request is one HTTP request as a route table sees it.
// fullMatchRegexp compiles pattern as a full match (RE2 anchored at both
// ends, as the proxy matches a safe_regex), once per pattern: the cache's
// equivalence checks and seeded walks ask the same few patterns millions of
// times, and compiling them each time was most of those tests' CPU (and of
// their time under the race detector).
func fullMatchRegexp(pattern string) (*regexp.Regexp, error) {
	if re, ok := compiledRegexps.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, err
	}
	compiledRegexps.Store(pattern, re)
	return re, nil
}

// compiledRegexps memoizes fullMatchRegexp. A *regexp.Regexp is safe for
// concurrent use.
var compiledRegexps sync.Map

type Request struct {
	// Host is the :authority.
	Host string
	// Path is the :path; "/" when empty, unless NoPath.
	Path string
	// NoPath: the request has no path at all (a CONNECT without one). The
	// connection manager answers any other path that does not begin with
	// "/" before routing, so this is the one request with a path no
	// "/"-prefixed route takes that reaches the routes. Only a CONNECT
	// matcher route takes it (FirstRoute).
	NoPath bool
	// Connect: the method is CONNECT (implied by NoPath).
	Connect bool
	// Headers are other request headers, lower-case names.
	Headers map[string]string
	// FilterState is the connection's filter state, as strings. A key that is
	// absent has no data.
	FilterState map[string]string
}

func reqInput(input *xdscorev3.TypedExtensionConfig, r Request) (string, bool, error) {
	msg, err := input.GetTypedConfig().UnmarshalNew()
	if err != nil {
		return "", false, err
	}
	switch in := msg.(type) {
	case *network_inputsv3.FilterStateInput:
		v, ok := r.FilterState[in.GetKey()]
		return v, ok, nil
	case *typematcherv3.HttpRequestHeaderMatchInput:
		name := strings.ToLower(in.GetHeaderName())
		if name == ":authority" || name == "host" {
			return r.Host, true, nil
		}
		v, ok := r.Headers[name]
		return v, ok, nil
	}
	return "", false, Unsupported{"route matcher input " + input.GetTypedConfig().GetTypeUrl()}
}

// MatcherActionNames returns every chain name an action of a listener's
// filter_chain_matcher selects, sorted and de-duplicated. A name the listener
// has no chain of closes the connection (Envoy's NoopFilterChainAction), so
// every one must be on the listener.
func MatcherActionNames(m *xdsmatcherv3.Matcher) []string {
	var names []string
	var walk func(m *xdsmatcherv3.Matcher)
	var onMatch func(om *xdsmatcherv3.Matcher_OnMatch)
	onMatch = func(om *xdsmatcherv3.Matcher_OnMatch) {
		switch {
		case om == nil:
		case om.GetMatcher() != nil:
			walk(om.GetMatcher())
		case om.GetAction() != nil:
			sv := &wrapperspb.StringValue{}
			if err := om.GetAction().GetTypedConfig().UnmarshalTo(sv); err != nil {
				names = append(names, fmt.Sprintf("<undecodable action %q>", om.GetAction().GetName()))
				return
			}
			names = append(names, sv.GetValue())
		}
	}
	walk = func(m *xdsmatcherv3.Matcher) {
		if m == nil {
			return
		}
		for _, fm := range m.GetMatcherList().GetMatchers() {
			onMatch(fm.GetOnMatch())
		}
		for _, om := range m.GetMatcherTree().GetExactMatchMap().GetMap() {
			onMatch(om)
		}
		for _, om := range m.GetMatcherTree().GetPrefixMatchMap().GetMap() {
			onMatch(om)
		}
		onMatch(m.GetOnNoMatch())
	}
	walk(m)
	slices.Sort(names)
	return slices.Compact(names)
}
