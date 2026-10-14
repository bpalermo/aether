package cache

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/serviceref"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// This file builds what proposal 043 calls "the addressing rule" on the
// captured path: one route table per HTTP port number of the mesh, selected by
// the port the client DIALLED, each holding one virtual host per service that
// serves HTTP on that port.
//
//	Request                                    Table of the dialled port D
//	Host <name>:P, P = D, D is HTTP            the service's port D
//	Host <name>:P, P != D                      421 port-mismatch
//	Host <name>, D is HTTP                     the service's port D
//	D is not an HTTP port of the service       421 port-not-declared / port-not-http
//	a name that is no mesh service             passthrough
//
// <name> is any of the service's names: the mesh name, the cluster.local
// forms, and the bare name when one namespace owns it. The rule is the same
// for all of them, which is the fix for #1682: no route in a table for port D
// leads to a cluster of another port.
//
// No port has another meaning. A dial to port 80 is port 80: served when the
// service declares it, refused otherwise. The table of the mesh's HTTP proxy
// port (18081) is not one of these: 18081 is not a port of any service, so
// nothing dialled there decides, and that chain is served the LOOPBACK table,
// where the port in Host is the port (snapshot.go,
// BuildProxyPortCaptureRouteConfiguration).

// ruleInputs is what a virtual host's routes are built from besides the
// service's own cluster: the GAMMA rules, the service-wide chain filters and
// the port maps of the Services the rules name. Read once per build, outside
// the cluster lock; every map is shared and read-only.
type ruleInputs struct {
	// declared is what the generated Services say their ports are, and ports
	// the effective ports of every service (effectivePorts), set by a builder
	// that holds the cluster lock. portsMap reads them.
	declared     map[string]servicePorts
	ports        map[string]servicePorts
	routes       map[string][]proxy.GammaRoute
	chainFilters map[string]proxy.ExtensionFilter
	portMaps     map[string]map[uint32]uint32
}

// servicePortMapsSnapshot returns the port maps of the Services routes name
// (SetServicePortMaps). The map is replaced wholesale, never mutated in place,
// so the reference is safe to read outside depMu.
func (c *SnapshotCache) servicePortMapsSnapshot() map[string]map[uint32]uint32 {
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	return c.servicePortMaps
}

// bumpRuleGenLocked advances the generation of the RULE INPUTS (the GAMMA
// rules, local and imported, the service-wide chain filters and the Service
// port maps) when one of them changed. Caller must hold depMu.
//
// The loopback table's virtual hosts are stored on the cluster entries, which
// a registry refresh builds. They carry the rule inputs as well, so an event
// that changes only those (a route, a filter, a Service's ports) must reach
// them without waiting for the next refresh: a snapshot built in between had
// a capture table with the new rule and a loopback table without it, and for
// a service-wide filter that is a request delivered with the filter skipped.
// The snapshot build compares this generation with the one the entries'
// virtual hosts were built at (refreshEntryVhostsLocked).
func (c *SnapshotCache) bumpRuleGenLocked(changed bool) {
	if changed {
		c.ruleGen++
	}
}

// SetServicePortMaps replaces the port maps of the Services that routes name
// as a parent or as a backend and that the registrar did not generate: Service
// port -> the pod port its targetPort names, 0 for a named targetPort
// (proposal 043, "A Service is a port map"). Fed by the GAMMA reconciler.
// Signals a snapshot rebuild on change.
func (c *SnapshotCache) SetServicePortMaps(portMaps map[string]map[uint32]uint32) {
	c.depMu.Lock()
	changed := !maps.EqualFunc(c.servicePortMaps, portMaps, func(a, b map[uint32]uint32) bool { return maps.Equal(a, b) })
	c.servicePortMaps = portMaps
	c.bumpRuleGenLocked(changed)
	c.bumpDepGenLocked()
	c.depMu.Unlock()
	if changed {
		c.signalDependencyChange()
	}
}

// podPort resolves a SERVICE port of svc to the pod port behind it: through
// the Service's port map when the Service is a user's, and to itself for a
// Service the registrar generated, which exposes its pods' ports unchanged.
// ok is false when the Service has a port map that does not resolve the port
// (no such Service port, or a named targetPort).
func podPort(portMaps map[string]map[uint32]uint32, svc string, servicePort uint32) (uint32, bool) {
	m, mapped := portMaps[svc]
	if !mapped {
		return servicePort, servicePort != 0
	}
	pod := m[servicePort]
	return pod, pod != 0
}

// rulePlace is where a virtual host that carries rules stands: the service it
// belongs to, the SERVICE port it answers for (0 on the loopback table, which
// is keyed by pod ports, and for a legacy virtual host) and the pod port its
// requests reach (0 when that is not known: a name this node holds no
// endpoint of, or a Service port whose targetPort is a name).
type rulePlace struct {
	svc         string
	servicePort uint32
	pod         uint32
}

// ruleApplies reports whether a rule of a service applies at a place (proposal
// 043, "The model", 6): a rule whose parentRef names no port applies on every
// port of the Service, one that names a port on that port only.
//
// The parentRef's port is a REAL port of the SERVICE, and nothing else:
//
//   - the Service port the table answers for (a port whose targetPort is a
//     name that has not resolved has no virtual host that routes, so a rule
//     for it is attached to nothing until it does);
//   - a user Service's port, through its port map;
//   - a generated Service's port, which is the pod's.
//
// A parentRef that names any other number attaches its rule to NOTHING. That
// includes the mesh HTTP port (18081) and a port 80 the pods do not declare:
// neither is a port of the service, and until chart 3.0.0 both were read as
// "the port the pods declare". The GAMMA reconciler reports such a parentRef
// in the route's status (Accepted=False, NoMatchingParent).
//
// A rule IMPORTED from a peer cluster (proposal 026) carries no port at all:
// the projection has no field for one yet. It was authored for some port of
// the service and nothing here can tell which, so it stays where it applied
// before the tables were per port, on the legacy target's virtual hosts (the
// port the pods declare in endpoint.aether.io/port), and is not spread over
// the service's other ports.
//
// The legacy target is known only while the node holds an endpoint. A service
// known by its generated Service's ports alone has none, and the rule was then
// placed NOWHERE: a cold request skipped the imported rule and its
// ExtensionRef filter, and a warm one ran them. So while the legacy target is
// not known the rule applies on every HTTP port the service is known to have.
// For a service with one HTTP port that is exactly where it applies warm. For
// one with several, a cold request to another port than the declared one runs
// the imported rule where a warm one does not: the rule and its filter are
// never skipped, at the price of that difference, until the projection
// carries the parent port (TestImportedRule_ColdAndWarmAlike).
func ruleApplies(rule proxy.GammaRoute, portMaps map[string]map[uint32]uint32, ports servicePorts, at rulePlace) bool {
	if rule.Imported {
		// legacyPort is 0 exactly when the legacy target is not known; a
		// known one that is not HTTP places the rule on no HTTP port.
		return at.pod == 0 || ports.legacyPort == 0 || at.pod == ports.legacyHTTPPort()
	}
	if rule.ParentPort == 0 || (at.servicePort != 0 && rule.ParentPort == at.servicePort) {
		return true
	}
	if at.pod == 0 {
		// A legacy virtual host of a name with no known port: every local
		// rule, as before the tables were per port.
		return at.servicePort == 0
	}
	if _, mapped := portMaps[at.svc]; mapped {
		resolved, ok := podPort(portMaps, at.svc, rule.ParentPort)
		return ok && resolved == at.pod
	}
	return rule.ParentPort == at.pod
}

// httpClusterForPort returns the cluster that serves pod port port of svc as
// HTTP: the default cluster "<fqdn>" for the service's legacy target, which is
// one and the same port, and "<fqdn>:<port>" for any other.
func httpClusterForPort(svc, meshDomain string, ports servicePorts, port uint32) string {
	if port != 0 && port == ports.legacyHTTPPort() {
		return proxy.ServiceClusterName(svc, meshDomain)
	}
	return proxy.PortClusterName(svc, meshDomain, port)
}

