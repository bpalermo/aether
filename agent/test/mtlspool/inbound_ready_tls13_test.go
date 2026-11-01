package mtlspool

// The per-pod inbound-readiness probe, live, on TLS 1.2 and TLS 1.3 (proposal
// 045: envoyproxy/envoy#48570 makes TLS 1.3 the upstream default on the
// 1.41-dev pin).
//
// What the probe must mean: the pod's mTLS inbound is listening, presents a
// certificate that verifies as THIS pod, AND has accepted the probe's client
// certificate (the node SVID). Under TLS 1.2 the client sees the server's
// rejection of its certificate inside the handshake. Under TLS 1.3 it does not:
// the client's handshake completes after it SENDS its certificate, and the
// server's rejection arrives afterwards as an alert, i.e. a reset of an
// established connection. A connect-only TCP health check passes on the
// Connected event, so under TLS 1.3 it passes against an inbound that rejects
// it (the upstream release note of #48570 says exactly that).
//
// The harness: a destination Envoy runs the pod's inbound listener as the agent
// builds it (proxy.NewInboundListener, moved onto a loopback port, SDS repointed
// at the harness). A probe Envoy runs the probe clusters as the agent builds them
// (proxy.NewInboundReadyProbeCluster, endpoint repointed at that listener, no
// netns bind, SDS repointed), one per case, with the TLS version forced on the
// probe's own context. The inbound pins the client's URI SAN to the workload
// shape spiffe://<td>/ns/ (#843), so a node SVID outside that shape is the
// rejection: spiffeNode (spiffe://<td>/node/test-node). The accepted node
// identity is the one the mesh really has, the agent's own workload SVID.
//
// The control case runs the OLD probe shape (connect-only TCP) under TLS 1.3
// against the rejecting inbound and requires it to report HEALTHY: that is the
// hole, and seeing it here is what shows the harness can tell the two apart.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// spiffeAgentNode is the node identity the mesh really has: the agent is an
// ordinary pod, so its SVID is a /ns/ workload identity, which the inbound's
// client SAN pin accepts (see TestInboundReadyProbeIsAcceptedByInboundValidation).
const spiffeAgentNode = "spiffe://" + trustDomain + "/ns/aether-system/sa/aether-agent"

// readyCase is one probe cluster in the probe Envoy.
type readyCase struct {
	name        string
	node        string                          // the client SVID the probe presents
	tls         tlsv3.TlsParameters_TlsProtocol // forced as both minimum and maximum
	connectOnly bool                            // the pre-change probe shape (the control)
	wantHealthy bool
}

func TestInboundReadyProbe_RequiresTheInboundToAcceptTheNodeIdentity(t *testing.T) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("locate envoy: %v", err)
	}

	p := newPKI(t)
	sdsAddr := startReadySDS(t, p)

	// The destination: the pod's inbound listener exactly as the agent builds it.
	pod := &cniv1.CNIPod{
		Name: "echo-0", Namespace: "demo", ServiceAccount: "echo",
		NetworkNamespace: "/var/run/netns/echo-0", ContainerId: "c-echo-0", Ips: []string{"10.0.0.7"},
	}
	inbound := productionInbound(t, pod, socketAddress("127.0.0.1", envoyPicksPort), true, false)
	dest := launchEnvoy(t, bin, "destination", &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters:  []*clusterv3.Cluster{sdsCluster(sdsAddr)},
			Listeners: []*listenerv3.Listener{inbound},
		},
	}, nil, "--concurrency", "1")
	inboundAddr := dest.listenerAddr(t, inbound.GetName())

	cases := []readyCase{
		{name: "tls13_rejected", node: spiffeNode, tls: tlsv3.TlsParameters_TLSv1_3, wantHealthy: false},
		{name: "tls12_rejected", node: spiffeNode, tls: tlsv3.TlsParameters_TLSv1_2, wantHealthy: false},
		{name: "tls13_accepted", node: spiffeAgentNode, tls: tlsv3.TlsParameters_TLSv1_3, wantHealthy: true},
		{name: "tls12_accepted", node: spiffeAgentNode, tls: tlsv3.TlsParameters_TLSv1_2, wantHealthy: true},
		// The control: the old connect-only probe passes against an inbound
		// that rejects it, under TLS 1.3. If this ever reports unhealthy, the
		// harness no longer reproduces the hole and the cases above prove
		// nothing.
		{name: "tls13_rejected_connect_only_control", node: spiffeNode, tls: tlsv3.TlsParameters_TLSv1_3, connectOnly: true, wantHealthy: true},
	}

	clusters := []*clusterv3.Cluster{sdsCluster(sdsAddr)}
	for _, c := range cases {
		clusters = append(clusters, readyProbeCluster(t, c, pod, inboundAddr))
	}
	probe := launchEnvoy(t, bin, "probe", &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Clusters: clusters},
	}, nil, "--concurrency", "1")

	// Every case gets at least three checks (5 s apart, the production cadence)
	// before anything is judged: one check is enough to pass, two failures mark
	// a host unhealthy, and a third proves the verdict is stable.
	for _, c := range cases {
		probe.await(t, func() string { return "run three health checks for " + c.name }, func() bool {
			return adminCounter(t, probe.admin, "cluster."+readyClusterName(c)+".health_check.attempt") >= 3
		})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prefix := "cluster." + readyClusterName(c) + "."
			healthy := adminCounter(t, probe.admin, prefix+"membership_healthy")
			success := adminCounter(t, probe.admin, prefix+"health_check.success")
			failure := adminCounter(t, probe.admin, prefix+"health_check.failure")
			t.Logf("%s: membership_healthy=%d success=%d failure=%d", c.name, healthy, success, failure)
			if c.wantHealthy {
				require.Equal(t, 1, healthy, "the probe must report the inbound ready")
				require.Positive(t, success)
				return
			}
			require.Equal(t, 0, healthy,
				"an inbound that rejects the node identity must never be reported ready (TLS %s)", c.tls)
			require.Zero(t, success, "not a single check may pass against a rejecting inbound")
			require.GreaterOrEqual(t, failure, 2)
		})
	}

	// The rejection is real: the destination refused client certificates on
	// the SAN pin, which is what the rejected cases were up against.
	require.Positive(t, adminCounterSum(t, dest.admin, "ssl.fail_verify_san"),
		"the destination must have rejected the node identity on its client SAN pin")
}

