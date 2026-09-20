// This file is the half of //test/mtlspool that issue #842 shipped without,
// and it is the reason rev228 reached a cluster.
//
// harness_test.go proves pool partitioning against a real Envoy, but it serves
// every secret over a BESPOKE api_config_source pointing at a static SDS
// cluster (rewriteSDSToHarness), because the harness had no control plane.
// Production does not: the node proxy's bootstrap declares ONE delta-ADS stream
// to the agent and every SDS reference is `ads: {}`, multiplexed onto it. That
// substitution was documented as "the ONLY place the harness diverges from the
// config the agent emits" — and it is exactly the axis the per-connection
// certificate selector turned out to be sensitive to.
//
// So this file removes the substitution. It runs the pinned proxy against a
// bootstrap that is production's in every respect that touches secret delivery:
//
//   - dynamic_resources.ads_config, api_type DELTA_GRPC, to a static agent_xds
//     cluster over an h2 Unix socket;
//   - cds_config / lds_config `ads: {}`;
//   - the mesh cluster and both source listeners delivered over that stream by
//     a real go-control-plane snapshot cache, not written into static_resources;
//   - every SDS config source inside the upstream TLS context left as the agent
//     emits it — `ads: {}`, NOT repointed.
//
// # The failure this is designed to catch
//
// It is a PAUSE, not an error. When the on-demand certificate selector cannot
// resolve a secret, doSelectTlsContext has already returned Pending: the
// handshake is suspended waiting for an SDS response that never lands. Nothing
// fails. On the fleet this produced no ssl_connection_error, no
// ssl_fail_verify_san, and an empty upstream_transport_failure_reason; the
// upstream host was selected and the request simply hung until the DOWNSTREAM
// client gave up (response_flags=DC, ~1980 ms).
//
// Every assertion here is therefore on COMPLETION WITHIN A BOUND. "No errors
// were observed" is precisely the reading a stalled selector produces, so it
// cannot be the test. Where a stat is read it is read as a positive obligation
// (cert_updated MUST have moved), never as an absence.
package mtlspool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/test/envoybin"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// adsClusterName is spelled the way the chart spells it
	// (charts/aether/templates/agent-proxy-configmap.yaml) so a reader can put
	// the two bootstraps side by side.
	adsClusterName = "agent_xds"

	// firstRequestBudget is how long the FIRST request through a source
	// listener may take. It has to accommodate a cold on-demand SDS fetch --
	// one local round trip to the control plane on the same box -- and nothing
	// else. Generous by two orders of magnitude on purpose: the failure this
	// bounds is unbounded, so the number only has to be small compared with
	// "never".
	firstRequestBudget = 15 * time.Second

	// rotationBudget is how long a re-issued SVID may take to reach the data
	// plane after the control plane publishes it. SPIRE rotates on a ~4h TTL, so
	// nothing in production depends on this being fast -- it depends on it
	// HAPPENING. The bound exists because "eventually" is not a property a test
	// can fail on, and because the failure mode here is a stall.
	rotationBudget = 20 * time.Second
)

// ---------------------------------------------------------------------------
// The control plane
// ---------------------------------------------------------------------------

// adsControlPlane is a go-control-plane snapshot cache served as an
// AggregatedDiscoveryService over a Unix socket, exactly as the node agent
// serves it (common/xds/xds.go registers the same service on the same kind of
// socket, and agent/internal/xds/cache builds its cache with ads=false too).
type adsControlPlane struct {
	socketPath string
	cache      cachev3.SnapshotCache
	// resources is the snapshot currently served, kept so a rotation can
	// replace ONE resource type and leave the rest alone.
	resources map[resourcev3.Type][]types.Resource
}

