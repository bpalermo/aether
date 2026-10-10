package proxy

import (
	"slices"
	"strings"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// Issue #1636: a pod name is an RFC 1123 SUBDOMAIN, so it may contain dots,
// and a dot is Envoy's stat-name separator. A stat name built from the pod
// name as it is ("cluster.health_team-a_web.external-7d9f.membership_healthy")
// has more segments than the chart's stats exclusions and tag extractors
// expect: they read everything up to the FIRST dot as the cluster or the
// listener. For this pod the exclusion `health_[^.]*\.(…|external|…).*` then
// swallows the membership gauges the health gateway reads, and the pod's
// endpoint is reported unhealthy.

func dottedPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "web.external-7d9f-abcde",
		Namespace:        "team-a",
		ServiceAccount:   "web",
		NetworkNamespace: "/var/run/netns/cni-dddd",
		Uid:              "33333333-3333-3333-3333-333333333333",
		// A raw-TCP secondary port, for the inbound listener's per-port
		// tcp_proxy chain and its stat prefix.
		Annotations: map[string]string{aetherannotations.AnnotationEndpointPorts: "8080,9000=tcp"},
	}
}

// perPodResources builds everything the generators derive from one pod.
func perPodResources(t *testing.T, pod *cniv1.CNIPod) (listeners []*listenerv3.Listener, clusters []*clusterv3.Cluster) {
	t.Helper()
	inbound, outbound, apps, health, err := GenerateListenersFromRegistryPod(pod, sameNameTrustDomain, sameNameMeshDomain, false, false, nil, nil, "")
	require.NoError(t, err)
	quic, err := NewInboundQUICListener(pod, sameNameTrustDomain, sameNameMeshDomain, false, false, nil, nil)
	require.NoError(t, err)
	capture, err := GenerateCaptureListener(pod, SpiffeIDFromPod(pod, sameNameTrustDomain), 15001, sameNameMeshDomain, false, nil, false, nil)
	require.NoError(t, err)
	udp, err := GenerateUDPCaptureListener(pod, 18082,
		map[string][]L4Backend{"ns-a/dns": {{Service: "ns-a/dns", Cluster: "udp:dns.ns-a.mesh.internal", Weight: 1}}},
		map[string]string{"ns-a/dns": "10.96.0.53"})
	require.NoError(t, err)
	require.NotNil(t, udp)
	ready := NewInboundReadyProbeCluster(InboundReadyClusterName(pod), pod.GetNetworkNamespace(),
		"spiffe://aether.internal/node", "ROOTCA", SpiffeIDFromPod(pod, sameNameTrustDomain))

	return []*listenerv3.Listener{inbound, outbound, quic, capture, udp}, append(append([]*clusterv3.Cluster{}, apps...), health, ready)
}

// statPrefixesOf returns every stat_prefix string in the message, at any
// depth and through every packed Any: the listener's own, its HTTP connection
// managers', its tcp_proxy and udp_proxy filters'.
func statPrefixesOf(t *testing.T, root proto.Message) []string {
	t.Helper()
	var out []string
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		if packed, ok := m.Interface().(*anypb.Any); ok {
			inner, err := anypb.UnmarshalNew(packed, proto.UnmarshalOptions{})
			require.NoErrorf(t, err, "unpacking %s", packed.GetTypeUrl())
			walk(inner.ProtoReflect())
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.Kind() == protoreflect.StringKind && !fd.IsList() && fd.Name() == "stat_prefix":
				out = append(out, v.String())
			case fd.IsMap():
				if fd.MapValue().Message() != nil {
					v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
						walk(mv.Message())
						return true
					})
				}
			case fd.IsList():
				if fd.Message() != nil {
					for i := 0; i < v.List().Len(); i++ {
						walk(v.List().Get(i).Message())
					}
				}
			case fd.Message() != nil:
				walk(v.Message())
			}
			return true
		})
	}
	walk(root.ProtoReflect())
	return out
}

// clusterStatName is the name Envoy keys a cluster's stats by.
func clusterStatName(c *clusterv3.Cluster) string {
	if c.GetAltStatName() != "" {
		return c.GetAltStatName()
	}
	return c.GetName()
}

// TestPodStatKey pins the spelling of a pod in a stat name: the resource key
// with every dot of the pod name replaced by '~', which is legal in neither a
// namespace nor a pod name, so the mapping can be read back and two different
// pods never share a stat name.
func TestPodStatKey(t *testing.T) {
	assert.Equal(t, "ns-a_web-0", PodStatKey(sameNamePodA()), "a pod name with no dot is spelled as in its resource names")
	assert.Equal(t, PodResourceKey(sameNamePodA()), PodStatKey(sameNamePodA()))
	assert.Equal(t, "team-a_web~external-7d9f-abcde", PodStatKey(dottedPod()))
	assert.Equal(t, "team-a_a~b~c", PodStatKey(&cniv1.CNIPod{Namespace: "team-a", Name: "a.b.c"}))

	// Read back: '~' -> '.' is the pod name again.
	_, name, ok := strings.Cut(PodStatKey(dottedPod()), "_")
	require.True(t, ok)
	assert.Equal(t, dottedPod().GetName(), strings.ReplaceAll(name, "~", "."))

	// Pods that differ only in where the dot is do not collide, with each
	// other or with a pod that has a '-' there.
	keys := map[string]string{}
	for _, name := range []string{"a.b-c", "a-b.c", "a-b-c", "a.b.c"} {
		key := PodStatKey(&cniv1.CNIPod{Namespace: "n", Name: name})
		assert.NotContains(t, key, ".")
		if other, dup := keys[key]; dup {
			t.Errorf("pods %q and %q share the stat key %q", other, name, key)
		}
		keys[key] = name
	}
}