// resolveRules returns the rules of a service for the virtual host at a place
// (ruleApplies), with every backendRef port resolved to the cluster of that
// port.
//
// A backendRef names a REAL port of the backend SERVICE (proposal 043, "The
// model", 6), and it is held to that:
//
//   - the port resolves to an HTTP port of the backend: that port's cluster;
//   - the rule carries no port (imported from a peer, proposal 026), or the
//     backend is not known on this node (no endpoint held yet) and nothing
//     says the port is wrong: the cluster the projector named, as before;
//   - anything else names no HTTP port of the backend: a port the Service
//     does not have (the mesh HTTP port 18081 and an undeclared 80 are two
//     such numbers; until chart 3.0.0 they meant "the port the pods
//     declare"), one that is not HTTP, or a user Service's port whose
//     targetPort is a NAME that has not resolved to a pod port (it was
//     delivered to the port the pods declare; refused until it resolves,
//     owner's ruling of 2026-10-13). The backend is INVALID: its share
//     of the rule is answered 500, as the Gateway API specifies, never
//     delivered to another port. The reconciler reports it in the route's
//     status (ResolvedRefs=False).
//
// The input is shared and read-only; a rule whose backends change is copied.
func resolveRules(in ruleInputs, all map[string]servicePorts, meshDomain string, at rulePlace) []proxy.GammaRoute {
	rules := in.routes[at.svc]
	if len(rules) == 0 {
		return nil
	}
	out := make([]proxy.GammaRoute, 0, len(rules))
	for _, rule := range rules {
		if !ruleApplies(rule, in.portMaps, all[at.svc], at) {
			continue
		}
		rule.Backends = resolveBackends(rule.Backends, in.portMaps, all, meshDomain)
		out = append(out, rule)
	}
	return out
}

func resolveBackends(backends []proxy.GammaBackend, portMaps map[string]map[uint32]uint32, all map[string]servicePorts, meshDomain string) []proxy.GammaBackend {
	var out []proxy.GammaBackend
	for i, b := range backends {
		cluster, invalid := backendCluster(b, portMaps, all, meshDomain)
		if cluster == b.Cluster && invalid == b.Invalid {
			continue
		}
		if out == nil {
			out = slices.Clone(backends)
		}
		out[i].Cluster, out[i].Invalid = cluster, invalid
	}
	if out == nil {
		return backends
	}
	return out
}

// backendCluster resolves one backendRef (resolveRules): the cluster of the
// port it names, and whether it is invalid.
func backendCluster(b proxy.GammaBackend, portMaps map[string]map[uint32]uint32, all map[string]servicePorts, meshDomain string) (cluster string, invalid bool) {
	if b.Port == 0 {
		return b.Cluster, false
	}
	ports := all[b.Service]
	pod := b.Port
	if m, mapped := portMaps[b.Service]; mapped {
		target, has := m[b.Port]
		switch {
		case !has:
			// The Service has no such port, whatever its pods declare.
			return b.Cluster, true
		case target == 0:
			// A targetPort that is a name the Service's EndpointSlices do
			// not resolve (yet): nothing says which port it is. Invalid, as
			// any reference that names no HTTP port of the backend; it was
			// delivered to the port the backend's pods declare.
			return b.Cluster, true
		}
		pod = target
	}
	switch {
	case !ports.known():
		// A backend this node holds no endpoint of yet. Its port cannot be
		// checked, but it is still the port the route names: the cluster is
		// that port's, fetched on demand, never the cluster the bare name
		// spells (the one of the port the backend's pods declare, which
		// answered a route to 9090 from 8080 while the backend was unknown).
		return proxy.PortClusterName(b.Service, meshDomain, pod), false
	case ports.hasHTTP(pod):
		return httpClusterForPort(b.Service, meshDomain, ports, pod), false
	default:
		return b.Cluster, true
	}
}

// portsMap returns the ports every virtual host built from in is judged by:
// the effective ports when the builder set them, else what the registry says.
// Caller must hold clusterMu.
func (c *SnapshotCache) portsMap(in ruleInputs) map[string]servicePorts {
	if in.ports != nil {
		return in.ports
	}
	return c.servicePorts
}

// outboundPortVhostLocked builds the out_http (loopback) virtual host of one
// of a service's HTTP cluster entries: the default entry (isDefault; the
// service's legacy target, claimed port-less and with its port) or a per-port
// one (its "<fqdn>:<port>" authority). Either carries the GAMMA rules that
// apply to its port, with their backends resolved, and the service's chain
// filter. Caller must hold clusterMu (it reads c.servicePorts).
//
// On the loopback listener nothing is dialled, so the port in Host is the port
// (proposal 043, decision 5). Before this change a per-port virtual host
// carried no rules at all: a route with a port-less parentRef applied to the
// port-less authority and to none of the service's ports.
//
// A per-port entry whose port the service does NOT serve as HTTP gets no
// virtual host: when pods disagree about a port's class the minority that
// declares it HTTP still has its cluster (both endpoint sets exist), and the
// service's one answer for the port is the other class. The authority is then
// claimed by the refusal (outboundRefusalVhostsLocked), as on the captured
// path, and not routed to the minority.
func (c *SnapshotCache) outboundPortVhostLocked(svc, entryName string, port uint32, isDefault bool, in ruleInputs) *routev3.VirtualHost {
	ports := c.portsMap(in)[svc]
	// (A retained entry whose port the service no longer declares as HTTP is
	// not retained at all: retentionEvidence.)
	if !isDefault && ports.known() && !ports.hasHTTP(port) {
		return nil
	}
	rules := resolveRules(in, c.portsMap(in), c.meshDomain, rulePlace{svc: svc, pod: port})
	fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
	name, doms := entryName, []string{entryName}
	if isDefault {
		// The default entry is the cluster of the port the pods declare. It
		// is named by the service alone, and answers for that PORT: the
		// port-less authority is not its spelling (it is port 80's, below).
		name, doms = fqdn, []string{fmt.Sprintf("%s:%d", fqdn, port)}
		if port == 0 {
			// Rows that carry no port at all (nothing since proposal 005
			// writes one): the service has one cluster and no port to spell.
			doms = []string{fqdn}
		}
	}
	// A name a USER's Service owns is addressed by that Service's ports: the
	// port in an authority is the Service's port, here as on a captured dial
	// (which is answered by the Service port that was dialled). So this
	// virtual host claims "<fqdn>:<service port>" for every Service port
	// whose target is this pod port, and NOT the pod port's own number unless
	// the Service has a port of that number. With no Service port in front of
	// it the pod port has no authority at all, and the refusals answer for
	// its number. (It used to claim the pod port: "<fqdn>:8080" was delivered
	// behind a Service that has only port 8000, and "<fqdn>:8000" refused.)
	if servicePorts, mapped := servicePortsOf(in.portMaps, svc, port); mapped && port != 0 {
		doms = doms[:0]
		for _, sp := range servicePorts {
			doms = append(doms, fmt.Sprintf("%s:%d", fqdn, sp))
		}
	}
	// A port-less authority is port 80, and nothing else: it belongs to the
	// virtual host of the pod port behind the Service's port 80, when that is
	// an HTTP port of the service.
	if pod, ok := portlessPodPort(in.portMaps, svc, ports); ok && pod == port && port != 0 {
		doms = append(doms, fqdn)
	}
	// With no authority (a pod port no Service port leads to) the virtual
	// host is still built, with no domain, and left out of the table where
	// the tables are assembled: WHICH entries have one is the registry
	// refresh's decision and part of nothing a rule event may change
	// (refreshEntryVhosts, the #1115 reuse), and a port map is a rule input.
	vh := proxy.BuildOutboundServiceVirtualHost(name, doms, rules)
	if ef, ok := in.chainFilters[svc]; ok {
		proxy.ApplyServiceChainFilter(vh, &ef)
	}
	return vh
}

// servicePortsOf returns, sorted, the ports of the user Service that owns svc
// whose target is the pod port pod. mapped is false when no user Service owns
// the name (a generated Service: its ports are the pods' ports).
func servicePortsOf(portMaps map[string]map[uint32]uint32, svc string, pod uint32) (ports []uint32, mapped bool) {
	m, mapped := portMaps[svc]
	if !mapped {
		return nil, false
	}
	for sp, target := range m {
		if target == pod {
			ports = append(ports, sp)
		}
	}
	slices.Sort(ports)
	return ports, true
}

// portlessPodPort returns the pod port a PORT-LESS authority of svc reaches:
// the one behind the Service's port 80 (through a user Service's port map, or
// 80 itself for a generated Service), when the service serves it as HTTP. ok
// is false when the service has no HTTP port 80: a port-less authority is
// then refused, never read as another port. (Until chart 3.0.0 it meant "the
// port the pods declare".)
func portlessPodPort(portMaps map[string]map[uint32]uint32, svc string, ports servicePorts) (uint32, bool) {
	pod, ok := podPort(portMaps, svc, 80)
	return pod, ok && ports.hasHTTP(pod)
}

// refusalBody is the body of every 421 the mesh answers for svc: what was
// wrong, and the ports the service can be addressed on (portsAsAddressed).
func refusalBody(fqdn, what string, ports servicePorts) string {
	return fmt.Sprintf("aether: %s %s. Its ports: %s. Address it as %s:<port>.\n", fqdn, what, ports.describe(), fqdn)
}

