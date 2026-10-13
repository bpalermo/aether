package cache

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
)

// loopbackGaps is the property the loopback table is built to have, checked
// on the snapshot the cache holds now: every mesh service the node CLAIMS
// (its ports are known from registry rows, or its generated Service declares
// ports, or a service-wide filter or a route names it) answers every one of
// its mesh authorities from a virtual host of its own, never from the
// catch-all. The catch-all's on-demand route runs neither a service's filter
// nor its routes, so an authority that falls to it is a request delivered
// without them. It returns one line per authority that does.
func loopbackGaps(t *testing.T, c *SnapshotCache) []string {
	t.Helper()
	rules := ruleInputs{routes: c.serviceRoutesSnapshot(), chainFilters: c.serviceChainFiltersSnapshot(), portMaps: c.servicePortMapsSnapshot(), declared: c.declaredByService()}
	c.clusterMu.RLock()
	claimed := map[string]loopbackSource{}
	names := map[string]struct{}{}
	for svc := range c.servicePorts {
		names[svc] = struct{}{}
	}
	for svc := range rules.declared {
		names[svc] = struct{}{}
	}
	for svc := range rules.chainFilters {
		names[svc] = struct{}{}
	}
	for svc := range rules.routes {
		names[svc] = struct{}{}
	}
	for svc := range names {
		if src := c.loopbackSourceLocked(svc, rules); src != loopbackUnknown {
			claimed[svc] = src
		}
	}
	mesh := c.meshDomain
	c.clusterMu.RUnlock()

	tables := a043Tables(t, c)
	var gaps []string
	for _, svc := range slices.Sorted(maps.Keys(claimed)) {
		fqdn := proxy.ServiceClusterName(svc, mesh)
		if fqdn == "" {
			continue
		}
		hosts := []string{fqdn}
		for _, p := range []uint32{80, 7000, 8000, 8080, 9000, 9090, 18081, 18082} {
			hosts = append(hosts, fmt.Sprintf("%s:%d", fqdn, p))
		}
		for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
			table := tables[tname]
			if table == nil {
				continue
			}
			for _, host := range hosts {
				if vh := r3Pick(t, table, host); vh.GetName() == "on_demand_catch_all" {
					gaps = append(gaps, fmt.Sprintf("%s (source %d): table %s, Host %s is answered by the catch-all", svc, claimed[svc], tname, host))
				}
			}
		}
	}
	return gaps
}

// TestLoopbackSource_OnePerService: the one classifier every builder of the
// loopback table asks. Rows the node holds outrank the generated Service,
// which outranks "only a filter or a route names it"; a name with none of the
// three is not the table's.
func TestLoopbackSource_OnePerService(t *testing.T) {
	c := newTestCache("node-1")
	held := servicePorts{http: []uint32{8080}}
	c.servicePorts = map[string]servicePorts{"ns/held": held, "ns/held-declared": held, "ns/reserved": {reservedOnly: true}, "ns/empty": {}}
	in := ruleInputs{
		declared:     map[string]servicePorts{"ns/held-declared": {http: []uint32{9090}}, "ns/declared": {http: []uint32{9090}}, "ns/declared-filtered": {tcp: []uint32{9000}}},
		chainFilters: map[string]proxy.ExtensionFilter{"ns/declared-filtered": {}, "ns/filtered": {}, "ns/held": {}},
		routes:       map[string][]proxy.GammaRoute{"ns/routed": {{}}, "ns/no-rules": nil},
	}
	for svc, want := range map[string]loopbackSource{
		"ns/held": loopbackHeld, "ns/held-declared": loopbackHeld, "ns/reserved": loopbackHeld,
		"ns/declared": loopbackDeclared, "ns/declared-filtered": loopbackDeclared,
		"ns/filtered": loopbackCold, "ns/routed": loopbackCold,
		"ns/empty": loopbackUnknown, "ns/no-rules": loopbackUnknown, "ns/nothing": loopbackUnknown,
	} {
		assert.Equalf(t, want, c.loopbackSourceLocked(svc, in), "%s", svc)
	}
}
