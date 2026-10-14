package proxy

import (
	"net"
	"slices"
	"strconv"
	"strings"

	"aethermesh.dev/agent/internal/xds/config"
	meshconst "aethermesh.dev/common/constants/mesh"
	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	network_inputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// The capture listener selects its chain with filter_chain_matcher, Envoy's
// generic matching tree, and not with each chain's filter_chain_match
// (proposal 043, "The chains of a capture listener").
//
// The reason is backtracking. filter_chain_match resolves the destination port
// first, the address second and the server name third, and never goes back: a
// connection to a port that SOME chain names is matched among those chains
// alone, and one to an address that some chain names among that address's
// chains alone. One HTTP chain beside the per-address raw-TCP and TLSRoute
// chains was therefore unreachable for every port number any service declares
// raw TCP, and for every port of an address that has a TLSRoute: the request
// matched the address, failed the SNI and left through the passthrough. Chart
// 3.0.0 worked around it with one HTTP chain per HTTP port number of the mesh,
// each with its own route table.
//
// The matching tree does go back: a branch that ends without a match hands
// the connection to the on_no_match of the map it came from
// (source/common/matcher/exact_map_matcher.h). So one HTTP chain serves every
// port, and the tree is:
//
//	destination address = a service's address
//	  destination port = one of its raw-TCP ports
//	    server name = a TLSRoute hostname of it  -> that TLSRoute's chain
//	    else                                     -> the port's raw-TCP chain
//	  any other port, unless the service serves HTTP on it
//	    server name = a TLSRoute hostname of it  -> that TLSRoute's chain
//	  (nothing matched: back to the root's on_no_match)
//	else (any address, or back from above)
//	  cleartext, sniffed HTTP
//	    destination port 18081                   -> cap_http_18081
//	    any other port                           -> cap_http
//	  (nothing matched: the default chain, the passthrough)
//
// The port that was dialled is no longer a property of the chain: cap_http
// stores it in filter state (DialledPortFilterStateKey) and its one route
// table branches on it (MergeCaptureRouteTables).
//
// TLS to a port the service itself serves as HTTP never reaches its TLSRoute:
// the chart-3.0.0 listener sent it to the passthrough, and so does this one
// (the owner's ruling for this change). So does TLS to 18081 when the service
// serves HTTP at all, which is where the proxy port carries its HTTP.
const (
	// CaptureHTTPChainName is the capture listener's one HTTP chain for every
	// port but the proxy port, and the name of its route table.
	CaptureHTTPChainName = "cap_http"
	// DialledPortFilterStateKey holds, on cap_http, the destination port of
	// the connection: the port the client dialled. The route table reads it.
	// It is set by the chain, on the connection, and never shared upstream:
	// a client has no way to set it.
	DialledPortFilterStateKey = "aether.dialled_port"

	captureMatchDestinationIP  = "envoy.matching.inputs.destination_ip"
	captureMatchDestinationPrt = "envoy.matching.inputs.destination_port"
	captureMatchServerName     = "envoy.matching.inputs.server_name"
	captureMatchTransport      = "envoy.matching.inputs.transport_protocol"
	captureMatchApplication    = "envoy.matching.inputs.application_protocol"
)

// captureHTTPApplicationProtocols are the application protocols http_inspector
// reports for cleartext HTTP, as ApplicationProtocolInput renders them: each
// quoted (envoy/extensions/matching/network/application_protocol).
var captureHTTPApplicationProtocols = []string{"'http/1.0'", "'http/1.1'", "'h2c'"}

// BuildDialledPortFilterState returns the network filter that stores the
// connection's destination port under DialledPortFilterStateKey. The capture
// listener restores the original destination before any filter runs, so the
// port is the one the client dialled. Not shared with the upstream.
func BuildDialledPortFilterState() *listenerv3.Filter {
	return networkFilter("envoy.filters.network.set_filter_state", &set_filter_state_v3.Config{
		OnNewConnection: []*setFilterStatev3.FilterStateValue{{
			Key:        &setFilterStatev3.FilterStateValue_ObjectKey{ObjectKey: DialledPortFilterStateKey},
			FactoryKey: genericStringFactory,
			Value: &setFilterStatev3.FilterStateValue_FormatString{FormatString: &corev3.SubstitutionFormatString{
				Format: &corev3.SubstitutionFormatString_TextFormatSource{TextFormatSource: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineString{InlineString: "%DOWNSTREAM_LOCAL_PORT%"},
				}},
			}},
		}},
	})
}

