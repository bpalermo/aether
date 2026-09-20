package mtlspool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	trustDomain = "aether.internal"

	// The two SOURCE workloads. Different ServiceAccounts, same node — the shape
	// #831 says is unsound and that nothing else in the tree exercises.
	spiffeSourceA = "spiffe://" + trustDomain + "/ns/demo/sa/source-a"
	spiffeSourceB = "spiffe://" + trustDomain + "/ns/demo/sa/source-b"

	// The node identity. It is what a transport_socket_matcher MISS presents
	// (InjectUpstreamMTLS sets on_no_match to it), so seeing this at the
	// destination is a different defect from seeing the other workload's ID —
	// keeping it distinct is what lets the test tell #686/#825 from #831.
	spiffeNode = "spiffe://" + trustDomain + "/node/test-node"

	// The DESTINATION workload's server identity, pinned by the upstream
	// validation context exactly as InjectUpstreamMTLS pins sanURIs.
	spiffeDest = "spiffe://" + trustDomain + "/ns/demo/sa/echo"

	// Identical on every transport socket, deliberately. serverNameOverride IS
	// folded into the upstream pool hash key (transport_socket_options_impl.cc),
	// so a per-source SNI would separate the pools for an unrelated reason and
	// silently rob this test of its power. Production is the same: aether's
	// intra-cluster SNI is the destination PORT, identical for every source.
	upstreamSNI = "18008"

	meshClusterName = "mesh_echo"
)

// ---------------------------------------------------------------------------
// Certificates
// ---------------------------------------------------------------------------

// pki is a throwaway trust domain: one CA, one leaf per identity, on disk in
// PEM. SDS delivery is deliberately NOT modelled — the property under test is
// which certificate an upstream CONNECTION ends up carrying, which is decided
// by pool selection long after the secret has been resolved. Files keep the
// harness to one process.
type pki struct {
	dir    string
	caPEM  []byte
	caPath string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	dir := t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aether-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	p := &pki{
		dir:    dir,
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		caPath: filepath.Join(dir, "ca.pem"),
		caCert: cert,
		caKey:  key,
	}
	if err := os.WriteFile(p.caPath, p.caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return p
}

// leaf issues a SPIFFE leaf certificate (URI SAN only, as a real SVID) and
// returns the on-disk chain and key paths.
func (p *pki) leaf(t *testing.T, name, spiffeID string, serverAuth bool) (certPath, keyPath string) {
	t.Helper()

	uri, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatalf("parse %q: %v", spiffeID, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if serverAuth {
		usage = append(usage, x509.ExtKeyUsageServerAuth)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
		URIs:         []*url.URL{uri},
	}
	if serverAuth {
		// Envoy sets SNI on the upstream handshake; without a name the peer
		// certificate would still verify (SAN matchers are URI-based) but Go's
		// client-side paths in this harness are simpler with the IP present.
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("sign %s: %v", name, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}

	certPath = filepath.Join(p.dir, name+".crt.pem")
	keyPath = filepath.Join(p.dir, name+".key.pem")
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Destination
// ---------------------------------------------------------------------------

// observation is what the DESTINATION saw for one request: the URI SAN of the
// client certificate it verified, and which TLS connection carried the stream.
//
// The peer URI SAN is exactly the value aether's inbound listener stamps into
// XFCC — ingress.go sets forward_client_cert_details SANITIZE_SET with
// set_current_client_cert_details.uri, which Envoy fills from the verified peer
// certificate's URI SAN. Reading it from the TLS state directly removes an
// Envoy hop from the harness without changing what is being measured.
type observation struct {
	peerURISAN string
	connID     uint64
}

type destination struct {
	addr string
	srv  *http.Server
}

// connRecord is what the destination learned about one TLS connection at
// handshake time.
type connRecord struct {
	id  uint64
	san string
}

// peerRegistry maps a client's remote address to what that connection proved.
//
// The peer identity is captured in the HANDSHAKE (VerifyPeerCertificate) rather
// than read off http.Request.TLS in the handler, because under Go's bundled
// HTTP/2 server r.TLS is nil — verified here on go1.27, and a silent nil is
// exactly the kind of thing that would turn this test green for the wrong
// reason. The handshake callback cannot be skipped: ClientAuth is
// RequireAndVerifyClientCert, so no connection reaches the handler without
// passing through it.
type peerRegistry struct {
	mu      sync.Mutex
	seq     atomic.Uint64
	records map[string]connRecord
}

func (r *peerRegistry) put(remote, san string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[remote] = connRecord{id: r.seq.Add(1), san: san}
}

func (r *peerRegistry) get(remote string) connRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records[remote]
}

func startDestination(t *testing.T, p *pki) *destination {
	t.Helper()

	certPath, keyPath := p.leaf(t, "echo", spiffeDest, true)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(p.caPEM) {
		t.Fatal("append CA to pool")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	reg := &peerRegistry{records: map[string]connRecord{}}
	baseTLS := func() *tls.Config {
		return &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			NextProtos:   []string{"h2"},
		}
	}

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2"},
			// Per-connection config so the verify callback can close over THIS
			// connection's remote address (hello.Conn is the raw conn beneath
			// the tls.Conn net/http will hand the handler, so the address is the
			// same string r.RemoteAddr reports).
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				remote := hello.Conn.RemoteAddr().String()
				cfg := baseTLS()
				cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					reg.put(remote, uriSAN(rawCerts))
					return nil
				}
				return cfg, nil
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := reg.get(r.RemoteAddr)
			san := rec.san
			if san == "" {
				san = "-"
			}
			w.Header().Set("x-aether-peer-uri-san", san)
			w.Header().Set("x-aether-conn-id", fmt.Sprint(rec.id))
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return &destination{addr: ln.Addr().String(), srv: srv}
}

// uriSAN returns the first URI SAN of the leaf client certificate, or "-".
// A SPIFFE SVID carries exactly one.
func uriSAN(rawCerts [][]byte) string {
	if len(rawCerts) == 0 {
		return "-"
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil || len(leaf.URIs) == 0 {
		return "-"
	}
	return leaf.URIs[0].String()
}

// ---------------------------------------------------------------------------
// Envoy
// ---------------------------------------------------------------------------

// sourceListener models one source pod's egress path: a filter chain that
// stamps the source-attribution filter states with PRODUCTION's builder, then
// an HCM routing everything at the shared mesh cluster.
func sourceListener(name, spiffeID string, port int) *listenerv3.Listener {
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
						Match: &routev3.RouteMatch{
							PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
						},
						Action: &routev3.Route_Route{Route: &routev3.RouteAction{
							ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: meshClusterName},
							Timeout:          durationpb.New(10 * time.Second),
						}},
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
			// proxy.BuildSourceFilterStates is the SAME call every
			// mesh-originating chain makes (filterchain.go, capture.go,
			// l4route.go). Re-spelling the set_filter_state proto here would let
			// the harness drift from production and pass by matching nothing.
			Filters: append(proxy.BuildSourceFilterStates(spiffeID), &listenerv3.Filter{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}),
		}},
	}
}