// readyClusterName is the probe cluster's name for a case. Stats are keyed by
// it: the probe cluster keeps per-pod stats (an empty alt_stat_name).
func readyClusterName(c readyCase) string { return "inboundready_" + c.name }

// readyProbeCluster is the probe cluster as the agent builds it, with the three
// substitutions running outside a pod needs: the endpoint is the destination's
// loopback listener, there is no network-namespace bind, and SDS points at the
// harness. Then the case's TLS version is forced on the probe's own context,
// and the control swaps in the old connect-only health check.
func readyProbeCluster(t *testing.T, c readyCase, pod *cniv1.CNIPod, inboundAddr string) *clusterv3.Cluster {
	t.Helper()
	cl := proxy.NewInboundReadyProbeCluster(readyClusterName(c), pod.GetNetworkNamespace(), c.node,
		validationContextName, proxy.SpiffeIDFromPod(pod, trustDomain))
	require.Empty(t, cl.GetAltStatName(), "the probe cluster keeps per-pod stats")

	host, port, err := net.SplitHostPort(inboundAddr)
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	cl.LoadAssignment = &endpointv3.ClusterLoadAssignment{
		ClusterName: cl.GetName(),
		Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, n)}},
		}}}},
	}
	cl.UpstreamBindConfig = nil

	rewriteSDSToHarness(t, cl.GetTransportSocket())
	var ctx tlsv3.UpstreamTlsContext
	require.NoError(t, cl.GetTransportSocket().GetTypedConfig().UnmarshalTo(&ctx))
	ctx.CommonTlsContext.TlsParams = &tlsv3.TlsParameters{
		TlsMinimumProtocolVersion: c.tls,
		TlsMaximumProtocolVersion: c.tls,
	}
	cl.TransportSocket.ConfigType = &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(&ctx)}

	if c.connectOnly {
		require.Len(t, cl.GetHealthChecks(), 1)
		cl.HealthChecks[0].HealthChecker = &corev3.HealthCheck_TcpHealthCheck_{TcpHealthCheck: &corev3.HealthCheck_TcpHealthCheck{}}
	}
	return cl
}

// startReadySDS serves both node identities as client certificates, the
// destination pod's server certificate, and the trust bundle.
func startReadySDS(t *testing.T, p *pki) string {
	t.Helper()
	secrets := secretResources(t, p, []string{spiffeAgentNode, spiffeNode})
	certPath, keyPath := p.leaf(t, "ready-dest", spiffeDest, true)
	secrets = append(secrets, &tlsv3.Secret{
		Name: spiffeDest,
		Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: fileDataSource(certPath), PrivateKey: fileDataSource(keyPath),
		}},
	})
	snapshot, err := cachev3.NewSnapshot("1", map[resourcev3.Type][]types.Resource{resourcev3.SecretType: secrets})
	require.NoError(t, err)
	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	require.NoError(t, cache.SetSnapshot(context.Background(), envoyNodeID, snapshot))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), cache, nil))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	return ln.Addr().String()
}

// adminStats returns the admin /stats lines whose name contains filter.
func adminStats(t *testing.T, admin, filter string) map[string]string {
	t.Helper()
	out := map[string]string{}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + admin + "/stats?filter=" + url.QueryEscape(filter))
	if err != nil {
		t.Logf("read admin stats: %v", err)
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Logf("read admin stats body: %v", err)
		return out
	}
	for _, line := range strings.Split(string(body), "\n") {
		name, value, ok := strings.Cut(line, ": ")
		if ok {
			out[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return out
}

// adminCounter reads one counter or gauge by its exact name; -1 if absent.
func adminCounter(t *testing.T, admin, name string) int {
	t.Helper()
	v, ok := adminStats(t, admin, "^"+strings.ReplaceAll(name, ".", `\.`)+"$")[name]
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

// adminCounterSum sums every counter whose name contains substr.
func adminCounterSum(t *testing.T, admin, substr string) int {
	t.Helper()
	sum := 0
	for _, v := range adminStats(t, admin, substr) {
		if n, err := strconv.Atoi(v); err == nil {
			sum += n
		}
	}
	return sum
}