// mappedRefusalBody is refusalBody for a name that may be a user Service's:
// it lists the ports an authority can name (portsAsAddressed), and says which
// ports of the Service cannot be named YET because their targetPort is a name
// the Service's EndpointSlices do not resolve. Every refusal of such a name
// says it, on every table: a caller refused on the proxy port for "<name>:8000"
// was told "Its ports: none" and nothing of why.
func mappedRefusalBody(fqdn, what string, portMap map[uint32]uint32, mapped bool, ports servicePorts) string {
	return refusalBody(fqdn, what, portsAsAddressed(portMap, mapped, ports)) + unresolvedNote(portMap)
}

// unresolvedNote is the sentence a refusal's body ends with when the name's
// Service has ports whose targetPort is a name that does not resolve yet, and
// "" when it has none.
func unresolvedNote(portMap map[uint32]uint32) string {
	var unresolved []string
	for sp, pod := range portMap {
		if pod == 0 {
			unresolved = append(unresolved, strconv.Itoa(int(sp)))
		}
	}
	if len(unresolved) == 0 {
		return ""
	}
	slices.SortFunc(unresolved, func(a, b string) int { return cmp.Compare(len(a), len(b))*2 + strings.Compare(a, b) })
	return "Its Service's port " + strings.Join(unresolved, ", ") + " has a targetPort that is a name the Service's EndpointSlices do not resolve yet: it is refused until they do.\n"
}

// portsAsAddressed returns the ports a refusal's body lists: the ports an
// authority can NAME, which are the ones the refusal was judged by. For a
// name a Service of the user's owns those are the SERVICE's ports, each with
// the class of the pod port behind it; a Service port whose pod port the pods
// do not declare is not one. The body used to list the pods' ports there:
// "<name>:8080 declares no such port. Its ports: 8080 (http)", for a Service
// that maps 8000 to the pods' 8080.
func portsAsAddressed(portMap map[uint32]uint32, mapped bool, ports servicePorts) servicePorts {
	if !mapped {
		return ports
	}
	var out servicePorts
	for sp, pod := range portMap {
		switch {
		case ports.hasHTTP(pod):
			out.http = append(out.http, sp)
		case slices.Contains(ports.tcp, pod):
			out.tcp = append(out.tcp, sp)
		case slices.Contains(ports.udp, pod):
			out.udp = append(out.udp, sp)
		}
	}
	slices.Sort(out.http)
	slices.Sort(out.tcp)
	slices.Sort(out.udp)
	return out
}

// outboundRefusalVhostsLocked returns the out_http (loopback) virtual hosts
// that REFUSE: for every service this node knows the ports of, the authorities
// that name the service and no HTTP port of it (proposal 043, "The rule, per
// entry path", column (b)). Caller must hold clusterMu.
//
//   - "<fqdn>:<p>", p declared raw TCP or UDP: 421 port-not-http;
//   - "<fqdn>:<anything else>": 421 port-not-declared;
//   - "<fqdn>" and "<fqdn>:18081" when the legacy target is not an HTTP port:
//     421 port-required, or no-http-port for a service with no HTTP port.
//
// Without these the request named a cluster that does not exist, waited out
// the on-demand timeout and answered 503; for a raw-TCP port it reached an h2
// cluster that could not answer.
//
// The mesh HTTP port of a service whose legacy target IS HTTP keeps its
// cluster: an explicit virtual host, because the port-not-declared wildcard
// would otherwise claim it.
func (c *SnapshotCache) outboundRefusalVhostsLocked(in ruleInputs) []*routev3.VirtualHost {
	if c.edge || len(c.servicePorts) == 0 {
		return nil
	}
	out := make([]*routev3.VirtualHost, 0, 2*len(c.servicePorts))
	for svc, ports := range c.servicePorts {
		if c.loopbackSourceLocked(svc, in) == loopbackHeld {
			out = c.appendOutboundRefusalsLocked(out, svc, ports, in)
		}
	}
	return c.dropClaimedDomainsLocked(out)
}

// loopbackSource is where the loopback table takes a mesh service's virtual
// hosts from. Exactly one per service, decided in ONE place
// (loopbackSourceLocked) and asked by every builder: the three builders used
// to carry a gate each ("has a cluster entry", "its ports are known", "its
// Service declares ports"), and wherever two gates disagreed about a service
// none of them built anything for it. Its mesh authority then fell to the
// catch-all's on-demand route, which runs neither the service's filter nor
// its routes. That happened five times, each through another pair of gates.
type loopbackSource int

const (
	// loopbackUnknown: nothing is known of the name. The catch-all answers.
	loopbackUnknown loopbackSource = iota
	// loopbackHeld: the node derived the service's ports from registry rows.
	// Its cluster entries carry the virtual hosts of its HTTP ports, and
	// outboundRefusalVhostsLocked claims every other authority of it.
	loopbackHeld
	// loopbackDeclared: no rows, and its generated Service declares ports
	// (outboundDeclaredVhostsLocked).
	loopbackDeclared
	// loopbackCold: neither, and a service-wide filter or a route names it
	// (outboundColdVhostsLocked).
	loopbackCold
)

// loopbackSourceLocked classifies one service. Caller must hold clusterMu.
func (c *SnapshotCache) loopbackSourceLocked(svc string, in ruleInputs) loopbackSource {
	switch {
	case c.servicePorts[svc].known():
		return loopbackHeld
	case in.declared[svc].known():
		return loopbackDeclared
	}
	if _, filtered := in.chainFilters[svc]; filtered || len(in.routes[svc]) > 0 {
		return loopbackCold
	}
	return loopbackUnknown
}

// outboundColdVhostsLocked returns the out_http virtual hosts of the services
// that carry a service-wide HTTP filter or route rules and that this node
// holds no cluster of. Caller must hold clusterMu.
//
// Such a service is reached on the loopback path through the on-demand route:
// the request is parked, the cluster is fetched, the request resumes. On the
// catch-all's route it resumed WITHOUT the service's filter, which only the
// service's own virtual host enables (an ext_authz filter is one: the first
// request to a service from a node was not authorized), and without its
// routes' matches, filters and backends (it reached the service's own
// cluster as if no HTTPRoute existed). The filter and the rules are known
// here whether or not the node holds an endpoint: they come from the API. So
// the service gets virtual hosts of its own before its cluster does, on the
// same on-demand route, with both. Once the node holds the cluster, the
// entry's virtual hosts and the refusals replace them.
//
// The service's ports are not known yet, so the rules are placed by what the
// rules themselves say: a rule whose parentRef names no port applies to every
// authority of the service ("<fqdn>" and "<fqdn>:*"); a rule whose parentRef
// names a port applies to the authority of the pod port behind it, which gets
// a virtual host of its own with the port-less rules as well (an exact
// authority is matched before the wildcard). A parentRef port that cannot be
// resolved (a named targetPort, or no such Service port) places its rule
// nowhere, here as on the service's real virtual hosts.
func (c *SnapshotCache) outboundColdVhostsLocked(in ruleInputs) []*routev3.VirtualHost {
	if c.edge || (len(in.chainFilters) == 0 && len(in.routes) == 0) {
		return nil
	}
	cold := map[string]struct{}{}
	// Only a name that carries a filter or a route. A port map alone does not
	// make a name the mesh's: the agent has the port map of EVERY Service in
	// the cluster, a plain one that fronts no mesh pod included, and a
	// stand-in for each would both make every such Service a mesh name (its
	// short name would be parked for a cluster that never comes instead of
	// passed through to it) and cost a virtual host per Service port on every
	// node. A name with a filter or a route that a user's Service owns gets
	// its stand-in from the port map (appendMappedColdVhostsLocked).
	for svc := range in.chainFilters {
		cold[svc] = struct{}{}
	}
	for svc, rules := range in.routes {
		if len(rules) > 0 {
			cold[svc] = struct{}{}
		}
	}
	var out []*routev3.VirtualHost
	for svc := range cold {
		if c.loopbackSourceLocked(svc, in) == loopbackCold {
			out = c.appendColdVhostsLocked(out, in, svc)
		}
	}
	return c.dropClaimedDomainsLocked(out)
}

// outboundDeclaredVhostsLocked returns the out_http virtual hosts of the
// services this node holds no cluster of and whose generated Service says
// what their ports are (ruleInputs.declared). Caller must hold clusterMu.
//
// They are the virtual hosts the service will have once the node holds an
// endpoint of it: each declared HTTP port routes to that port's cluster
// ("<fqdn>:<port>", fetched on demand), with the service's filter and the
// rules of the port; a name without a port is port 80 when 80 is one of them;
// everything else is refused by name, with the reasons a known service gets.
// So a first request is delivered exactly where a later one is, or refused
// exactly as a later one is: there is no authority for which the node asks
// for a cluster by the name the client typed.
func (c *SnapshotCache) outboundDeclaredVhostsLocked(in ruleInputs) []*routev3.VirtualHost {
	if c.edge || len(in.declared) == 0 {
		return nil
	}
	var out []*routev3.VirtualHost
	for svc, ports := range in.declared {
		if c.loopbackSourceLocked(svc, in) != loopbackDeclared {
			continue
		}
		for _, p := range ports.http {
			if vh := c.outboundPortVhostLocked(svc, proxy.PortClusterName(svc, c.meshDomain, p), p, false, in); vh != nil {
				out = append(out, vh)
			}
		}
		out = c.appendOutboundRefusalsLocked(out, svc, ports, in)
	}
	return c.dropClaimedDomainsLocked(out)
}