// meshCluster is proxy.NewServiceCluster — the real node-proxy service cluster,
// including its connection_pool_per_downstream_connection setting and its h2
// upstream protocol options — rewritten from EDS to STATIC so it resolves
// without a control plane, and given a per-source transport_socket_matcher.
//
// perDownstreamPool is a parameter rather than a constant so the test can run
// the identical scenario with aether's real setting and with it removed. That
// second run is the negative control: it is the configuration #831 assumes.
func meshCluster(t *testing.T, p *pki, destAddr string, perDownstreamPool bool) *clusterv3.Cluster {
	t.Helper()

	cl := proxy.NewServiceCluster(meshClusterName, meshClusterName, meshClusterName, nil, perDownstreamPool)

	// EDS -> STATIC. Everything else about the cluster is production's.
	cl.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
	cl.EdsClusterConfig = nil
	// Subset selectors key off endpoint metadata the static endpoint below does
	// not carry; the criteria-less fallback would cover it, but dropping them
	// keeps the config to the one dimension under test.
	cl.LbSubsetConfig = nil
	cl.LoadAssignment = staticEndpoint(meshClusterName, destAddr)

	// One transport socket per source identity, named by the SPIFFE ID exactly
	// as proxy.UpstreamTransportSocketMatches names them, plus the node identity.
	type sock struct {
		name string
		file string
		id   string
	}
	var matches []*clusterv3.Cluster_TransportSocketMatch
	for _, s := range []sock{
		{spiffeSourceA, "source-a", spiffeSourceA},
		{spiffeSourceB, "source-b", spiffeSourceB},
		{spiffeNode, "node", spiffeNode},
	} {
		certPath, keyPath := p.leaf(t, s.file, s.id, false)
		matches = append(matches, &clusterv3.Cluster_TransportSocketMatch{
			Name:            s.name,
			TransportSocket: fileUpstreamTLS(p.caPath, certPath, keyPath),
		})
	}
	cl.TransportSocketMatches = matches

	// PRODUCTION's matcher, unmodified: the exact_match_map keyed on
	// proxy.SourceIdentityFilterStateKey via
	// envoy.matching.inputs.transport_socket_filter_state.
	matcher := proxy.UpstreamTransportSocketMatcher([]string{spiffeSourceA, spiffeSourceB})
	if matcher == nil {
		t.Fatal("UpstreamTransportSocketMatcher returned nil for two identities")
	}
	cl.TransportSocketMatcher = matcher
	// A matcher MISS falls through to the cluster's default transport socket.
	// Production instead sets on_no_match to the node identity; both land on the
	// node certificate, which is the point — a miss must be distinguishable at
	// the destination from a pooling leak.
	nodeCert, nodeKey := p.leaf(t, "node-default", spiffeNode, false)
	cl.TransportSocket = fileUpstreamTLS(p.caPath, nodeCert, nodeKey)

	return cl
}

