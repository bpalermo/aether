package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterConstructors is every function of this package that builds a
// cluster, with the names the clusters it builds are published under, split
// by where the agent's pin gauges put them: a mesh cluster entry's name (the
// gauges count it) or a name of a family they do not count.
//
// It is the enumeration of the cluster families the node agent and the edge
// can publish. A constructor is added here together with its names, and its
// names are then held to exactly one of IsMeshEntryClusterName and
// ClusterNameOutsidePinGauge.
func clusterConstructors() map[string]struct{ entry, outside []string } {
	const domain = "aether.internal"
	pod := &cniv1.CNIPod{Name: "web-0", Namespace: "demo"}
	fqdn := ServiceClusterName("demo/web", domain)
	floor := TCPClusterName("demo/web", domain)
	return map[string]struct{ entry, outside []string }{
		// The HTTP cluster of a service, its per-port clusters and port
		// aliases; a QUIC twin is cloned from one.
		"NewServiceCluster": {
			entry:   []string{fqdn, PortClusterName("demo/web", domain, 8080)},
			outside: []string{QUICClusterName("demo/web", domain, "demo/client")},
		},
		"NewTCPServiceCluster":             {entry: []string{floor, TCPPortClusterName(floor, 5432)}},
		"NewUDPServiceCluster":             {outside: []string{UDPClusterName("demo/web", domain)}},
		"NewAppCluster":                    {outside: []string{AppClusterName(pod, 8080), HealthProbeClusterName(pod)}},
		"NewInboundReadyProbeCluster":      {outside: []string{InboundReadyClusterName(pod)}},
		"NewPassthroughOriginalDstCluster": {outside: []string{PassthroughClusterName}},
		"NewBlackholeCluster":              {outside: []string{BlackholeClusterName}},
		"BuildWaypointIngressCluster":      {outside: []string{WaypointIngressClusterName(fqdn)}},
		"BuildEdgeK8sCluster":              {outside: []string{EdgeK8sClusterName("demo", "web", 8080)}},
	}
}

// TestEveryClusterConstructorIsOfAClassifiedFamily is the gate on the two
// name predicates the agent reads a proxy's statement with
// (cache.ackedPins.restateLocked): a cluster the proxy holds and the agent no
// longer publishes is taken for a mesh cluster of unknown pin state unless its
// name says the pin gauges do not count it. A cluster family that neither
// predicate knows would withdraw the acknowledged pin gauge for a cluster the
// gauge never counts; one that both knew would be left out of the count.
//
// Two halves. Every function of this package that builds a clusterv3.Cluster
// is in clusterConstructors (found by scanning the package's source, so a new
// constructor fails here until it is listed). And every name listed is
// classified by exactly one predicate, the one it is listed under.
func TestEveryClusterConstructorIsOfAClassifiedFamily(t *testing.T) {
	const domain = "aether.internal"
	listed := clusterConstructors()

	entries, err := proxySources.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var found []string
	files := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files++
		src, err := proxySources.ReadFile(e.Name())
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, e.Name(), src, 0)
		require.NoError(t, err, e.Name())
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			builds := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cluster" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "clusterv3" {
						builds = true
					}
				}
				return true
			})
			if builds {
				found = append(found, fn.Name.Name)
			}
		}
	}
	require.NotZero(t, files, "the embed must see this package's sources, or the scan is vacuous")
	slices.Sort(found)
	want := make([]string, 0, len(listed))
	for name := range listed {
		want = append(want, name)
	}
	slices.Sort(want)
	require.Equal(t, want, found,
		"the functions of this package that build a clusterv3.Cluster are not the ones clusterConstructors lists: "+
			"list the new one with the names its clusters are published under, and classify a new family in clusterfamily.go")

	for constructor, names := range listed {
		require.NotEmpty(t, append(slices.Clone(names.entry), names.outside...), constructor)
		for _, name := range names.entry {
			require.NotEmpty(t, name, constructor)
			assert.True(t, IsMeshEntryClusterName(name, domain), "%s: %q is a mesh cluster entry's name", constructor, name)
			assert.False(t, ClusterNameOutsidePinGauge(name), "%s: %q is a name the pin gauges count", constructor, name)
		}
		for _, name := range names.outside {
			require.NotEmpty(t, name, constructor)
			assert.True(t, ClusterNameOutsidePinGauge(name), "%s: %q is of a family the pin gauges do not count", constructor, name)
			assert.False(t, IsMeshEntryClusterName(name, domain), "%s: %q is not a mesh cluster entry's name", constructor, name)
		}
	}

	// A name of no family is neither: the agent then errs to "unknown".
	assert.False(t, ClusterNameOutsidePinGauge("some_future_cluster"))
	assert.False(t, IsMeshEntryClusterName("some_future_cluster", domain))
	// And a mesh entry of another mesh domain is not this mesh's.
	assert.False(t, IsMeshEntryClusterName(ServiceClusterName("demo/web", "other.internal"), domain))
}