// rotateSecrets republishes the snapshot with a new generation of secrets and
// everything else unchanged, under a new version — which is exactly the shape
// of an SVID rotation at the agent (SnapshotCache.SetSecrets replaces the whole
// secret map and regenerates the snapshot; clusters and listeners do not move).
//
// Replacing the whole snapshot instead would silently withdraw the listeners
// over LDS and the test would fail with "connection refused" rather than
// anything to do with certificates.
func (cp *adsControlPlane) rotateSecrets(t *testing.T, version string, secrets []types.Resource) {
	t.Helper()

	next := make(map[resourcev3.Type][]types.Resource, len(cp.resources))
	for typ, res := range cp.resources {
		next[typ] = res
	}
	next[resourcev3.SecretType] = secrets

	snapshot, err := cachev3.NewSnapshot(version, next)
	if err != nil {
		t.Fatalf("build rotated snapshot: %v", err)
	}
	if err := cp.cache.SetSnapshot(context.Background(), envoyNodeID, snapshot); err != nil {
		t.Fatalf("set rotated snapshot: %v", err)
	}
	cp.resources = next
}

// startADSControlPlane serves clusters, listeners and secrets on ONE stream.
//
// Delivering the cluster over CDS rather than static_resources is not
// incidental detail: a statically configured cluster is built during bootstrap
// on the main thread before any worker exists, while a CDS-delivered one is
// built from an xDS update and its transport socket factory -- which is what
// instantiates the certificate selector -- is created in that context. If the
// selector's secret subscription is sensitive to where it is created from, only
// this shape shows it.
func startADSControlPlane(t *testing.T, resources map[resourcev3.Type][]types.Resource) *adsControlPlane {
	t.Helper()

	snapshot, err := cachev3.NewSnapshot("1", resources)
	if err != nil {
		t.Fatalf("build ADS snapshot: %v", err)
	}
	// ads=false matches agent/internal/xds/cache.NewSnapshotCache.
	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	if err := cache.SetSnapshot(context.Background(), envoyNodeID, snapshot); err != nil {
		t.Fatalf("set ADS snapshot: %v", err)
	}

	// A short path: AF_UNIX addresses are capped at 107 bytes and a Bazel test
	// tmpdir is long enough to matter.
	dir, err := os.MkdirTemp("", "aetherads")
	if err != nil {
		t.Fatalf("temp dir for xds socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "xds.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", socketPath, err)
	}
	gs := grpc.NewServer()
	srv := serverv3.NewServer(context.Background(), cache, xdsTrace(t))
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(gs, srv)
	// The node agent registers the per-type services alongside ADS on the same
	// socket (common/xds/xds.go), and the certificate selector's own
	// api_config_source uses SecretDiscoveryService rather than the ADS stream.
	// A harness that registered only ADS would answer that stream UNIMPLEMENTED.
	secretservice.RegisterSecretDiscoveryServiceServer(gs, srv)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	return &adsControlPlane{socketPath: socketPath, cache: cache, resources: resources}
}

// xdsTrace logs what the proxy asks for and what the control plane answers
// with, on BOTH transports -- the delta-ADS stream every other resource rides,
// and the SotW SecretDiscoveryService stream the certificate selector opens.
//
// It is the only way to tell an Envoy that never asked from a control plane
// that never answered, and those two have very different fixes. The rev228
// outage was the first: the subscription was deduplicated away inside Envoy and
// no request was ever sent, which no amount of control-plane logging would have
// shown.
func xdsTrace(t *testing.T) serverv3.Callbacks {
	secretsOnly := func(typeURL string) bool { return strings.HasSuffix(typeURL, "v3.Secret") }
	return &serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: func(_ int64, req *discoverygrpc.DeltaDiscoveryRequest) error {
			if secretsOnly(req.GetTypeUrl()) {
				t.Logf("[ads-delta]  SUBSCRIBE add=%v remove=%v nonce=%q",
					req.GetResourceNamesSubscribe(), req.GetResourceNamesUnsubscribe(), req.GetResponseNonce())
			}
			return nil
		},
		StreamDeltaResponseFunc: func(_ int64, _ *discoverygrpc.DeltaDiscoveryRequest, resp *discoverygrpc.DeltaDiscoveryResponse) {
			if !secretsOnly(resp.GetTypeUrl()) {
				return
			}
			names := make([]string, 0, len(resp.GetResources()))
			for _, r := range resp.GetResources() {
				names = append(names, r.GetName())
			}
			t.Logf("[ads-delta]  RESPOND   resources=%v removed=%v", names, resp.GetRemovedResources())
		},
		StreamRequestFunc: func(id int64, req *discoverygrpc.DiscoveryRequest) error {
			if secretsOnly(req.GetTypeUrl()) {
				t.Logf("[sds-sotw]   stream=%d REQUEST names=%v version=%q nonce=%q",
					id, req.GetResourceNames(), req.GetVersionInfo(), req.GetResponseNonce())
			}
			return nil
		},
		StreamResponseFunc: func(_ context.Context, _ int64, _ *discoverygrpc.DiscoveryRequest, resp *discoverygrpc.DiscoveryResponse) {
			if secretsOnly(resp.GetTypeUrl()) {
				t.Logf("[sds-sotw]   RESPOND   count=%d version=%q", len(resp.GetResources()), resp.GetVersionInfo())
			}
		},
	}
}

