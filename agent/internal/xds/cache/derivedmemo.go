package cache

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/proto"
)

// derivedMemo memoizes the resources every snapshot DERIVES from cluster-cache
// entries (#1115): the load assignments republished under a port alias's or a
// QUIC twin's own EDS name (proxy.LoadAssignmentAlias, aether#1013), and the
// QUIC twin clusters themselves (proxy.QUICClusterFrom).
//
// Both used to be built fresh on every snapshot, so they were never the same
// object as the previous build's and the #1105 version memo re-marshalled and
// re-hashed every one of them on every build -- the alias copies alone are two
// thirds of a node's load assignments at the #1105 benchmark shape.
//
// Each is a pure function of its arguments: the base proto OBJECT (never
// mutated once built -- the #1105 invariant, enforced by the version-memo
// audit -- so the pointer stands for its bytes) plus plain values. So the
// previous object is returned whenever every argument is the same; any change
// to the base builds a new base and therefore a new derived object. In this
// package's tests every hit is rebuilt fresh and compared (registryReuseAudit).
//
// Two generations bound the memory: lookups promote from the previous
// generation, and rotate (once per snapshot) drops whatever the last snapshot
// did not ask for.
type derivedMemo struct {
	mu                sync.Mutex
	aliases, aliasOld map[string]aliasCLA
	twins, twinsOld   map[string]twinCluster
}

type aliasCLA struct {
	base, out *endpointv3.ClusterLoadAssignment
}

// twinInputs is every argument of proxy.QUICClusterFrom but the name (the
// memo's key).
type twinInputs struct {
	base                              *clusterv3.Cluster
	clientSpiffeID, validationContext string
	sanURIs                           []string
	sni                               string
	idleTimeout                       time.Duration
}

func (a twinInputs) equal(b twinInputs) bool {
	return a.base == b.base && a.clientSpiffeID == b.clientSpiffeID && a.validationContext == b.validationContext &&
		slices.Equal(a.sanURIs, b.sanURIs) && a.sni == b.sni && a.idleTimeout == b.idleTimeout
}

type twinCluster struct {
	in  twinInputs
	out *clusterv3.Cluster
}

// rotate starts a new generation; call once per snapshot.
func (m *derivedMemo) rotate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aliasOld, m.aliases = m.aliases, make(map[string]aliasCLA, len(m.aliases))
	m.twinsOld, m.twins = m.twins, make(map[string]twinCluster, len(m.twins))
}

// aliasCLA returns proxy.LoadAssignmentAlias(base, name), reusing the previous
// object for the same base object and name.
func (m *derivedMemo) aliasCLA(base *endpointv3.ClusterLoadAssignment, name string) *endpointv3.ClusterLoadAssignment {
	if base == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.aliases[name]
	if !ok || e.base != base {
		e, ok = m.aliasOld[name]
		ok = ok && e.base == base
	}
	switch {
	case !ok:
		e = aliasCLA{base: base, out: proxy.LoadAssignmentAlias(base, name)}
	case registryReuseAudit:
		if fresh := proxy.LoadAssignmentAlias(base, name); !proto.Equal(fresh, e.out) {
			panic(fmt.Sprintf("derived memo: alias load assignment %q is STALE (#1115)", name))
		}
	}
	if m.aliases == nil {
		m.aliases = make(map[string]aliasCLA)
	}
	m.aliases[name] = e
	return e.out
}

// twinCluster returns proxy.QUICClusterFrom(...), reusing the previous object
// when every argument is the same.
func (m *derivedMemo) twinCluster(base *clusterv3.Cluster, name, clientSpiffeID, validationContextName string, sanURIs []string, sni string, idleTimeout time.Duration) *clusterv3.Cluster {
	in := twinInputs{base: base, clientSpiffeID: clientSpiffeID, validationContext: validationContextName, sanURIs: sanURIs, sni: sni, idleTimeout: idleTimeout}
	build := func() *clusterv3.Cluster {
		return proxy.QUICClusterFrom(base, name, clientSpiffeID, validationContextName, sanURIs, sni, idleTimeout)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.twins[name]
	if !ok || !e.in.equal(in) {
		e, ok = m.twinsOld[name]
		ok = ok && e.in.equal(in)
	}
	switch {
	case !ok:
		e = twinCluster{in: in, out: build()}
	case registryReuseAudit:
		if !proto.Equal(build(), e.out) {
			panic(fmt.Sprintf("derived memo: QUIC twin cluster %q is STALE (#1115)", name))
		}
	}
	if m.twins == nil {
		m.twins = make(map[string]twinCluster)
	}
	m.twins[name] = e
	return e.out
}