// appendColdVhostsLocked appends ONE service's stand-in virtual hosts
// (outboundColdVhostsLocked). Caller must hold clusterMu.
func (c *SnapshotCache) appendColdVhostsLocked(out []*routev3.VirtualHost, in ruleInputs, svc string) []*routev3.VirtualHost {
	fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
	if fqdn == "" {
		return out
	}
	if m := in.portMaps[svc]; len(m) > 0 {
		return c.appendMappedColdVhostsLocked(out, in, svc, m)
	}
	add := func(name string, domains []string, rules []proxy.GammaRoute) {
		for i := range rules {
			rules[i].Backends = resolveBackends(rules[i].Backends, in.portMaps, c.portsMap(in), c.meshDomain)
		}
		vh := proxy.BuildOnDemandServiceVirtualHost(name, domains, rules)
		applyChainFilter(vh, in.chainFilters, svc)
		out = append(out, vh)
	}
	everywhere, byPod := coldRules(in, svc)
	// Only authorities WITH a port: the cluster asked for is the one the
	// authority spells, and "<fqdn>:<port>" is a port's cluster. The port-less
	// authority would spell the bare cluster name, which is no port's; for a
	// service nothing is known of it cannot be said whether it has a port 80,
	// so it is refused until it is.
	add(fqdn+"|cold", []string{fqdn + ":*"}, everywhere)
	out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|port-required", []string{fqdn}, proxy.RefusalPortRequired,
		fmt.Sprintf("aether: %s was addressed without a port, and this node does not know its ports yet. Address it as %s:<port>.\n", fqdn, fqdn)))
	// The mesh's own two ports are a port of no service, known or not: an
	// authority that names one is refused here too, and not parked for a
	// cluster that can never exist (proxy.RefusalReservedPortBody).
	out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|reserved",
		[]string{fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyOutboundPort), fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyL4OutboundPort)},
		proxy.RefusalPortNotDeclared, proxy.RefusalReservedPortBody))
	for pod, rules := range byPod {
		if reservedMeshPort(pod) {
			continue // a Service port that maps onto one: refused above
		}
		add(fmt.Sprintf("%s|cold|%d", fqdn, pod), []string{fmt.Sprintf("%s:%d", fqdn, pod)}, rules)
	}
	return out
}

// appendMappedColdVhostsLocked appends the stand-in virtual hosts of a service
// this node holds no cluster of whose name a USER's Service owns: the node has
// that Service's port map, so it knows the service's ports, and the service is
// routed and refused by them like one it holds an endpoint of. Caller must
// hold clusterMu.
//
//   - "<fqdn>:<service port>": the rules of that port, then the cluster of the
//     pod port behind it, fetched on demand;
//   - "<fqdn>" when the Service has a port 80: that port;
//   - every other port, the pod ports' own numbers included: 421.
//
// Before this the stand-in claimed "<fqdn>:*": any port spelling was taken,
// and a rule with a backendRef answered it from the rule's backends, where the
// same request is refused once the node holds an endpoint. This is a steady
// state on the proxy port, not a window: the name of a route's parent is
// always known this way.
//
// It is for a name that carries a filter or a route (outboundColdVhostsLocked
// says why not for every Service with a port map). A mesh service behind a
// user's Service that has neither is, while the node holds no endpoint of it,
// a name the node knows nothing of: "<fqdn>:<port>" is offered on demand with
// the port read as the pods'.
//
// What a port map does not say is the class of a pod port. Every mapped pod
// port is taken for HTTP here; one the pods declare raw TCP is asked for on
// demand, never arrives, and is refused "port-not-http" only once an endpoint
// is held. A named targetPort that no EndpointSlice resolved yet (0) has no
// pod port to ask for: its Service port is refused with the rest.
func (c *SnapshotCache) appendMappedColdVhostsLocked(out []*routev3.VirtualHost, in ruleInputs, svc string, portMap map[uint32]uint32) []*routev3.VirtualHost {
	var pods []uint32
	for _, pod := range portMap {
		if pod != 0 && !reservedMeshPort(pod) && !slices.Contains(pods, pod) {
			pods = append(pods, pod)
		}
	}
	slices.Sort(pods)
	assumed := servicePorts{http: pods}
	in.ports = maps.Clone(c.portsMap(in))
	if in.ports == nil {
		in.ports = map[string]servicePorts{}
	}
	in.ports[svc] = assumed
	for _, pod := range pods {
		if vh := c.outboundPortVhostLocked(svc, proxy.PortClusterName(svc, c.meshDomain, pod), pod, false, in); vh != nil && len(vh.GetDomains()) > 0 {
			out = append(out, vh)
		}
	}
	return c.appendOutboundRefusalsLocked(out, svc, assumed, in)
}

// coldRules splits the rules of a service this node holds no cluster of
// (outboundColdVhostsLocked): the ones that apply to every authority of the
// service, and, per pod port a port-specific rule resolves to, the rules of
// that port's authority (the port-less ones included, in the route's order).
// Every returned slice is a copy.
func coldRules(in ruleInputs, svc string) (everywhere []proxy.GammaRoute, byPod map[uint32][]proxy.GammaRoute) {
	rules := in.routes[svc]
	pods := map[uint32]struct{}{}
	for _, rule := range rules {
		if rule.ParentPort == 0 {
			everywhere = append(everywhere, rule)
			continue
		}
		if pod, ok := podPort(in.portMaps, svc, rule.ParentPort); ok {
			pods[pod] = struct{}{}
		}
	}
	byPod = make(map[uint32][]proxy.GammaRoute, len(pods))
	for pod := range pods {
		for _, rule := range rules {
			resolved, ok := podPort(in.portMaps, svc, rule.ParentPort)
			if rule.ParentPort == 0 || (ok && resolved == pod) {
				byPod[pod] = append(byPod[pod], rule)
			}
		}
	}
	return everywhere, byPod
}

// dropClaimedDomainsLocked removes from the refusing virtual hosts every
// domain a cluster entry's own virtual host already claims, and every virtual
// host left with none. Caller must hold clusterMu.
//
// The two are built from one answer (servicePorts): an entry whose port the
// service does not serve as HTTP publishes no virtual host
// (outboundPortVhostLocked), so they are not expected to overlap. This is the
// guard for a case nobody thought of: a domain claimed twice makes the proxy
// reject the WHOLE route table, which is every pod's loopback path on the
// node, where a missing refusal is one slow error. The entry wins. It runs on
// every snapshot build, so it says so at DEBUG.
func (c *SnapshotCache) dropClaimedDomainsLocked(refusals []*routev3.VirtualHost) []*routev3.VirtualHost {
	claimed := make(map[string]struct{}, 2*len(c.clusters))
	for _, entry := range c.clusters {
		for _, d := range entry.vhost.GetDomains() {
			claimed[d] = struct{}{}
		}
	}
	out := refusals[:0]
	for _, vh := range refusals {
		kept := vh.Domains[:0]
		for _, d := range vh.GetDomains() {
			if _, taken := claimed[d]; taken {
				c.log.Debug("a refusing virtual host and a cluster's virtual host claim the same authority; keeping the cluster's", "authority", d, "refusal", vh.GetName())
				continue
			}
			kept = append(kept, d)
		}
		if vh.Domains = kept; len(kept) > 0 {
			out = append(out, vh)
		}
	}
	return out
}