// fileUpstreamTLS mirrors proxy.UpstreamTransportSocket (ALPN h2, URI-SAN-pinned
// combined validation) with the SDS references replaced by files.
//
// Every socket is byte-identical apart from the certificate, and in particular
// carries the SAME SNI: sni is part of the upstream pool hash key, so varying
// it per source would separate the pools for a reason that has nothing to do
// with the filter state.
func fileUpstreamTLS(caPath, certPath, keyPath string) *corev3.TransportSocket {
	ctx := &tlsv3.UpstreamTlsContext{
		Sni: upstreamSNI,
		CommonTlsContext: &tlsv3.CommonTlsContext{
			AlpnProtocols: []string{"h2"},
			TlsCertificates: []*tlsv3.TlsCertificate{{
				CertificateChain: fileDataSource(certPath),
				PrivateKey:       fileDataSource(keyPath),
			}},
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
				ValidationContext: &tlsv3.CertificateValidationContext{
					TrustedCa: fileDataSource(caPath),
					MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
						SanType: tlsv3.SubjectAltNameMatcher_URI,
						Matcher: &matcherv3.StringMatcher{
							MatchPattern: &matcherv3.StringMatcher_Exact{Exact: spiffeDest},
						},
					}},
				},
			},
		},
	}
	return &corev3.TransportSocket{
		Name:       "envoy.transport_sockets.tls",
		ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(ctx)},
	}
}

func fileDataSource(path string) *corev3.DataSource {
	return &corev3.DataSource{Specifier: &corev3.DataSource_Filename{Filename: path}}
}

func socketAddress(addr string, port int) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{
		SocketAddress: &corev3.SocketAddress{
			Address:       addr,
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(port)},
		},
	}}
}

func staticEndpoint(clusterName, hostPort string) *endpointv3.ClusterLoadAssignment {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		panic(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		panic(err)
	}
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, port)},
				},
			}},
		}},
	}
}

// proxyHandle is a running Envoy plus the two source listener addresses.
type proxyHandle struct {
	addrA string
	addrB string
}

// startEnvoy writes a static bootstrap and runs the pinned aether-proxy Envoy
// against it.
//
// --concurrency 1 is LOAD-BEARING. Connection pools are per worker thread, so
// with more than one worker the two source connections can land on different
// workers and get separate pools for a reason unrelated to the pool key. The
// production node proxy runs many workers and therefore leaks only between
// sources that happen to share one; pinning to a single worker makes the
// property deterministic instead of probabilistic.
func startEnvoy(t *testing.T, p *pki, destAddr string, perDownstreamPool bool) *proxyHandle {
	t.Helper()

	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}

	portA, portB := freePort(t), freePort(t)
	bs := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "test-node", Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters: []*clusterv3.Cluster{meshCluster(t, p, destAddr, perDownstreamPool)},
			Listeners: []*listenerv3.Listener{
				sourceListener("source_a", spiffeSourceA, portA),
				sourceListener("source_b", spiffeSourceB, portB),
			},
		},
	}

	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	writeFile(t, path, data)
	t.Logf("bootstrap: %s (connection_pool_per_downstream_connection=%v)", path, perDownstreamPool)

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

	h := &proxyHandle{
		addrA: fmt.Sprintf("127.0.0.1:%d", portA),
		addrB: fmt.Sprintf("127.0.0.1:%d", portB),
	}
	waitListening(t, h.addrA)
	waitListening(t, h.addrB)
	return h
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("envoy never listened on %s", addr)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type testWriter struct {
	t      *testing.T
	prefix string
}

func (w *testWriter) Write(b []byte) (int, error) {
	w.t.Logf("[%s] %s", w.prefix, b)
	return len(b), nil
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// sourceClient is one source workload's application: a keep-alive HTTP/1.1
// client whose connection to its own egress listener stays open between
// requests, which is what gives the proxy an opportunity to reuse a pool.
type sourceClient struct {
	name string
	base string
	http *http.Client
}

func newSourceClient(name, addr string) *sourceClient {
	return &sourceClient{
		name: name,
		base: "http://" + addr + "/",
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        1,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     time.Minute,
			},
		},
	}
}

func (c *sourceClient) call(t *testing.T) observation {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base, nil)
	if err != nil {
		t.Fatalf("%s: new request: %v", c.name, err)
	}
	req.Host = "echo.demo.svc"
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s: request: %v", c.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d (upstream never completed mTLS?)", c.name, resp.StatusCode)
	}
	var id uint64
	_, _ = fmt.Sscanf(resp.Header.Get("x-aether-conn-id"), "%d", &id)
	return observation{
		peerURISAN: resp.Header.Get("x-aether-peer-uri-san"),
		connID:     id,
	}
}