// TestPerPodStatNamesCarryNoPodDot is the stat-name inventory, the sibling of
// TestPerPodResourceNamesCarryTheNamespace: with the real generators, no stat
// name derived from a pod contains a dot of the pod's name, and the RESOURCE
// names are what they were (#1634 named them; this change renames nothing
// Envoy or go-control-plane keys a resource by).
func TestPerPodStatNamesCarryNoPodDot(t *testing.T) {
	pod := dottedPod()
	const resKey, statKey = "team-a_web.external-7d9f-abcde", "team-a_web~external-7d9f-abcde"
	listeners, clusters := perPodResources(t, pod)

	// Per listener: its own stat prefix first, then every nested one (the
	// HTTP connection managers' are constants).
	wantPrefixes := map[string][]string{
		"inbound_" + resKey:         {"inbound_" + statKey, "in_tcp_" + statKey, "in_tcp_" + statKey + "_9000", "inbound"},
		"outbound_http_" + resKey:   {"out_http_" + statKey, "outbound_http"},
		"inbound_" + resKey + "_h3": {"inbound_" + statKey + "_h3", "inbound"},
		"capture_" + resKey:         {"capture_" + statKey, "capture_http", "cap_tcp_blackhole"},
		"capture_udp_" + resKey:     {"capture_udp_" + statKey},
	}
	require.Len(t, listeners, len(wantPrefixes))
	for _, l := range listeners {
		want, ok := wantPrefixes[l.GetName()]
		require.Truef(t, ok, "listener %q: resource names are unchanged by #1636", l.GetName())
		got := statPrefixesOf(t, l)
		for _, p := range got {
			assert.NotContainsf(t, p, ".", "listener %s: stat prefix %q carries a dot of the pod name", l.GetName(), p)
		}
		assert.Equalf(t, want[0], l.GetStatPrefix(), "listener %s", l.GetName())
		slices.Sort(got)
		got = slices.Compact(got)
		want = slices.Clone(want)
		slices.Sort(want)
		assert.Equalf(t, want, got, "listener %s: its stat prefixes, at every depth", l.GetName())
	}

	wantClusters := map[string]string{
		"app_" + resKey + "_8080": "app",
		"app_" + resKey + "_9000": "app",
		"health_" + resKey:        "health_" + statKey,
		"inboundready_" + resKey:  "inboundready_" + statKey,
	}
	require.Len(t, clusters, len(wantClusters))
	for _, c := range clusters {
		want, ok := wantClusters[c.GetName()]
		require.Truef(t, ok, "cluster %q: resource names are unchanged by #1636", c.GetName())
		assert.Equalf(t, want, clusterStatName(c), "cluster %s: the name its stats are keyed by", c.GetName())
		assert.Equal(t, c.GetName(), c.GetLoadAssignment().GetClusterName())
	}

	// The health gateway is configured by cluster NAME, and so are its paths:
	// what the agent's liveness loop requests does not change.
	gateway := BuildHealthGatewayListener("/run/aether/health.sock", []HealthGatewayProbe{
		NewHealthGatewayProbe(HealthProbeClusterName(pod), InboundReadyClusterName(pod)),
	})
	assert.Contains(t, gateway.String(), "/healthz/health_"+resKey)
	assert.Contains(t, gateway.String(), "/healthz/inboundready_"+resKey)
	assert.NotContains(t, gateway.String(), "~")
}

// TestPodWithoutADotKeepsItsStatNames is the guard for every pod whose name
// has no dot, which is every pod a workload controller names: its stat
// prefixes are what they were and its probe clusters set no alt_stat_name, so
// an agent upgrade changes none of its listeners or clusters and Envoy
// re-creates nothing.
func TestPodWithoutADotKeepsItsStatNames(t *testing.T) {
	pod := sameNamePodA()
	listeners, clusters := perPodResources(t, pod)

	want := map[string]string{
		"inbound_ns-a_web-0":       "inbound_ns-a_web-0",
		"outbound_http_ns-a_web-0": "out_http_ns-a_web-0",
		"inbound_ns-a_web-0_h3":    "inbound_ns-a_web-0_h3",
		"capture_ns-a_web-0":       "capture_ns-a_web-0",
		"capture_udp_ns-a_web-0":   "capture_udp_ns-a_web-0",
	}
	require.Len(t, listeners, len(want))
	for _, l := range listeners {
		assert.Equalf(t, want[l.GetName()], l.GetStatPrefix(), "listener %s", l.GetName())
		for _, p := range statPrefixesOf(t, l) {
			assert.NotContainsf(t, p, "~", "listener %s: stat prefix %q", l.GetName(), p)
		}
	}
	for _, c := range clusters {
		if strings.HasPrefix(c.GetName(), "app_") {
			assert.Equal(t, "app", c.GetAltStatName())
			continue
		}
		assert.Emptyf(t, c.GetAltStatName(),
			"cluster %s: no alt_stat_name when the name already is a valid stat name; setting one would change the cluster on every node at upgrade", c.GetName())
	}
}