// appendOutboundRefusalsLocked appends ONE service's refusing out_http
// virtual hosts (outboundRefusalVhostsLocked). Caller must hold clusterMu.
func (c *SnapshotCache) appendOutboundRefusalsLocked(out []*routev3.VirtualHost, svc string, ports servicePorts, in ruleInputs) []*routev3.VirtualHost {
	fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
	if fqdn == "" {
		return out
	}
	portMap, mapped := in.portMaps[svc]
	// The port-less authority, when the service has no HTTP port 80 for it.
	// It is refused by name: 18081 is the proxy's port, not a port of the
	// service, and a name without a port is port 80 and nothing else.
	if _, ok := portlessPodPort(in.portMaps, svc, ports); !ok {
		if ports.servesHTTP() {
			out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|port-required", []string{fqdn}, proxy.RefusalPortRequired,
				mappedRefusalBody(fqdn, "was addressed without a port: a name without a port is port 80, which it does not serve as HTTP", portMap, mapped, ports)))
		} else {
			out = append(out, c.noHTTPPortVhost(fqdn+"|port-required", svc, []string{fqdn}, ports, portMap))
		}
	}
	var notHTTP []string
	for _, p := range append(slices.Clone(ports.tcp), ports.udp...) {
		// The authority's port is the Service's: for a name a user's Service
		// owns, the Service ports in front of this pod port.
		if servicePorts, mapped := servicePortsOf(in.portMaps, svc, p); mapped {
			for _, sp := range servicePorts {
				notHTTP = append(notHTTP, fmt.Sprintf("%s:%d", fqdn, sp))
			}
			continue
		}
		notHTTP = append(notHTTP, fmt.Sprintf("%s:%d", fqdn, p))
	}
	if len(notHTTP) > 0 {
		slices.Sort(notHTTP)
		out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|not-http", notHTTP, proxy.RefusalPortNotHTTP,
			mappedRefusalBody(fqdn, "does not serve HTTP on that port", portMap, mapped, ports)))
	}
	return append(out, proxy.BuildRefusalVirtualHost(fqdn+"|undeclared", []string{fqdn + ":*"}, proxy.RefusalPortNotDeclared,
		mappedRefusalBody(fqdn, "declares no such port", portMap, mapped, ports)))
}

// tcpSpellings returns the raw-TCP addresses of a service that serves no HTTP
// port, for the no-http-port refusal body.
func (c *SnapshotCache) tcpSpellings(svc string, ports servicePorts) string {
	fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
	var spellings []string
	if ports.legacyTCPPort() != 0 {
		spellings = append(spellings, fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyL4OutboundPort))
	}
	for _, p := range ports.tcp {
		spellings = append(spellings, fmt.Sprintf("%s:%d", fqdn, p))
	}
	if len(spellings) == 0 {
		return "(it declares no raw-TCP port either)"
	}
	return strings.Join(spellings, " or ")
}

// pruneServicePortsLocked drops what was derived for a service that is in no
// listing of this refresh (listed) and has no cluster entry left. Caller must
// hold clusterMu, after the retention step.
//
// A service that vanished from the registry listing keeps its clusters, empty,
// for a grace period (retainAbsentClustersLocked): pod churn can transiently
// empty a single-replica service. Its ports are kept for exactly as long, for
// the same reason and one more: the capture listener's chains are built from
// them, and a port number leaving and re-entering the mesh in the length of a
// pod restart would rebuild every pod's capture listener twice.
func (c *SnapshotCache) pruneServicePortsLocked(listed map[string]struct{}) {
	if len(c.servicePorts) == len(listed) {
		return
	}
	// (The caller compares the result with the previous refresh's and bumps
	// servicePortsGen: noteServicePortsLocked.)
	routed := make(map[string]struct{}, len(c.clusters))
	for _, entry := range c.clusters {
		routed[entry.service] = struct{}{}
	}
	for svc := range c.servicePorts {
		if _, ok := listed[svc]; ok {
			continue
		}
		if _, ok := routed[svc]; !ok {
			delete(c.servicePorts, svc)
		}
	}
}

// noteServicePortsLocked advances servicePortsGen when this refresh's derived
// ports differ from the previous refresh's (prev). The capture route tables
// are kept between snapshots on that generation (captureRouteTables), so a
// change that did not advance it would leave them stale. Caller must hold
// clusterMu, after pruneServicePortsLocked.
func (c *SnapshotCache) noteServicePortsLocked(prev map[string]servicePorts) {
	same := maps.EqualFunc(prev, c.servicePorts, sameServicePorts)
	if !same {
		c.servicePortsGen++
		if c.podPortsChanged != nil {
			c.podPortsChanged()
		}
	}
}

// servicePortConflictsLocked returns, per service, the ports whose class its
// endpoints disagree about, as one string. Caller must hold clusterMu.
func (c *SnapshotCache) servicePortConflictsLocked() map[string]string {
	var out map[string]string
	for svc, ports := range c.servicePorts {
		if len(ports.conflicts) == 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[svc] = strings.Join(ports.conflicts, "; ")
	}
	return out
}

// reportServicePortConflicts says, once per distinct state, which services
// have endpoints that declare different classes for one port number, and what
// each side declares (proposal 043, fact 4: "with a WARN").
//
// It is expected for the length of a rollout that retypes a port, and a
// misconfiguration when it lasts: the port is served as the class most
// endpoints declare, so the callers of the other side are not served. A
// registry refresh runs on every endpoint change, so an unchanged disagreement
// is not repeated; a change in the counts is.
func (c *SnapshotCache) reportServicePortConflicts(ctx context.Context, now map[string]string) {
	c.clusterMu.Lock()
	last := c.servicePortConflicts
	c.servicePortConflicts = now
	c.clusterMu.Unlock()
	for _, svc := range slices.Sorted(maps.Keys(now)) {
		if last[svc] == now[svc] {
			continue
		}
		c.log.WarnContext(ctx, "a mesh service's ports cannot all be served as its endpoints declare them: "+
			"a port whose endpoints declare different protocols is served as the protocol most of them declare, "+
			"and a port reserved for the mesh (18081, 18082) is not served at all",
			"service", svc, "ports", now[svc])
	}
}

// meshName is one name the capture tables answer for: a mesh service with a
// generated Service, or the Service a route names as its parent.
type meshName struct {
	// svc is the "<ns>/<name>" key.
	svc string
	// mesh is the service's mesh authority and default cluster name.
	mesh string
	// names are every authority, without a port, a client can address the
	// service by: the mesh name, the cluster.local forms and, when one
	// namespace owns it, the bare name.
	names []string
	// only: names is the mesh name and nothing else, because the service's
	// Kubernetes names are not the mesh's (meshOnly). Set where names is.
	only bool
	// ports is what this node knows of the service's ports; the zero value
	// when it holds no endpoint of it.
	ports servicePorts
	// portMap is the Service's port map when the Service is a user's
	// (servicePortMaps), nil for a generated one.
	portMap map[uint32]uint32
	mapped  bool
	// targetPorts are the parentRef ports of the routes that name the service
	// (routeTargetPorts): the only Service ports known of a route target that
	// has neither endpoints here nor a port map.
	targetPorts []uint32
	// arms are the service's east-west QUIC selection arms, when eligible.
	arms map[string]string
}

// meshOnly reports whether the tables of a dialled port have the name by its
// mesh authority alone: a mesh service whose Kubernetes names are not the
// mesh's (a user's Service owns them and no route names it, or its generated
// Service has not been seen yet).
func (n meshName) meshOnly() bool { return n.only }

// servicePortFor returns the pod port behind SERVICE port p of the name, and
// whether the Service has that port at all.
func (n meshName) servicePortFor(p uint32) (pod uint32, declared bool) {
	if n.mapped {
		pod, declared = n.portMap[p]
		return pod, declared
	}
	if n.ports.known() {
		return p, n.ports.classOf(p) != 0
	}
	return p, slices.Contains(n.targetPorts, p)
}

// httpServicePorts returns the Service ports of the name that carry HTTP: the
// port numbers whose table holds a routing virtual host for it.
func (n meshName) httpServicePorts() []uint32 {
	switch {
	case n.mapped:
		out := make([]uint32, 0, len(n.portMap))
		for sp, pod := range n.portMap {
			// A port of a service this node knows the classes of counts only
			// when its pod port is HTTP, and one whose targetPort is a name
			// that has not resolved (pod 0) carries nothing: it is refused
			// (appendNameVhosts).
			if pod == 0 || (n.ports.known() && !n.ports.hasHTTP(pod)) {
				continue
			}
			out = append(out, sp)
		}
		slices.Sort(out)
		return out
	case n.ports.known():
		return n.ports.http
	default:
		return n.targetPorts
	}
}

// captureTableInputs is everything the capture tables of one snapshot are
// built from, read once.
type captureTableInputs struct {
	names []meshName
	rules ruleInputs
	all   map[string]servicePorts
}

// captureTableInputs reads the inputs of the capture route tables. Locks are
// taken one after the other, never nested: depMu, captureMu, clusterMu.
func (c *SnapshotCache) captureTableInputs() captureTableInputs {
	// Read-only membership tests only, so take the shared memo rather than a copy.
	return c.captureTableInputsFor(c.dependencySetShared(), c.quicArmsByService())
}