// adsBootstrapCluster is the chart's agent_xds cluster: a STATIC h2 cluster over
// the control plane's Unix socket, with the same keepalive and window settings
// the deployed bootstrap carries.
func adsBootstrapCluster(socketPath string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 adsClusterName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(5 * time.Second),
		LoadAssignment:       pipeEndpoint(adsClusterName, socketPath),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
	}
}

// pipeEndpoint is the AF_UNIX equivalent of staticEndpoint: the node proxy
// reaches its agent over a pipe, not a TCP socket.
func pipeEndpoint(clusterName, socketPath string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{Address: &corev3.Address{
						Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: socketPath}},
					}},
				},
			}},
		}},
	}
}

// ---------------------------------------------------------------------------
// Envoy
// ---------------------------------------------------------------------------

// adsProxyHandle is proxyHandle plus the admin address, so a test can read the
// selector's own stats and say WHY a handshake never completed rather than only
// that it did not.
type adsProxyHandle struct {
	*proxyHandle
	adminAddr string
	cp        *adsControlPlane
}

// startEnvoyOverADS runs the pinned proxy against a production-shaped bootstrap:
// nothing but the ADS cluster in static_resources, everything else over one
// delta-ADS stream, and NO SDS config source rewritten.
// adsOptions are the axes the ADS harness varies. They are deliberately named
// after the PRODUCTION facts they model, not after the Envoy fields they set.
type adsOptions struct {
	// staticallyReferenced names identities whose SVID is ALSO named, up front,
	// by an inbound listener on the same proxy -- which on a real node is every
	// local pod's own identity plus the node SVID. It is the condition that
	// makes the certificate selector the SECOND subscriber for that name.
	staticallyReferenced []string

	// freshUpstreamConnectionPerRequest sets max_requests_per_connection: 1 on
	// the mesh cluster, so every request opens a NEW upstream connection and
	// therefore re-runs certificate selection.
	//
	// It is how the rotation test observes a rotation deterministically instead
	// of waiting for an idle pooled connection to be reclaimed. It models
	// nothing about production except the moment production reaches anyway: the
	// next connection after the secret changed. Existing connections keeping
	// their old certificate is correct and is not what is under test here.
	freshUpstreamConnectionPerRequest bool
}