// captureServiceChains is what one service contributes to a capture
// listener: its chains, and how the matching tree reaches them.
type captureServiceChains struct {
	// ip is the service's address as DestinationIPInput renders it.
	ip string
	// chains are the service's chains, in listener order.
	chains []*listenerv3.FilterChain
	// tcpPorts maps each raw-TCP port of the service to its chain's name.
	tcpPorts map[uint32]string
	// sni lists, in precedence order, the server names of the service's
	// TLSRoutes with the chain each selects.
	sni []sniChain
	// httpPorts are the ports on which TLS is NOT matched against the
	// service's TLSRoutes: the ports it serves as HTTP, and the proxy port
	// when it serves any.
	httpPorts []uint32
}

// sniChain is one server name of a TLSRoute and its chain.
type sniChain struct {
	name  string
	chain string
}

// buildCaptureServiceChains builds one service's raw-TCP and TLSRoute chains
// and the facts the matching tree needs about them. ok is false for a service
// with no usable address.
func buildCaptureServiceChains(svc CaptureTCPService, sourceSpiffeID string) (captureServiceChains, bool) {
	ip := net.ParseIP(svc.ClusterIP)
	if ip == nil || svc.ClusterName == "" {
		return captureServiceChains{}, false
	}
	out := captureServiceChains{ip: ip.String(), tcpPorts: map[uint32]string{}}

	tls := BuildCaptureTLSRouteFilterChains(svc, svc.TLSRouteRules, sourceSpiffeID)
	out.chains = append(out.chains, tls...)
	out.sni = sniPrecedence(tls)

	// One chain per raw-TCP port other than the legacy target (proposal
	// 037), each to its own cluster.
	for _, p := range svc.TCPPorts {
		if p == svc.LegacyTCPPort || p == meshconst.ProxyL4OutboundPort {
			continue // the legacy chains below own both numbers
		}
		if pc := buildCaptureTCPPortFilterChain(svc, p, sourceSpiffeID); pc != nil {
			out.chains = append(out.chains, pc)
			out.tcpPorts[p] = pc.GetName()
		}
	}
	// A raw-TCP legacy target: its own port and the TCP mesh port (proposal
	// 043, "The legacy spellings during Phases 1 and 2"). A service whose
	// legacy target is HTTP gets neither.
	if svc.LegacyTCPPort != 0 {
		for _, lc := range legacyTCPChains(svc, sourceSpiffeID) {
			out.chains = append(out.chains, lc)
			out.tcpPorts[lc.GetFilterChainMatch().GetDestinationPort().GetValue()] = lc.GetName()
		}
	}

	out.httpPorts = slices.Clone(svc.HTTPPorts)
	if len(svc.HTTPPorts) > 0 {
		// The proxy port carries the cleartext HTTP of every service that
		// serves HTTP: a pod sends "Host: <name>:<port>" to "<name>:18081".
		out.httpPorts = append(out.httpPorts, meshconst.ProxyOutboundPort)
	}
	slices.Sort(out.httpPorts)
	out.httpPorts = slices.Compact(out.httpPorts)
	return out, len(out.chains) > 0
}