// tableNameKeys returns the services the capture tables have a name for:
// every generated mesh Service, every route target, and every service that
// carries a service-wide HTTP filter.
//
// The last are names of the tables whether or not this agent knows a Service
// for them (the registrar generates it on a ticker after the first endpoint
// registers, and generates none where a user's Service owns the name).
// Without a virtual host of its own such a service's mesh authority was
// routed by the catch-all, with the filter skipped. It has no Kubernetes name
// to claim: only its mesh authority.
func tableNameKeys(authorities map[string]string, rules ruleInputs) map[string]struct{} {
	keys := make(map[string]struct{}, len(authorities)+len(rules.routes))
	for svc := range authorities {
		keys[svc] = struct{}{}
	}
	for svc := range rules.routes {
		keys[svc] = struct{}{}
	}
	for svc := range rules.chainFilters {
		keys[svc] = struct{}{}
	}
	return keys
}

// captureTableInputsFor is captureTableInputs with the two inputs the tables'
// memo has already read: the dependency set and the east-west QUIC selection
// arms per eligible service (proposal 038 Phase 4b; the same predicate that
// publishes the quic: twins, so a capture route can never name a twin the
// snapshot does not carry).
func (c *SnapshotCache) captureTableInputsFor(deps map[string]struct{}, quicArms map[string]map[string]string) captureTableInputs {
	in := captureTableInputs{rules: ruleInputs{
		routes:       c.serviceRoutesSnapshot(),
		chainFilters: c.serviceChainFiltersSnapshot(),
		portMaps:     c.servicePortMapsSnapshot(),
	}}
	c.depMu.RLock()
	targetPorts := c.routeTargetPorts
	c.depMu.RUnlock()

	c.captureMu.RLock()
	authorities := c.captureAuthorities
	c.captureMu.RUnlock()

	in.rules.declared = c.declaredByService()
	c.clusterMu.RLock()
	in.all = effectivePorts(c.servicePorts, in.rules.declared)
	c.clusterMu.RUnlock()
	in.rules.ports = in.all

	keys := tableNameKeys(authorities, in.rules)
	in.names = make([]meshName, 0, len(keys))
	for _, svc := range slices.Sorted(maps.Keys(keys)) {
		// Scoped to the dependency set so a table only references clusters
		// the snapshot carries. Every mesh Service with an address and every
		// route target is in it.
		if _, ok := deps[svc]; !ok {
			continue
		}
		ref, ok := serviceref.ParseKey(svc)
		if !ok {
			continue
		}
		fqdn, generated := authorities[svc]
		if !generated {
			fqdn = ref.ClusterLocalFQDN()
		}
		n := meshName{
			svc:         svc,
			mesh:        proxy.ServiceClusterName(svc, c.meshDomain),
			ports:       in.all[svc],
			targetPorts: targetPorts[svc],
			arms:        quicArms[svc],
		}
		n.portMap, n.mapped = in.rules.portMaps[svc]
		if _, routed := in.rules.routes[svc]; generated || routed {
			n.names = serviceNames(ref, fqdn, n.mesh)
		} else {
			n.names, n.only = []string{n.mesh}, true
		}
		in.names = append(in.names, n)
	}
	return in
}

// serviceNames returns every authority, without a port, a client can address
// a service by. A captured request carries whatever name the client dialled:
// Kubernetes search-domain resolution lets it reach a Service by the bare name
// (same namespace), <name>.<namespace>, <name>.<namespace>.svc and the full
// cluster.local name, besides aether's mesh name. Every table must match every
// spelling, or a dial by a short name misses the service's virtual host and is
// passed through to the cluster's own routing, outside the mesh.
func serviceNames(ref serviceref.ServiceRef, fqdn, mesh string) []string {
	// The short name ("<svc>") is NOT one of them: it means the service of
	// that name in the caller's namespace, and is claimed by a virtual host
	// of its own that every namespace's service of the name shares
	// (proxy.ShortNameVirtualHosts).
	names := []string{fqdn, mesh, ref.Name + "." + ref.Namespace, ref.Name + "." + ref.Namespace + ".svc"}
	out := names[:0]
	for _, n := range names {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// domains renders names with each suffix: "" for the port-less authority,
// ":<port>" for one port, ":*" for every port (a prefix wildcard, which an
// exact domain outranks).
func domains(names []string, suffixes ...string) []string {
	out := make([]string, 0, len(names)*len(suffixes))
	for _, s := range suffixes {
		for _, n := range names {
			out = append(out, n+s)
		}
	}
	return out
}

// captureHTTPPorts returns, sorted, the HTTP port numbers of the mesh as this
// node sees them: the port numbers the capture listener gets a chain and a
// route table for (proposal 043, "The chains of a capture listener").
//
//   - every port a mesh service with an address declares as HTTP;
//   - every Service port of a route's parent Service;
//   - the two legacy numbers, the mesh HTTP port and 80.
//
// It is derived from the Services and the registry endpoints, not from the
// demand of the pods on the node, so a first call to a service a pod has never
// used still finds its port's chain. Locks are taken one after the other.
func (c *SnapshotCache) captureHTTPPorts() []uint32 {
	set := map[uint32]struct{}{meshconst.ProxyOutboundPort: {}, 80: {}}

	routes := c.serviceRoutesSnapshot()
	c.depMu.RLock()
	portMaps := c.servicePortMaps
	targetPorts := c.routeTargetPorts
	c.depMu.RUnlock()

	c.clusterMu.RLock()
	all := c.servicePorts
	c.clusterMu.RUnlock()

	c.captureMu.RLock()
	for _, e := range c.captureTCPServices {
		if _, mapped := portMaps[e.serviceName]; mapped {
			continue // a user Service owns the name: its Service ports, below
		}
		ports := e.ports
		if !ports.known() {
			// No endpoint held yet: the Service's own declaration, so that
			// a first request finds the chain and the table of its port.
			ports = e.declared
		}
		for _, p := range ports.http {
			set[p] = struct{}{}
		}
	}
	c.captureMu.RUnlock()

	for svc := range routes {
		n := meshName{ports: all[svc], targetPorts: targetPorts[svc]}
		n.portMap, n.mapped = portMaps[svc]
		for _, p := range n.httpServicePorts() {
			set[p] = struct{}{}
		}
	}
	// The TCP mesh port can never be an HTTP chain's port: the raw-TCP legacy
	// chains match on its number.
	delete(set, meshconst.ProxyL4OutboundPort)
	delete(set, 0)
	return slices.Sorted(maps.Keys(set))
}

// captureHTTPPortsKey renders a port set for comparison.
func captureHTTPPortsKey(ports []uint32) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(int(p)))
	}
	return strings.Join(parts, ",")
}

// reconcileCaptureHTTPPorts rebuilds the per-pod capture listeners when the
// HTTP port set of the mesh has CHANGED since they were last built.
//
// The set has three sources that change on their own schedules (the
// mesh-Service reconciler, the registry refresh, the GAMMA reconciler), and a
// listener is also built by whichever pod event comes next. Running here, on
// every snapshot build, is the discipline reconcileCaptureTCPChains follows for
// the same reason: one comparison over the DERIVED set, so that no source can
// leave a listener without a chain for a port forever, and so that nothing is
// rebuilt on an event that left the set alone. A listener rebuild drains
// connections, and the set changes only when a port NUMBER enters or leaves
// the mesh, which is rarer than a service changing (proposal 043, Risk 8).
func (c *SnapshotCache) reconcileCaptureHTTPPorts() {
	if !c.captureEnabled || c.edge {
		return
	}
	key := captureHTTPPortsKey(c.captureHTTPPorts())

	c.captureMu.Lock()
	changed := c.captureHTTPPortsSeen != key
	c.captureHTTPPortsSeen = key
	c.captureMu.Unlock()

	if !changed {
		return
	}
	shared := c.extensionHTTPFilters()
	c.listenerMu.Lock()
	c.rebuildPodListenersLocked("HTTP port set change", shared)
	c.listenerMu.Unlock()
}