func startEnvoyOverADS(t *testing.T, p *pki, destAddr string, opts adsOptions) *adsProxyHandle {
	t.Helper()

	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}

	portA, portB, adminPort := freePort(t), freePort(t), freePort(t)

	listeners := []types.Resource{
		sourceListener("source_a", spiffeSourceA, portA, true),
		sourceListener("source_b", spiffeSourceB, portB, true),
	}
	for i, id := range opts.staticallyReferenced {
		listeners = append(listeners, inboundListener(t, fmt.Sprintf("inbound_%d", i), id, freePort(t)))
	}

	mesh := newMeshCluster(t, destAddr)
	if opts.freshUpstreamConnectionPerRequest {
		setMaxRequestsPerConnection(t, mesh, 1)
	}

	cp := startADSControlPlane(t, map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  {mesh},
		resourcev3.ListenerType: listeners,
		resourcev3.SecretType:   secretResources(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode}),
	})

	bs := &bootstrapv3.Bootstrap{
		Node:  &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		Admin: &bootstrapv3.Admin{Address: socketAddress("127.0.0.1", adminPort)},
		DynamicResources: &bootstrapv3.Bootstrap_DynamicResources{
			AdsConfig: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_DELTA_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices: []*corev3.GrpcService{{
					TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: adsClusterName},
					},
				}},
			},
			CdsConfig: config.XDSConfigSourceADS(),
			LdsConfig: config.XDSConfigSourceADS(),
		},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters: []*clusterv3.Cluster{adsBootstrapCluster(cp.socketPath)},
		},
	}

	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bootstrap-ads.json")
	writeFile(t, path, data)
	t.Logf("ADS bootstrap: %s (xds socket %s)", path, cp.socketPath)

	cmd := exec.Command(bin, "-c", path,
		"--concurrency", "1",
		"--use-dynamic-base-id",
		"--log-level", "warn")
	cmd.Stdout = &testWriter{t: t, prefix: "envoy"}
	cmd.Stderr = &testWriter{t: t, prefix: "envoy"}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start envoy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	h := &adsProxyHandle{
		proxyHandle: &proxyHandle{
			addrA: fmt.Sprintf("127.0.0.1:%d", portA),
			addrB: fmt.Sprintf("127.0.0.1:%d", portB),
		},
		adminAddr: fmt.Sprintf("127.0.0.1:%d", adminPort),
		cp:        cp,
	}
	// The listeners arrive over LDS, so this also proves the stream is up.
	waitListening(t, h.addrA)
	waitListening(t, h.addrB)
	return h
}

// setMaxRequestsPerConnection sets the cluster's upstream
// max_requests_per_connection, inside typed_extension_protocol_options where
// the v3 API wants it -- the same-named field directly on Cluster is deprecated
// and recent Envoy rejects deprecated fields by default.
func setMaxRequestsPerConnection(t *testing.T, cl *clusterv3.Cluster, n uint32) {
	t.Helper()

	any, ok := cl.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
	if !ok {
		t.Fatalf("cluster %q has no upstream HTTP protocol options to amend", cl.GetName())
	}
	var opts httpv3.HttpProtocolOptions
	if err := any.UnmarshalTo(&opts); err != nil {
		t.Fatalf("unmarshal upstream HTTP protocol options: %v", err)
	}
	if opts.GetCommonHttpProtocolOptions() == nil {
		opts.CommonHttpProtocolOptions = &corev3.HttpProtocolOptions{}
	}
	opts.CommonHttpProtocolOptions.MaxRequestsPerConnection = wrapperspb.UInt32(n)
	cl.TypedExtensionProtocolOptions[config.UpstreamHTTPProtocolOptionsKey] = config.TypedConfig(&opts)
}