// sniPrecedence lists the server names of tls chains in the order
// filter_chain_match resolved them: every exact name first, then the wildcard
// names ("*.example.com"), longest suffix first. A name that two chains claim
// goes to the first.
func sniPrecedence(tls []*listenerv3.FilterChain) []sniChain {
	var exact, wild []sniChain
	seen := map[string]bool{}
	for _, fc := range tls {
		for _, n := range fc.GetFilterChainMatch().GetServerNames() {
			n = strings.ToLower(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if strings.HasPrefix(n, "*.") {
				wild = append(wild, sniChain{name: n, chain: fc.GetName()})
				continue
			}
			exact = append(exact, sniChain{name: n, chain: fc.GetName()})
		}
	}
	slices.SortStableFunc(wild, func(a, b sniChain) int { return len(b.name) - len(a.name) })
	return append(exact, wild...)
}

// captureChainMatcher builds the capture listener's filter_chain_matcher (see
// the top of this file). scoped is capture without redirect-all: no
// passthrough, HTTP chains matched on the port alone, and the TCP mesh port's
// blackhole chain for what no service claims there.
func captureChainMatcher(services []captureServiceChains, scoped bool) *xdsmatcherv3.Matcher {
	httpBranch := captureHTTPBranch(scoped)
	byAddress := map[string]*xdsmatcherv3.Matcher_OnMatch{}
	for _, s := range services {
		if _, dup := byAddress[s.ip]; dup {
			continue // one address, one service: the first keeps it
		}
		if m := captureAddressMatcher(s); m != nil {
			byAddress[s.ip] = &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: m}}
		}
	}
	if len(byAddress) == 0 {
		return httpBranch
	}
	return &xdsmatcherv3.Matcher{
		MatcherType: exactTree(captureMatchDestinationIP, &network_inputsv3.DestinationIPInput{}, byAddress),
		OnNoMatch:   &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: httpBranch}},
	}
}

// captureAddressMatcher is the branch of one service's address: its raw-TCP
// ports, then its TLSRoutes on any port it does not serve HTTP on. It has no
// match of its own for anything else, which sends the connection back to the
// root's on_no_match. nil when the service has nothing to match.
func captureAddressMatcher(s captureServiceChains) *xdsmatcherv3.Matcher {
	var sni *xdsmatcherv3.Matcher
	if len(s.sni) > 0 {
		sni = sniMatcher(s.sni)
	}
	// On a port that is not one of its raw-TCP ports: the TLSRoutes, unless
	// the port is one the service serves HTTP on.
	var otherPorts *xdsmatcherv3.Matcher_OnMatch
	if sni != nil {
		otherPorts = onMatchMatcher(sni)
		if len(s.httpPorts) > 0 {
			otherPorts = onMatchMatcher(&xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{
				Matchers: []*xdsmatcherv3.Matcher_MatcherList_FieldMatcher{{
					Predicate: &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_NotMatcher{
						NotMatcher: portsPredicate(s.httpPorts),
					}},
					OnMatch: otherPorts,
				}},
			}}})
		}
	}
	if len(s.tcpPorts) == 0 {
		if otherPorts == nil {
			return nil
		}
		return otherPorts.GetMatcher()
	}
	byPort := make(map[string]*xdsmatcherv3.Matcher_OnMatch, len(s.tcpPorts))
	for p, chain := range s.tcpPorts {
		leaf := chainAction(chain)
		if sni != nil {
			withSNI := sniMatcher(s.sni)
			withSNI.OnNoMatch = leaf
			leaf = onMatchMatcher(withSNI)
		}
		byPort[strconv.Itoa(int(p))] = leaf
	}
	return &xdsmatcherv3.Matcher{
		MatcherType: exactTree(captureMatchDestinationPrt, &network_inputsv3.DestinationPortInput{}, byPort),
		OnNoMatch:   otherPorts,
	}
}

// sniMatcher selects a TLSRoute chain by server name, in precedence order;
// it has no match for any other name.
func sniMatcher(names []sniChain) *xdsmatcherv3.Matcher {
	input := typedInput(captureMatchServerName, &network_inputsv3.ServerNameInput{})
	list := make([]*xdsmatcherv3.Matcher_MatcherList_FieldMatcher, 0, len(names))
	for _, n := range names {
		sm := &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: n.name}, IgnoreCase: true}
		if suffix, wild := strings.CutPrefix(n.name, "*"); wild {
			sm = &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Suffix{Suffix: suffix}, IgnoreCase: true}
		}
		list = append(list, &xdsmatcherv3.Matcher_MatcherList_FieldMatcher{
			Predicate: singlePredicate(input, sm),
			OnMatch:   chainAction(n.chain),
		})
	}
	return &xdsmatcherv3.Matcher{MatcherType: &xdsmatcherv3.Matcher_MatcherList_{MatcherList: &xdsmatcherv3.Matcher_MatcherList{Matchers: list}}}
}