// captureTablePorts returns the port numbers a capture route table must be
// published for in a snapshot that carries listeners: every port the node's
// HTTP port set holds now, and every port a cap_http_<P> chain of one of
// those listeners names.
//
// The second half is what makes "a chain and its table appear and disappear
// in one snapshot" true by construction instead of by timing. A chain naming
// a table the snapshot does not carry leaves its listener warming until the
// initial-fetch timeout (#817); reading the names off the very listeners
// being published means a listener built a moment ago against a set that has
// since changed still finds its tables. A table no chain names costs nothing:
// no proxy subscribes to it.
func captureTablePorts(listeners []types.Resource, current []uint32) []uint32 {
	set := make(map[uint32]struct{}, len(current))
	for _, p := range current {
		set[p] = struct{}{}
	}
	for _, r := range listeners {
		l, ok := r.(*listenerv3.Listener)
		if !ok || l == nil || !strings.HasPrefix(l.GetName(), "capture_") {
			continue
		}
		for _, fc := range l.GetFilterChains() {
			if p, ok := proxy.CaptureHTTPRoutePort(fc.GetName()); ok {
				set[p] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// captureTablesKey names the inputs a set of capture route tables was built
// from, by generation: while it is unchanged (and the QUIC arms are, which
// have no generation of their own) the tables built last are still exact.
type captureTablesKey struct {
	// depGen covers everything under depMu: the routes, the chain filters,
	// the port maps, the route targets' ports and the dependency set's inputs.
	depGen uint64
	// deps is the identity of the memoized dependency set, which is also
	// rebuilt when an observed dependency expires by wall clock, with no
	// mutator running.
	deps uintptr
	// authGen: the mesh Services' names. portsGen: the registry's ports.
	authGen  uint64
	portsGen uint64
	// ports is the set of tables asked for.
	ports string
}

// captureTablesMemo is the last set of capture route tables and what it was
// built from. Guarded by snapshotMu (generateSnapshot is the only caller on
// the serving path).
type captureTablesMemo struct {
	valid bool
	key   captureTablesKey
	// deps is the dependency set whose identity key.deps is. Held, so that the
	// address cannot be given to another map while the memo still compares
	// against it.
	deps   map[string]struct{}
	arms   map[string]map[string]string
	tables []types.Resource
}

// captureRouteTables returns the capture listener's route tables for one
// snapshot: cap_http_<P> for every port in ports, and cap_http_other.
//
// The tables are a function of the Services, the registry's ports, the routes
// and the QUIC arms, none of which an endpoint coming or going changes. They
// are therefore kept between snapshots and returned AS THE SAME OBJECTS while
// those inputs are unchanged, so the snapshot's version memo does not marshal
// and hash them again (#1105, #1115): a node has one such table per HTTP port
// number, each holding a virtual host or two per mesh service, and a snapshot
// is built on every pod event.
//
// The generations are read BEFORE the inputs. A mutation that lands in
// between leaves tables newer than their key; the next build then sees a
// newer generation and rebuilds, which costs one build and never serves a
// stale table.
func (c *SnapshotCache) captureRouteTables(ports []uint32) []types.Resource {
	key := captureTablesKey{ports: captureHTTPPortsKey(ports)}
	c.depMu.RLock()
	key.depGen = c.depGen
	c.depMu.RUnlock()
	c.captureMu.RLock()
	key.authGen = c.captureAuthGen
	c.captureMu.RUnlock()
	c.clusterMu.RLock()
	key.portsGen = c.servicePortsGen
	c.clusterMu.RUnlock()

	deps := c.dependencySetShared()
	key.deps = reflect.ValueOf(deps).Pointer()
	arms := c.quicArmsByService()
	if m := &c.captureTables; m.valid && m.key == key && maps.EqualFunc(m.arms, arms, func(a, b map[string]string) bool { return maps.Equal(a, b) }) {
		return m.tables
	}

	in := c.captureTableInputsFor(deps, arms)
	out := make([]types.Resource, 0, len(ports)+1)
	for _, p := range ports {
		if p == meshconst.ProxyOutboundPort {
			// The table of the proxy port is not a per-port table: it is the
			// loopback table under another name (proxyPortCaptureTable),
			// built with the snapshot.
			continue
		}
		out = append(out, proxy.BuildCaptureRouteConfiguration(
			proxy.CaptureHTTPRouteName(p),
			c.captureTableVhosts(in, p),
			// Never a port-less authority: on a dial to port 80 it would
			// name the cluster of the bare name, which is not port 80's. A
			// name this node knows has its own virtual host for it
			// (coldVhost).
			proxy.BuildCaptureCatchAllVirtualHost(c.meshDomain, p, false, c.captureRedirectAll),
			c.shortNamesOf(in, p),
		))
	}
	out = append(out, proxy.BuildCaptureRouteConfiguration(
		proxy.CaptureHTTPOtherRouteName,
		c.captureTableVhosts(in, 0),
		proxy.BuildCaptureCatchAllVirtualHost(c.meshDomain, 0, false, c.captureRedirectAll),
		c.shortNamesOf(in, 0),
	))
	c.captureTables = captureTablesMemo{valid: true, key: key, deps: deps, arms: arms, tables: out}
	return out
}

// captureTableVhosts builds the virtual hosts of the table of dialled port
// port (0 for cap_http_other), in name order.
// shortNamesOf says how the table of dialled port port (0: cap_http_other)
// builds its short names.
func (c *SnapshotCache) shortNamesOf(in captureTableInputs, port uint32) *proxy.ShortNames {
	short := &proxy.ShortNames{MeshDomain: c.meshDomain}
	for _, n := range in.names {
		// A name the table claims by its mesh name alone (a mesh service
		// behind a user's Service that no route names: the agent has neither
		// a generated Service nor a port map for it). Its Kubernetes names
		// pass through to the user's Service, and so does its short name.
		if n.meshOnly() {
			short.MeshOnly = append(short.MeshOnly, n.mesh)
		}
	}
	// cap_http_other (port 0) claims nothing for a name nothing is known of:
	// its full name leaves through the table's catch-all. So does its short
	// name, with no arm of its own: a caller whose namespace has no arm gets
	// a copy of the catch-all's routes (shortNameVirtualHost), which is the
	// same answer. (It had an arm that held the catch-all's last route alone;
	// nothing told the two apart, except that the arm skipped the catch-all's
	// refusal of the mesh's own two ports.)
	return short
}

func (c *SnapshotCache) captureTableVhosts(in captureTableInputs, port uint32) []*routev3.VirtualHost {
	vhosts := make([]*routev3.VirtualHost, 0, 2*len(in.names))
	for _, n := range in.names {
		if port != 0 {
			vhosts = c.appendNameVhosts(vhosts, in, n, port)
			continue
		}
		// cap_http_other: HTTP on a port number no service is known to
		// declare as HTTP. A name this node knows the ports of is refused.
		//
		// Not one the table has by its mesh name alone (meshOnly: a mesh
		// service behind a Service of the user's that no route names). That
		// refusal appeared the moment a service-wide filter made the service
		// one the node holds, and turned a dial to the user's Service port
		// with the mesh name in Host, which had passed through, into a 421.
		// This table has no on-demand route, so what is not claimed here
		// leaves as any name that is not the mesh's does, to the address that
		// was dialled. (The table of a port number that HAS a chain has an
		// on-demand route, and says so itself: appendMeshOnlyVhosts.)
		if (n.ports.known() || n.mapped) && !n.meshOnly() {
			vhosts = append(vhosts, proxy.BuildRefusalVirtualHost(n.mesh+"|undeclared", domains(n.names, "", ":*"), proxy.RefusalPortNotDeclared,
				mappedRefusalBody(n.mesh, "does not declare the port that was dialled", n.portMap, n.mapped, n.ports)))
			continue
		}
		// A name nothing is known of (no endpoint held, no Service with
		// ports): this table does not know which port was dialled, so there
		// is no port's cluster it could name for it. Nothing is claimed.
	}
	// in.names is sorted and each name appends in a fixed order, so the
	// table's repeated virtual_hosts field is deterministic (#135). The short
	// names are added by the table's builder (shortNamesOf).
	return vhosts
}

// appendNameVhosts appends the virtual hosts of ONE name in the table of
// dialled port d (never 0). See the table at the top of the file.
func (c *SnapshotCache) appendNameVhosts(vhosts []*routev3.VirtualHost, in captureTableInputs, n meshName, d uint32) []*routev3.VirtualHost {
	if n.meshOnly() {
		return c.appendMeshOnlyVhosts(vhosts, in, n, d)
	}
	refuse := func(kind, reason, what string, doms []string) {
		vhosts = append(vhosts, proxy.BuildRefusalVirtualHost(n.mesh+"|"+kind, doms, reason, mappedRefusalBody(n.mesh, what, n.portMap, n.mapped, n.ports)))
	}
	// What this node can say about the name's ports. Without it (a service it
	// holds no endpoint of, a route target that is only a name) there is
	// nothing to refuse BY: the name gets a virtual host where it can be
	// routed and none elsewhere.
	informed := n.ports.known() || n.mapped

	exact := fmt.Sprintf(":%d", d)
	pod, declared := n.servicePortFor(d)
	// A Service port routes when its pod port is HTTP, or is not known to be
	// anything else (a name this node holds no endpoint of).
	//
	// A user Service's port whose targetPort is a NAME that has not resolved
	// to a pod port (pod 0) routes nowhere: nothing says which port it is.
	// It was routed to the service's default cluster, the port the pods
	// declare, with the parent's rules; by the owner's ruling of 2026-10-13
	// it is refused until the name resolves, and the route reconciler says
	// so in the status of every route that names it.
	unresolved := declared && pod == 0 && n.mapped
	routable := declared && pod != 0 && (!n.ports.known() || n.ports.hasHTTP(pod))
	own := domains(n.names, "", exact)

	switch {
	case routable:
		// A port-less Host means the port that was dialled, port 80 included.
		vhosts = append(vhosts, c.portVhost(in, n, d, pod, own))
	case unresolved:
		refuse("refused", proxy.RefusalPortNotDeclared, "has, on the port that was dialled, a targetPort that is a name its Service's EndpointSlices do not resolve yet", own)
	case n.ports.known() && !n.ports.servesHTTP():
		// HTTP to a service that serves none: say where it is reached.
		vhosts = append(vhosts, c.noHTTPPortVhost(n.mesh+"|refused", n.svc, own, n.ports, n.portMap))
	case !informed:
		// A name this node holds no endpoint of, on a port nothing says it
		// has. Its mesh authority may still name the port's cluster on
		// demand (the cold path), and it does so from a virtual host of its
		// OWN, so that the service's HTTP filters and rules are in force when
		// the cluster arrives; the catch-all carries neither.
		vhosts = append(vhosts, c.coldVhost(in, n, d))
	case declared:
		refuse("refused", proxy.RefusalPortNotHTTP, "does not serve HTTP on the port that was dialled", own)
	default:
		refuse("refused", proxy.RefusalPortNotDeclared, "does not declare the port that was dialled", own)
	}

	// A Host that names a port other than the one dialled. The dialled port
	// decides: the request is refused, never answered by another port. That
	// holds for a name this node knows no port of as well: what was dialled
	// is known, and no mesh name may reach the table's fallthrough.
	refuse("mismatch", proxy.RefusalPortMismatch, fmt.Sprintf("was dialled on port %d with a Host header that names another port", d), domains(n.names, ":*"))
	return vhosts
}

// appendMeshOnlyVhosts appends the virtual hosts, in the table of dialled port
// d, of a name the table has by its mesh authority alone (meshName.only): a
// mesh service whose Kubernetes names a Service of the user's owns and that no
// route names. The node has it in its tables because something applies to it,
// typically a service-wide filter.
//
// Such a service is reached by a captured dial only at an address that is not
// the mesh's (the user's Service, or another Service that selects the same
// pods), so what the dial meant was decided before any filter existed, and a
// filter must not turn a call that worked into a refusal. On this table an
// authority that names the service is therefore NEVER refused, and it is one
// of two things, decided by the service's OWN pods and by nothing else:
//
//   - a mesh delivery, WITH the service's filters and rules, when the port
//     number that was dialled is one the service's own pods declare as HTTP,
//     with a Host that names that port or none. Before the filter the
//     catch-all fetched that cluster on demand;
//   - what the table's catch-all does for a name that is not the mesh's, for
//     every other dial: it leaves to the address that was dialled.
//
// The port map of the Service that owns the name decides nothing here. It did
// for a while (a port of that Service was a delivery too), and since this
// table exists only for a port number some mesh service declares as HTTP, the
// same dial to the user's Service was taken into the mesh, or left alone,
// according to whether an UNRELATED service happened to declare that number.
//
// What it must never be is fetched on demand WITHOUT the filter. The
// catch-all of a table with a chain offers on demand exactly one spelling of
// a mesh name, "<mesh name>:<the port of the table>", and lets every other
// one (no port, another port) fall through to the address that was dialled.
// So that one spelling is the one that must be claimed here: by a virtual
// host that carries the filter, or by one that reaches no mesh cluster
// (BuildFallthroughVirtualHost). Nothing else of the name is claimed, and it
// leaves as it did before the node held the service.
//
// A service the node knows no port of cannot be told either way. Its mesh
// name with the port of the table was offered on demand before the filter and
// still is, from a virtual host of its own that carries the filter
// (coldVhost). That virtual host claims the port-less mesh name as well, so a
// port-less Host, which passed through before the filter, is parked on demand
// with it until the node holds the service's rows: the one spelling where the
// filter changes what a dial does, and it changes it to a filtered delivery
// or a timeout, never to an unfiltered one.
func (c *SnapshotCache) appendMeshOnlyVhosts(vhosts []*routev3.VirtualHost, in captureTableInputs, n meshName, d uint32) []*routev3.VirtualHost {
	own := domains(n.names, "", fmt.Sprintf(":%d", d))
	switch {
	case n.ports.hasHTTP(d):
		return append(vhosts, c.portVhost(in, n, d, d, own))
	case !n.ports.known():
		return append(vhosts, c.coldVhost(in, n, d))
	}
	return append(vhosts, proxy.BuildFallthroughVirtualHost(n.mesh+"|dialled", own, c.captureRedirectAll))
}

// coldVhost is the virtual host of a name this node knows no port of, in the
// table of dialled port d: EVERY name of the service, with ":d" and without a
// port (on a captured dial a port-less Host means the port that was dialled),
// routed to the cluster of that port ("<fqdn>:d"), which the on-demand filter
// fetches. It carries the service's HTTP filters and the rules that apply on
// d.
//
// The names are the ones an informed service's virtual host claims in the
// same table. Claiming only the mesh authority left the Kubernetes names of a
// service the node held no endpoint of (the registry unreachable when the
// agent started) to the table's fallthrough: passed through to a selectorless
// Service's address, an immediate failure, where the request should be parked
// until the cluster arrives.
//
// The cluster is always the one of the dialled port, never the one the bare
// name spells (the cluster of the port the pods declare), also on a dial to
// port 80. The table's catch-all therefore accepts no port-less authority.
func (c *SnapshotCache) coldVhost(in captureTableInputs, n meshName, d uint32) *routev3.VirtualHost {
	vh := proxy.BuildServiceVirtualHost(n.mesh+"|"+strconv.Itoa(int(d)), proxy.PortClusterName(n.svc, c.meshDomain, d),
		domains(n.names, "", fmt.Sprintf(":%d", d)), resolveRules(in.rules, in.all, c.meshDomain, rulePlace{svc: n.svc, servicePort: d}))
	applyChainFilter(vh, in.rules.chainFilters, n.svc)
	return vh
}

// portVhost builds the virtual host that ROUTES the name in the table of
// dialled Service port d, whose pod port is pod: to that port's cluster, with
// the rules that apply to the port, the service's chain filter and, on the
// default cluster's routes, the per-source HTTP/3 selection.
func (c *SnapshotCache) portVhost(in captureTableInputs, n meshName, d, pod uint32, doms []string) *routev3.VirtualHost {
	var cluster string
	switch {
	case n.ports.known():
		cluster = httpClusterForPort(n.svc, c.meshDomain, n.ports, pod)
	default:
		// The Service says which pod port this is, and the node holds no
		// endpoint yet: that port's cluster, fetched on demand, never the
		// cluster the bare name spells.
		cluster = proxy.PortClusterName(n.svc, c.meshDomain, pod)
	}
	vh := proxy.BuildServiceVirtualHost(n.mesh+"|"+strconv.Itoa(int(d)), cluster, doms, resolveRules(in.rules, in.all, c.meshDomain, rulePlace{svc: n.svc, servicePort: d, pod: pod}))
	applyChainFilter(vh, in.rules.chainFilters, n.svc)
	// The per-source HTTP/3 selection applies to the default cluster's
	// routes, here exactly as on out_http.
	if cluster == n.mesh && len(n.arms) > 0 {
		proxy.ApplyQUICClusterSelection(vh, n.mesh, n.arms)
	}
	return vh
}

// noHTTPPortVhost is the refusal of an HTTP request for a service that serves
// no HTTP port at all (proposal 037): 421 no-http-port, naming where the
// service is reached over raw TCP.
//
// A name whose Service has a port with a targetPort that is a name that does
// not resolve yet is told so (portMap, unresolvedNote): that port may well be
// its HTTP port, and "serves no HTTP port, reach it over TCP" alone sent the
// reader the wrong way. And when nothing is known of the service's ports at
// all (a name the node has by a filter or a route and a port map alone), the
// body does not claim it serves none.
func (c *SnapshotCache) noHTTPPortVhost(name, svc string, doms []string, ports servicePorts, portMap map[uint32]uint32) *routev3.VirtualHost {
	fqdn, note := proxy.ServiceClusterName(svc, c.meshDomain), unresolvedNote(portMap)
	body := fmt.Sprintf("aether: %s serves no HTTP port. Reach it over TCP instead: %s\n", fqdn, c.tcpSpellings(svc, ports))
	if note != "" && !ports.known() {
		body = fmt.Sprintf("aether: %s has no port it can be addressed on yet.\n", fqdn)
	}
	return proxy.BuildRefusalVirtualHost(name, doms, proxy.RefusalNoHTTPPort, body+note)
}