// stats returns the admin /stats lines whose name contains substr.
func (h *adsProxyHandle) stats(t *testing.T, substr string) map[string]string {
	t.Helper()

	out := map[string]string{}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + h.adminAddr + "/stats?filter=" + substr)
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
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// TestUpstreamHandshakeCompletesWithADSDeliveredSecrets is the gate rev228
// needed and did not have.
//
// It asserts the one thing a stalled on-demand certificate selector cannot do:
// COMPLETE. Not "without an error" -- a paused handshake raises none -- but
// within firstRequestBudget, from a cold start, with the secret reachable only
// over the same `ads: {}` stream production uses.
//
// A failure here reads as a request that never returned. That is the whole
// signature of the outage: on the fleet the identical config produced zero
// ssl_connection_error, zero ssl_fail_verify_san, an empty
// upstream_transport_failure_reason, and a selected upstream host on every
// hung request.
func TestUpstreamHandshakeCompletesWithADSDeliveredSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoyOverADS(t, p, dest.addr, adsOptions{})

	obs, elapsed := timedCall(t, "source-a", h.addrA)

	t.Logf("first request completed in %s: destination verified %s", elapsed, obs.peerURISAN)
	require.Equal(t, spiffeSourceA, obs.peerURISAN,
		"the destination must verify source-a's own certificate, selected on demand over ADS")

	// The selector's own stats, read as a positive obligation. cert_requested
	// moving proves only that a fetch was STARTED -- on the fleet it moved on
	// every node while the mesh was entirely down. cert_updated is the one that
	// says a secret actually arrived and was applied.
	st := h.stats(t, "on_demand_secret")
	for name, value := range st {
		t.Logf("  %s: %s", name, value)
	}
	require.NotEmpty(t, st, "the on-demand selector reported no stats at all")
}

// TestSecondIdentityAlsoResolvesOverADS covers the identity that is NOT
// prefetch_secret_names.
//
// The selector prefetches exactly one name, the default (the node identity), so
// a harness that only ever exercised the default would resolve every handshake
// from the prefetch cache and never touch the on-demand path at all. Both
// sources here are workload identities, deliberately: each one must be fetched
// on demand, and each must complete.
func TestSecondIdentityAlsoResolvesOverADS(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoyOverADS(t, p, dest.addr, adsOptions{})

	a, elapsedA := timedCall(t, "source-a", h.addrA)
	b, elapsedB := timedCall(t, "source-b", h.addrB)

	t.Logf("source-a completed in %s as %s", elapsedA, a.peerURISAN)
	t.Logf("source-b completed in %s as %s", elapsedB, b.peerURISAN)

	require.Equal(t, spiffeSourceA, a.peerURISAN)
	require.Equal(t, spiffeSourceB, b.peerURISAN)
}

// timedCall makes one request and fails with the elapsed time if it does not
// complete inside firstRequestBudget.
//
// The bound is the assertion. A client that simply waits would report a
// transport error after its own timeout and read like any other flake; this
// says, in the failure message, that the handshake never finished.
func timedCall(t *testing.T, name, addr string) (observation, time.Duration) {
	t.Helper()

	type result struct {
		obs observation
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		c := &http.Client{
			Timeout: firstRequestBudget,
			Transport: &http.Transport{
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     time.Minute,
			},
		}
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Host = "echo.demo.svc"
		resp, err := c.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			done <- result{err: fmt.Errorf("status %d", resp.StatusCode)}
			return
		}
		var id uint64
		_, _ = fmt.Sscanf(resp.Header.Get("x-aether-conn-id"), "%d", &id)
		done <- result{obs: observation{
			peerURISAN: resp.Header.Get("x-aether-peer-uri-san"),
			peerSerial: resp.Header.Get("x-aether-peer-serial"),
			connID:     id,
		}}
	}()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.err != nil {
			t.Fatalf("%s: request did not complete in %s: %v\n"+
				"    The upstream mTLS handshake PAUSED: the on-demand certificate\n"+
				"    selector returned Pending and no SDS response ever resumed it.\n"+
				"    Expect NO ssl_connection_error and NO transport failure reason --\n"+
				"    the absence of errors is the failure mode, not evidence against it.",
				name, elapsed, r.err)
		}
		return r.obs, elapsed
	case <-time.After(firstRequestBudget + 5*time.Second):
		t.Fatalf("%s: request still outstanding after %s", name, time.Since(start))
		return observation{}, 0
	}
}