// captureHTTPBranch is the root's on_no_match: cleartext HTTP to the proxy
// port's chain or to cap_http. Scoped capture matches on the port alone, and
// sends the TCP mesh port to the blackhole.
func captureHTTPBranch(scoped bool) *xdsmatcherv3.Matcher {
	byPort := map[string]*xdsmatcherv3.Matcher_OnMatch{
		strconv.Itoa(meshconst.ProxyOutboundPort): chainAction(CaptureHTTPRouteName(meshconst.ProxyOutboundPort)),
	}
	if scoped {
		byPort[strconv.Itoa(meshconst.ProxyL4OutboundPort)] = chainAction(captureTCPBlackholeChainName)
	}
	ports := &xdsmatcherv3.Matcher{
		MatcherType: exactTree(captureMatchDestinationPrt, &network_inputsv3.DestinationPortInput{}, byPort),
		OnNoMatch:   chainAction(CaptureHTTPChainName),
	}
	if scoped {
		return ports
	}
	byApp := make(map[string]*xdsmatcherv3.Matcher_OnMatch, len(captureHTTPApplicationProtocols))
	for _, a := range captureHTTPApplicationProtocols {
		byApp[a] = onMatchMatcher(ports)
	}
	return &xdsmatcherv3.Matcher{MatcherType: exactTree(captureMatchTransport, &network_inputsv3.TransportProtocolInput{}, map[string]*xdsmatcherv3.Matcher_OnMatch{
		"raw_buffer": onMatchMatcher(&xdsmatcherv3.Matcher{
			MatcherType: exactTree(captureMatchApplication, &network_inputsv3.ApplicationProtocolInput{}, byApp),
		}),
	})}
}

// portsPredicate matches a destination port in ports.
func portsPredicate(ports []uint32) *xdsmatcherv3.Matcher_MatcherList_Predicate {
	input := typedInput(captureMatchDestinationPrt, &network_inputsv3.DestinationPortInput{})
	preds := make([]*xdsmatcherv3.Matcher_MatcherList_Predicate, 0, len(ports))
	for _, p := range ports {
		preds = append(preds, singlePredicate(input, &xdsmatcherv3.StringMatcher{MatchPattern: &xdsmatcherv3.StringMatcher_Exact{Exact: strconv.Itoa(int(p))}}))
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_OrMatcher{
		OrMatcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_PredicateList{Predicate: preds},
	}}
}

func singlePredicate(input *xdscorev3.TypedExtensionConfig, sm *xdsmatcherv3.StringMatcher) *xdsmatcherv3.Matcher_MatcherList_Predicate {
	return &xdsmatcherv3.Matcher_MatcherList_Predicate{MatchType: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
		SinglePredicate: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
			Input:   input,
			Matcher: &xdsmatcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: sm},
		},
	}}
}

func typedInput(name string, msg proto.Message) *xdscorev3.TypedExtensionConfig {
	return &xdscorev3.TypedExtensionConfig{Name: name, TypedConfig: config.TypedConfig(msg)}
}

func exactTree(inputName string, input proto.Message, m map[string]*xdsmatcherv3.Matcher_OnMatch) *xdsmatcherv3.Matcher_MatcherTree_ {
	return &xdsmatcherv3.Matcher_MatcherTree_{MatcherTree: &xdsmatcherv3.Matcher_MatcherTree{
		Input:    typedInput(inputName, input),
		TreeType: &xdsmatcherv3.Matcher_MatcherTree_ExactMatchMap{ExactMatchMap: &xdsmatcherv3.Matcher_MatcherTree_MatchMap{Map: m}},
	}}
}

func onMatchMatcher(m *xdsmatcherv3.Matcher) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Matcher{Matcher: m}}
}

// chainAction selects the filter chain of that name.
func chainAction(name string) *xdsmatcherv3.Matcher_OnMatch {
	return &xdsmatcherv3.Matcher_OnMatch{OnMatch: &xdsmatcherv3.Matcher_OnMatch_Action{Action: &xdscorev3.TypedExtensionConfig{
		Name:        name,
		TypedConfig: config.TypedConfig(wrapperspb.String(name)),
	}}}
}
