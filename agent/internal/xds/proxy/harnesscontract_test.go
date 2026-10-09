package proxy

import (
	"regexp"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/test/harnesscontract"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
)

// TestExternalHarnessContract_AnyPortStat holds the any-port shim's stat
// prefix to the external-harness contract
// (test/harnesscontract/external-harness.yaml). A harness outside this
// repository reads tcp.<prefix><cluster>.downstream_cx_total by pattern, and a
// counter Envoy never incremented has no series at all: after a rename its
// query would read "no data", which is exactly what a legitimate zero looks
// like. The prefix is taken from a capture listener the real generator built.
func TestExternalHarnessContract_AnyPortStat(t *testing.T) {
	c := harnesscontract.MustLoad(t)
	c.Owns(t, "//agent/internal/xds/proxy:proxy_test", "envoy.capture.anyport_downstream_cx")
	entry := c.EnvoyStat(t, "envoy.capture.anyport_downstream_cx")

	const cluster = "tcp:echo-tcp.a-namespace.aether.internal"
	l, err := GenerateCaptureListener(&cniv1.CNIPod{Name: "p1", NetworkNamespace: "/var/run/netns/p1"},
		"spiffe://aether.internal/ns/default/sa/test", 15001, "aether.internal", false,
		[]CaptureTCPService{{ClusterName: cluster, ClusterIP: "10.96.1.50", PrimaryIsTCP: true, PrimaryPort: 9000}}, true, nil)
	if err != nil {
		t.Fatalf("GenerateCaptureListener: %v", err)
	}
	var portless []string // the stat prefixes of the chains with no destination_port
	for _, fc := range l.GetFilterChains() {
		if fc.GetFilterChainMatch().GetDestinationPort().GetValue() != 0 {
			continue
		}
		for _, f := range fc.GetFilters() {
			tc := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig() != nil && f.GetTypedConfig().UnmarshalTo(tc) == nil {
				portless = append(portless, tc.GetStatPrefix())
			}
		}
	}
	want := entry.StatPrefix + cluster
	found := false
	for _, p := range portless {
		found = found || p == want
	}
	if !found {
		harnesscontract.Errorf(t, "%s says the any-port chain of a TCP-primary service counts under the stat prefix %q + the cluster name (%q); the portless chains of the generated capture listener have %q.",
			harnesscontract.File, entry.StatPrefix, want, portless)
	}

	// The rest of the entry is what Envoy and a metrics pipeline make of the
	// prefix. Not something this repository can run, but the three spellings
	// in the entry must at least agree with one another.
	if wantStat := "tcp." + entry.StatPrefix + "<cluster>.downstream_cx_total"; entry.Stat != wantStat {
		harnesscontract.Errorf(t, "%s: the stat of %s is %q, and its stat_prefix makes it %q.", harnesscontract.File, entry.ID, entry.Stat, wantStat)
	}
	flat := "envoy_" + regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString("tcp."+want+".downstream_cx_total", "_")
	pattern, err := regexp.Compile("^(?:" + entry.StoredNamePattern + ")$")
	if err != nil {
		t.Fatalf("%s: stored_name_pattern of %s is not a regular expression: %v", harnesscontract.File, entry.ID, err)
	}
	if !pattern.MatchString(flat) || !strings.Contains(entry.StoredNamePattern, strings.TrimSuffix(entry.StatPrefix, "_")) {
		harnesscontract.Errorf(t, "%s: stored_name_pattern %q of %s does not match %q, the flattened name of the stat for cluster %q.", harnesscontract.File, entry.StoredNamePattern, entry.ID, flat, cluster)
	}
}