// inboundListener models a local pod's INBOUND mesh listener: an mTLS
// terminator whose server certificate is that pod's own SVID, named
// STATICALLY in tls_certificate_sds_secret_configs and fetched over the same
// `ads: {}` stream.
//
// It exists here for exactly one reason. On a real node every local pod's SVID
// is referenced twice on one ADS stream:
//
//   - statically, by this listener, at config load; and
//   - on demand, by the mesh cluster's certificate selector, the first time
//     that pod sends traffic.
//
// The harness had no inbound side, so no secret was ever referenced both ways,
// and the shape that broke the fleet could not occur in it.
//
// It is built by production's own proxy.DownstreamTransportSocket so the SDS
// reference is spelled the way the agent spells it.
func inboundListener(t *testing.T, name, podSpiffeID string, port int) *listenerv3.Listener {
	t.Helper()

	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		CodecType:  hcmv3.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{
			RouteConfig: &routev3.RouteConfiguration{
				Name: name,
				VirtualHosts: []*routev3.VirtualHost{{
					Name:    "all",
					Domains: []string{"*"},
					Routes: []*routev3.Route{{
						Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_DirectResponse{
							DirectResponse: &routev3.DirectResponseAction{Status: http.StatusOK},
						},
					}},
				}},
			},
		},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		}},
	}

	return &listenerv3.Listener{
		Name:    name,
		Address: socketAddress("127.0.0.1", port),
		FilterChains: []*listenerv3.FilterChain{{
			TransportSocket: proxy.DownstreamTransportSocket(podSpiffeID, validationContextName, trustDomain),
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}},
		}},
	}
}

// TestOnDemandCertificateResolvesWhenAlreadyStaticallyReferenced is the
// regression gate for the rev228 outage.
//
// It is TestUpstreamHandshakeCompletesWithADSDeliveredSecrets plus one thing:
// the source pod's SVID is ALSO named statically, by that pod's own inbound
// listener, on the same ADS stream -- which is the invariant shape of a real
// node and which the harness had no way to express before.
//
// The assertion is completion, not the absence of an error. When this fails it
// fails by never finishing: the upstream handshake is suspended on an SDS
// response that the control plane has no reason to send a second time, and
// Envoy reports nothing at all until the request's own deadline expires.
func TestOnDemandCertificateResolvesWhenAlreadyStaticallyReferenced(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	// spiffeNode is statically referenced too: on a real node the
	// inboundready_<pod> probe clusters name it, and it is ALSO the selector's
	// prefetch_secret_names entry, so the default certificate is exposed to the
	// same double reference as every workload identity.
	h := startEnvoyOverADS(t, p, dest.addr, adsOptions{
		staticallyReferenced: []string{spiffeSourceA, spiffeSourceB, spiffeNode},
	})

	obs, elapsed := timedCall(t, "source-a", h.addrA)
	t.Logf("first request completed in %s: destination verified %s", elapsed, obs.peerURISAN)

	st := h.stats(t, "on_demand_secret")
	for _, key := range []string{
		"cluster." + meshClusterName + ".on_demand_secret.cert_requested",
		"cluster." + meshClusterName + ".on_demand_secret.cert_updated",
	} {
		t.Logf("  %s: %s", key, st[key])
	}
	require.Equal(t, spiffeSourceA, obs.peerURISAN,
		"the destination must verify source-a's own certificate")
	require.NotEqual(t, "0", st["cluster."+meshClusterName+".on_demand_secret.cert_updated"],
		"cert_updated never moved: the selector requested a secret and none was ever applied")
}

