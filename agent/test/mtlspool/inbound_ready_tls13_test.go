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
	"crypto/tls"
	"crypto/x509"
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
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
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
	answers503  bool                            // dial the inbound whose liveness answer is a 503
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
	// The same inbound, answering the probe with a 503 instead of a 200: what
	// an overloaded inbound does (stop_accepting_requests replies 503 before
	// the health_check filter runs). Here the liveness filter itself is made to
	// fail, by conditioning it on a cluster that does not exist; either way the
	// 503 is the inbound's own response, sent after it accepted the client.
	inbound503 := productionInbound(t, pod, socketAddress("127.0.0.1", envoyPicksPort), true, false)
	inbound503.Name = inbound.GetName() + "_503"
	eachHCM(t, inbound503, func(h *hcmv3.HttpConnectionManager) {
		for _, f := range h.GetHttpFilters() {
			if f.GetName() != "envoy.filters.http.health_check.live" {
				continue
			}
			var hc healthcheckv3.HealthCheck
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(&hc))
			hc.ClusterMinHealthyPercentages = map[string]*typev3.Percent{"no_such_cluster": {Value: 100}}
			f.ConfigType = &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&hc)}
		}
	})
	dest := launchEnvoy(t, bin, "destination", &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters:  []*clusterv3.Cluster{sdsCluster(sdsAddr)},
			Listeners: []*listenerv3.Listener{inbound, inbound503},
		},
	}, nil, "--concurrency", "1")
	inboundAddr := dest.listenerAddr(t, inbound.GetName())
	inbound503Addr := dest.listenerAddr(t, inbound503.GetName())

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
		// An inbound that answers 503 (an overloaded one) has still accepted the
		// node identity: ready. Readiness must not follow overload, or every
		// pod on an overloaded node is demoted at once. A rejected identity
		// stays a reset, so it still fails against the same inbound.
		{name: "tls13_accepted_inbound_503", node: spiffeAgentNode, tls: tlsv3.TlsParameters_TLSv1_3, answers503: true, wantHealthy: true},
		{name: "tls12_accepted_inbound_503", node: spiffeAgentNode, tls: tlsv3.TlsParameters_TLSv1_2, answers503: true, wantHealthy: true},
		{name: "tls13_rejected_inbound_503", node: spiffeNode, tls: tlsv3.TlsParameters_TLSv1_3, answers503: true, wantHealthy: false},
	}

	clusters := []*clusterv3.Cluster{sdsCluster(sdsAddr)}
	for _, c := range cases {
		addr := inboundAddr
		if c.answers503 {
			addr = inbound503Addr
		}
		clusters = append(clusters, readyProbeCluster(t, c, pod, addr))
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
			// What the probe side records for each case, for the runbook's
			// "how a rejection shows" (docs/runbook.md, the inbound-readiness
			// section): the probe cluster's own ssl.* counters.
			t.Logf("%s: probe-side ssl.* %v", c.name, adminStats(t, probe.admin, prefix+"ssl."))
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

	// The 503s are real: the destination's inbound connection managers answered
	// 5xx (only the second inbound does).
	// Health-check responses are not counted in the connection manager's
	// downstream_rq_<code> stats, so the status is read by asking directly,
	// as the node identity, over h2.
	require.Equal(t, http.StatusOK, liveStatus(t, p, inboundAddr), "the first inbound answers the probe path with a 200")
	require.Equal(t, http.StatusServiceUnavailable, liveStatus(t, p, inbound503Addr), "the second inbound answers the probe path with a 503")

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

// liveStatus GETs the inbound's liveness path over mTLS h2 as the accepted
// node identity and returns the status the inbound answered with.
func liveStatus(t *testing.T, p *pki, addr string) int {
	t.Helper()
	certPath, keyPath := p.leaf(t, "live-client", spiffeAgentNode, false)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(p.caPEM))
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      roots,
				NextProtos:   []string{"h2"},
				MinVersion:   tls.VersionTLS12,
			},
		},
	}
	resp, err := client.Get("https://" + addr + proxy.MeshLivePath)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	// Offering ALPN h2 selects the inbound's no-SNI h2 chain, as the probe's
	// does. The inbound's TLS context advertises no ALPN of its own, so Go
	// falls back to HTTP/1.1 on that chain, which its AUTO codec accepts. The
	// status comes from the same health_check filter either way.
	return resp.StatusCode
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
