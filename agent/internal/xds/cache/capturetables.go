package cache

import (
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
// Two tables are legacy (proposal 043, "The legacy spellings during Phases 1
// and 2"): the mesh HTTP port (18081), and port 80 for a service that does not
// declare it. A dial to either reaches the service's legacy target if that is
// an HTTP port, and is refused otherwise.

// legacyHTTPPorts are the two port numbers whose table exists for every mesh
// service whether or not it declares them.
func isLegacyHTTPPort(port uint32) bool {
	return port == meshconst.ProxyOutboundPort || port == 80
}

// ruleInputs is what a virtual host's routes are built from besides the
// service's own cluster: the GAMMA rules, the service-wide chain filters and
// the port maps of the Services the rules name. Read once per build, outside
// the cluster lock; every map is shared and read-only.
type ruleInputs struct {
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

// SetServicePortMaps replaces the port maps of the Services that routes name
// as a parent or as a backend and that the registrar did not generate: Service
// port -> the pod port its targetPort names, 0 for a named targetPort
// (proposal 043, "A Service is a port map"). Fed by the GAMMA reconciler.
// Signals a snapshot rebuild on change.
func (c *SnapshotCache) SetServicePortMaps(portMaps map[string]map[uint32]uint32) {
	c.depMu.Lock()
	changed := !maps.EqualFunc(c.servicePortMaps, portMaps, func(a, b map[uint32]uint32) bool { return maps.Equal(a, b) })
	c.servicePortMaps = portMaps
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
// The parentRef's port is a port of the SERVICE:
//
//   - the Service port the table answers for, whatever its targetPort is (a
//     named targetPort cannot be resolved to a pod port, and the rule still
//     belongs to the port it names);
//   - a user Service's port, through its port map;
//   - a generated Service's port, which is the pod's. Its two legacy numbers
//     are ports of it too and lead to the legacy target: the mesh HTTP port
//     always, and 80 unless the pods declare 80 themselves. A parentRef that
//     names one of them attached its rule to nothing, where a backendRef that
//     names one resolves to the default cluster.
//
// A rule IMPORTED from a peer cluster (proposal 026) carries no port at all:
// the projection has no field for one yet. It was authored for some port of
// the service and nothing here can tell which, so it stays where it applied
// before the tables were per port, on the legacy target's virtual hosts, and
// is never spread over the service's other ports.
func ruleApplies(rule proxy.GammaRoute, portMaps map[string]map[uint32]uint32, ports servicePorts, at rulePlace) bool {
	legacy := ports.legacyHTTPPort()
	onLegacyTarget := at.pod == 0 || at.pod == legacy
	if rule.Imported {
		return onLegacyTarget
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
	if rule.ParentPort == at.pod {
		return true
	}
	if rule.ParentPort == meshconst.ProxyOutboundPort || (rule.ParentPort == 80 && !ports.hasHTTP(80)) {
		return at.pod == legacy
	}
	return false
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
// A backendRef names a port of the backend SERVICE (proposal 043, "The model",
// 6). Until this change the port was not read at all and every backend was the
// backend's default cluster, so a route to port 9090 of a two-port service
// delivered to its other port and answered 200.
//
//   - the port resolves to an HTTP port of the backend: that port's cluster;
//   - the backend is not known on this node (no endpoint held yet), or the
//     rule carries no port (imported from a peer, proposal 026): the cluster
//     the projector named, the backend's default cluster, as before;
//   - the mesh HTTP port, or port 80 of a generated Service that does not
//     declare it: the legacy spelling of the backend, its default cluster;
//   - anything else names no HTTP port of the backend. The route points at
//     "<fqdn>:<port>", a cluster that does not exist, and the request fails
//     where it used to be delivered to a port nobody asked for.
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
		cluster := backendCluster(b, portMaps, all, meshDomain)
		if cluster == b.Cluster {
			continue
		}
		if out == nil {
			out = slices.Clone(backends)
		}
		out[i].Cluster = cluster
	}
	if out == nil {
		return backends
	}
	return out
}

func backendCluster(b proxy.GammaBackend, portMaps map[string]map[uint32]uint32, all map[string]servicePorts, meshDomain string) string {
	ports := all[b.Service]
	if b.Port == 0 || !ports.known() {
		return b.Cluster
	}
	pod, ok := podPort(portMaps, b.Service, b.Port)
	if !ok {
		// A user Service whose targetPort is a name, or that has no such
		// port. The port cannot be resolved to a pod port, so the backend
		// keeps the cluster it had before backendRef ports were read: the
		// projected one. (Rendering "<fqdn>:<Service port>" here named a
		// cluster of ANOTHER port whenever the pods declare that number.)
		// The reconciler reports a named targetPort.
		return b.Cluster
	}
	if ports.hasHTTP(pod) {
		return httpClusterForPort(b.Service, meshDomain, ports, pod)
	}
	if _, mapped := portMaps[b.Service]; !mapped && isLegacyHTTPPort(pod) && ports.legacyHTTPPort() != 0 {
		return proxy.ServiceClusterName(b.Service, meshDomain)
	}
	return proxy.PortClusterName(b.Service, meshDomain, pod)
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
	if ports := c.servicePorts[svc]; !isDefault && ports.known() && !ports.hasHTTP(port) {
		return nil
	}
	rules := resolveRules(in, c.servicePorts, c.meshDomain, rulePlace{svc: svc, pod: port})
	var vh *routev3.VirtualHost
	if isDefault {
		fqdn := proxy.ServiceClusterName(svc, c.meshDomain)
		vh = proxy.BuildOutboundServiceVirtualHost(fqdn, []string{fqdn, fmt.Sprintf("%s:%d", fqdn, port)}, rules)
	} else {
		vh = proxy.BuildOutboundServiceVirtualHost(entryName, []string{entryName}, rules)
	}
	if ef, ok := in.chainFilters[svc]; ok {
		proxy.ApplyServiceChainFilter(vh, &ef)
	}
	return vh
}

// refusalBody is the body of every 421 the mesh answers for svc: what was
// wrong, and the ports the service does declare.
func refusalBody(fqdn, what string, ports servicePorts) string {
	return fmt.Sprintf("aether: %s %s. Its ports: %s. Address it as %s:<port>.\n", fqdn, what, ports.describe(), fqdn)
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
	// A service is refused by name only while it has a cluster entry: that is
	// what "this node routes the service" means everywhere else.
	routed := make(map[string]struct{}, len(c.clusters))
	for _, entry := range c.clusters {
		routed[entry.service] = struct{}{}
	}
	out := make([]*routev3.VirtualHost, 0, 2*len(c.servicePorts))
	for svc, ports := range c.servicePorts {
		if _, ok := routed[svc]; ok {
			out = c.appendOutboundRefusalsLocked(out, svc, ports, in)
		}
	}
	return c.dropClaimedDomainsLocked(out)
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
	meshPort := fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyOutboundPort)
	switch {
	case ports.legacyHTTPPort() != 0:
		// The legacy spelling of the default cluster. The alias cluster of
		// that name exists whenever the default one does, unless the
		// service declares 18081 itself.
		if _, ok := c.clusters[meshPort]; ok && !ports.hasHTTP(meshconst.ProxyOutboundPort) {
			// It is a virtual host of the service like any other: the rules of
			// the legacy target and the service's HTTP filters apply on it.
			vh := proxy.BuildServiceVirtualHost(fqdn+"|legacy", meshPort, []string{meshPort},
				resolveRules(in, c.servicePorts, c.meshDomain, rulePlace{svc: svc, pod: ports.legacyHTTPPort()}))
			applyChainFilter(vh, in.chainFilters, svc)
			out = append(out, vh)
		}
	case ports.servesHTTP():
		out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|legacy", []string{fqdn, meshPort}, proxy.RefusalPortRequired,
			refusalBody(fqdn, "was addressed without a port, and the port its pods declare is not an HTTP port", ports)))
	default:
		out = append(out, c.noHTTPPortVhost(fqdn+"|legacy", svc, []string{fqdn, meshPort}, ports))
	}
	var notHTTP []string
	for _, p := range append(slices.Clone(ports.tcp), ports.udp...) {
		if p != meshconst.ProxyOutboundPort {
			notHTTP = append(notHTTP, fmt.Sprintf("%s:%d", fqdn, p))
		}
	}
	if len(notHTTP) > 0 {
		slices.Sort(notHTTP)
		out = append(out, proxy.BuildRefusalVirtualHost(fqdn+"|not-http", notHTTP, proxy.RefusalPortNotHTTP,
			refusalBody(fqdn, "does not serve HTTP on that port", ports)))
	}
	return append(out, proxy.BuildRefusalVirtualHost(fqdn+"|undeclared", []string{fqdn + ":*"}, proxy.RefusalPortNotDeclared,
		refusalBody(fqdn, "declares no such port", ports)))
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
	same := maps.EqualFunc(prev, c.servicePorts, func(a, b servicePorts) bool {
		return a.equal(b) && slices.Equal(a.conflicts, b.conflicts)
	})
	if !same {
		c.servicePortsGen++
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
		c.log.WarnContext(ctx, "the endpoints of a mesh service declare different protocols for one port; the port is served as the protocol most endpoints declare (ties: http, then tcp)",
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
			// when its pod port is HTTP. A named targetPort (pod 0) cannot be
			// resolved; the port still has its table, where the parent's
			// rules are (appendNameVhosts).
			if pod != 0 && n.ports.known() && !n.ports.hasHTTP(pod) {
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

	c.clusterMu.RLock()
	in.all = c.servicePorts
	c.clusterMu.RUnlock()

	// The bare name is a name of the service only when one namespace owns it:
	// a duplicate domain makes Envoy reject the route table, and a bare name
	// two namespaces share says nothing about which one was meant.
	bare := make(map[string]int, len(authorities)+len(in.rules.routes))
	keys := make(map[string]struct{}, len(authorities)+len(in.rules.routes))
	for svc := range authorities {
		keys[svc] = struct{}{}
	}
	for svc := range in.rules.routes {
		keys[svc] = struct{}{}
	}
	for svc := range keys {
		if ref, ok := serviceref.ParseKey(svc); ok {
			bare[ref.Name]++
		}
	}
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
		n.names = serviceNames(ref, fqdn, n.mesh, bare[ref.Name] == 1)
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
func serviceNames(ref serviceref.ServiceRef, fqdn, mesh string, bareIsUnique bool) []string {
	names := []string{fqdn, mesh, ref.Name + "." + ref.Namespace, ref.Name + "." + ref.Namespace + ".svc"}
	if bareIsUnique {
		names = append(names, ref.Name)
	}
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
		for _, p := range e.ports.http {
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
		out = append(out, proxy.BuildCaptureRouteConfiguration(
			proxy.CaptureHTTPRouteName(p),
			c.captureTableVhosts(in, p),
			proxy.BuildCaptureCatchAllVirtualHost(c.meshDomain, p, isLegacyHTTPPort(p), c.captureRedirectAll),
		))
	}
	out = append(out, proxy.BuildCaptureRouteConfiguration(
		proxy.CaptureHTTPOtherRouteName,
		c.captureTableVhosts(in, 0),
		proxy.BuildCaptureCatchAllVirtualHost(c.meshDomain, 0, false, c.captureRedirectAll),
	))
	c.captureTables = captureTablesMemo{valid: true, key: key, deps: deps, arms: arms, tables: out}
	return out
}

// captureTableVhosts builds the virtual hosts of the table of dialled port
// port (0 for cap_http_other), in name order.
func (c *SnapshotCache) captureTableVhosts(in captureTableInputs, port uint32) []*routev3.VirtualHost {
	vhosts := make([]*routev3.VirtualHost, 0, 2*len(in.names))
	for _, n := range in.names {
		if port != 0 {
			vhosts = c.appendNameVhosts(vhosts, in, n, port)
			continue
		}
		// cap_http_other: HTTP on a port number no service declares as HTTP.
		// A name this node knows the ports of is refused; nothing is routed.
		if n.ports.known() || n.mapped {
			vhosts = append(vhosts, proxy.BuildRefusalVirtualHost(n.mesh+"|undeclared", domains(n.names, "", ":*"), proxy.RefusalPortNotDeclared,
				refusalBody(n.mesh, "does not declare the port that was dialled", n.ports)))
		}
	}
	// in.names is sorted and each name appends in a fixed order, so the
	// table's repeated virtual_hosts field is already deterministic (#135).
	return vhosts
}

// appendNameVhosts appends the virtual hosts of ONE name in the table of
// dialled port d (never 0). See the table at the top of the file.
func (c *SnapshotCache) appendNameVhosts(vhosts []*routev3.VirtualHost, in captureTableInputs, n meshName, d uint32) []*routev3.VirtualHost {
	refuse := func(kind, reason, what string, doms []string) {
		vhosts = append(vhosts, proxy.BuildRefusalVirtualHost(n.mesh+"|"+kind, doms, reason, refusalBody(n.mesh, what, n.ports)))
	}
	// What this node can say about the name's ports. Without it (a service it
	// holds no endpoint of, a route target that is only a name) there is
	// nothing to refuse BY: the name gets a virtual host where it can be
	// routed and none elsewhere.
	informed := n.ports.known() || n.mapped

	exact := fmt.Sprintf(":%d", d)
	pod, declared := n.servicePortFor(d)
	// A Service port routes when its pod port is HTTP, or is not known to be
	// anything else: a name this node holds no endpoint of, and a user
	// Service's port whose targetPort is a NAME (pod 0). The registry carries
	// no port names, so that port cannot be told from a raw-TCP one; it is
	// routed to the service's default cluster with the parent's rules, as it
	// was before the tables were per port, and reported by the reconciler.
	named := declared && pod == 0 && n.mapped
	routable := declared && (named || (pod != 0 && (!n.ports.known() || n.ports.hasHTTP(pod))))
	own := domains(n.names, "", exact)

	switch {
	case routable:
		// A port-less Host means the port that was dialled, port 80 included.
		vhosts = append(vhosts, c.portVhost(in, n, d, pod, own))
	case isLegacyHTTPPort(d):
		vhosts = c.appendLegacyVhost(vhosts, in, n, own)
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
	// decides: the request is refused, never answered by another port.
	if informed || routable || isLegacyHTTPPort(d) {
		refuse("mismatch", proxy.RefusalPortMismatch, fmt.Sprintf("was dialled on port %d with a Host header that names another port", d), domains(n.names, ":*"))
	}
	return vhosts
}

// coldVhost is the virtual host of a name this node knows no port of, in the
// table of a dialled port d that is not a legacy one: the mesh authority that
// spells d, routed to the cluster of that name ("<fqdn>:d"), which the
// on-demand filter fetches. It carries the service's HTTP filters and the
// rules that apply on d.
func (c *SnapshotCache) coldVhost(in captureTableInputs, n meshName, d uint32) *routev3.VirtualHost {
	vh := proxy.BuildServiceVirtualHost(n.mesh+"|"+strconv.Itoa(int(d)), proxy.PortClusterName(n.svc, c.meshDomain, d),
		[]string{fmt.Sprintf("%s:%d", n.mesh, d)}, resolveRules(in.rules, in.all, c.meshDomain, rulePlace{svc: n.svc, servicePort: d}))
	applyChainFilter(vh, in.rules.chainFilters, n.svc)
	return vh
}

// portVhost builds the virtual host that ROUTES the name in the table of
// dialled Service port d, whose pod port is pod: to that port's cluster, with
// the rules that apply to the port, the service's chain filter and, on the
// default cluster's routes, the per-source HTTP/3 selection.
func (c *SnapshotCache) portVhost(in captureTableInputs, n meshName, d, pod uint32, doms []string) *routev3.VirtualHost {
	cluster := n.mesh
	if n.ports.known() && pod != 0 {
		cluster = httpClusterForPort(n.svc, c.meshDomain, n.ports, pod)
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

// appendLegacyVhost appends the virtual host that serves a LEGACY spelling of
// the name (a dial to the mesh HTTP port, or to port 80 of a service that does
// not declare it; proposal 043, "The legacy spellings during Phases 1 and 2").
// It reaches the service's legacy target if that is an HTTP port, and refuses
// otherwise.
func (c *SnapshotCache) appendLegacyVhost(vhosts []*routev3.VirtualHost, in captureTableInputs, n meshName, doms []string) []*routev3.VirtualHost {
	name := n.mesh + "|legacy"
	switch {
	case !n.ports.known() || n.ports.legacyHTTPPort() != 0:
		// The default cluster: the legacy target's, or, for a name this node
		// holds no endpoint of, the cluster the cold path fetches by that
		// name.
		vh := proxy.BuildServiceVirtualHost(name, n.mesh, doms, resolveRules(in.rules, in.all, c.meshDomain, rulePlace{svc: n.svc, pod: n.ports.legacyHTTPPort()}))
		applyChainFilter(vh, in.rules.chainFilters, n.svc)
		if len(n.arms) > 0 {
			proxy.ApplyQUICClusterSelection(vh, n.mesh, n.arms)
		}
		return append(vhosts, vh)
	case n.ports.servesHTTP():
		return append(vhosts, proxy.BuildRefusalVirtualHost(name, doms, proxy.RefusalPortRequired,
			refusalBody(n.mesh, "was addressed without one of its ports, and the port its pods declare is not an HTTP port", n.ports)))
	default:
		return append(vhosts, c.noHTTPPortVhost(name, n.svc, doms, n.ports))
	}
}

// noHTTPPortVhost is the refusal of an HTTP request for a service that serves
// no HTTP port at all (proposal 037): 421 no-http-port, naming where the
// service is reached over raw TCP.
func (c *SnapshotCache) noHTTPPortVhost(name, svc string, doms []string, ports servicePorts) *routev3.VirtualHost {
	body := fmt.Sprintf("aether: %s serves no HTTP port. Reach it over TCP instead: %s\n",
		proxy.ServiceClusterName(svc, c.meshDomain), c.tcpSpellings(svc, ports))
	return proxy.BuildRefusalVirtualHost(name, doms, proxy.RefusalNoHTTPPort, body)
}