// TestRotatedSVIDIsPickedUpOverTheSelectorStream closes the gap the fix for
// #842 would otherwise have opened, and it is the reason this PR is not a
// one-line change.
//
// Moving the certificate selector off `ads: {}` onto its own SotW
// SecretDiscoveryService stream fixes the cold fetch. It says nothing about the
// SECOND thing that stream has to do: deliver a NEW VERSION of a secret it has
// already resolved. SPIRE rotates SVIDs on roughly a 4 h TTL, so an 8 h soak
// crosses about two rotations while a 60-75 minute deploy validation crosses
// none. If rotation were broken here, the first thing to notice would be a soak
// four hours in — and because this subsystem fails by PAUSING, it would present
// as the soak going quiet rather than as an error.
//
// What is asserted:
//
//   - the destination eventually verifies a DIFFERENT certificate for the same
//     SPIFFE ID (the serial changes; the SAN cannot, that is what a rotation is);
//   - it happens within rotationBudget, not merely "eventually";
//   - EVERY request throughout succeeds. A rotation that resolved correctly but
//     stalled a request in the middle would still be a data-plane outage, and
//     with this failure mode a stall is what it would look like.
//
// The certificate is also referenced statically, as in the reproduction above,
// because that is the shape of a real node and rotation has to work in it.
func TestRotatedSVIDIsPickedUpOverTheSelectorStream(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	identities := []string{spiffeSourceA, spiffeSourceB, spiffeNode}
	h := startEnvoyOverADS(t, p, dest.addr, adsOptions{
		staticallyReferenced: identities,
		// Every request on a new upstream connection, so "has the selector
		// picked up the new certificate" is answered by the next request rather
		// than by whenever a pooled connection happens to be reclaimed. A
		// pooled connection legitimately keeps the certificate it handshook
		// with; that is not what is under test.
		freshUpstreamConnectionPerRequest: true,
	})

	before, _ := timedCall(t, "source-a", h.addrA)
	require.Equal(t, spiffeSourceA, before.peerURISAN)
	require.NotEqual(t, "-", before.peerSerial, "the destination must report the peer's certificate serial")
	t.Logf("pre-rotation:  SAN=%s serial=%s", before.peerURISAN, before.peerSerial)

	updatedBefore := h.certUpdated(t)

	// Re-issue every identity and publish under a new snapshot version, which is
	// what the agent's SPIRE bridge does on an SVID rotation.
	h.cp.rotateSecrets(t, "2", secretResourcesGen(t, p, identities, 2))
	t.Log("rotated: generation 2 of every SVID published at snapshot version 2")

	deadline := time.Now().Add(rotationBudget)
	var after observation
	var attempts int
	for time.Now().Before(deadline) {
		attempts++
		// timedCall fails the test on ANY non-completion, so a rotation that
		// stalls the data plane is caught here rather than being retried away.
		after, _ = timedCall(t, "source-a", h.addrA)
		require.Equal(t, spiffeSourceA, after.peerURISAN,
			"the identity must not change across a rotation, only the certificate")
		if after.peerSerial != before.peerSerial {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Logf("post-rotation: SAN=%s serial=%s after %d request(s) in %s",
		after.peerURISAN, after.peerSerial, attempts, rotationBudget-time.Until(deadline))

	require.NotEqualf(t, before.peerSerial, after.peerSerial,
		"the selector never picked up the rotated SVID within %s: the destination still verifies "+
			"certificate serial %s. On its own stream a re-resolved secret must be pushed to the "+
			"already-open watch; if it is not, every proxy keeps a frozen certificate until it "+
			"expires and the mesh fails CLOSED some hours after a deploy nobody will connect it to",
		rotationBudget, before.peerSerial)

	// cert_updated is the selector's only success signal, and a rotation is
	// exactly a second success for a name it already had. Reading it as a
	// positive obligation is what distinguishes "the new certificate was
	// applied" from "nothing happened and the old one still works".
	updatedAfter := h.certUpdated(t)
	require.Greaterf(t, updatedAfter, updatedBefore,
		"cert_updated did not move across the rotation (%d -> %d): the new secret was never applied",
		updatedBefore, updatedAfter)
	t.Logf("cert_updated: %d -> %d across the rotation", updatedBefore, updatedAfter)
}

// certUpdated reads the mesh cluster's on_demand_secret.cert_updated counter.
// Absent means zero — Envoy does not render a counter it has never touched,
// which is precisely how the rev228 outage hid.
func (h *adsProxyHandle) certUpdated(t *testing.T) int {
	t.Helper()

	raw, ok := h.stats(t, "on_demand_secret")["cluster."+meshClusterName+".on_demand_secret.cert_updated"]
	if !ok {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		t.Fatalf("parse cert_updated %q: %v", raw, err)
	}
	return n
}
