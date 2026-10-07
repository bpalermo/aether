package mtlspool

// The HTTP/3 arm of the pool-identity harness (proposal 038 Phase 4b).
//
// The TCP arm proves the per-connection certificate SELECTOR presents the
// right identity. QUIC has no selector -- QuicClientTransportSocketFactory
// rejects one -- so a per-source `quic:` cluster carries ONE identity and the
// ROUTE chooses the cluster by the source's filter-state identity
// (proxy.ApplyQUICClusterSelection). The thing that can fail is therefore the
// SELECTION: source B's request routed to A's cluster, so the destination sees
// A's SAN for B's request and the request is still delivered -- #831's shape,
// failing open. The pool-sharing control of the TCP arm is vacuous here
// (separate clusters structurally never share a pool), so the negative control
// is the arm map itself, swapped.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	quicTwinA = "quic:" + meshClusterName + "@demo/source-a"
	quicTwinB = "quic:" + meshClusterName + "@demo/source-b"
	// quicDestFQDN is the destination's mesh name; the SVID carries it and a
	// wildcard under it as DNS SANs (issue #957, path B), and the QUIC upstream
	// dials with SNI "<port>.<fqdn>" so the port demux survives Envoy's QUIC
	// client hostname check (verifyLeafCertMatchesHostname: DNS SANs only).
	quicDestFQDN = "echo.demo.svc"
)

// quicSNI is the production shape (proxy.QUICServerName), so this harness
// proves the SNI the agent actually programs.
var quicSNI = proxy.QUICServerName(upstreamSNI, quicDestFQDN)

// destinationH3 is an HTTP/3 server that requires and verifies the client
// certificate and reports, per request, the URI SAN it verified, the QUIC
// connection it arrived on, and the protocol.
type destinationH3 struct {
	addr string
	srv  *http3.Server
	reg  *peerRegistry
}

func startDestinationH3(t *testing.T, p *pki) *destinationH3 {
	t.Helper()
	return startDestinationH3With(t, p, defaultQUICOptions())
}

// startDestinationH3With is startDestinationH3 with the leaf's shape taken
// from o.uriOnlyLeaf.
func startDestinationH3With(t *testing.T, p *pki, o quicOptions) *destinationH3 {
	t.Helper()
	var certPath, keyPath string
	if o.uriOnlyLeaf {
		certPath, keyPath = p.leaf(t, "echo-h3", spiffeDest, true)
	} else {
		certPath, keyPath = p.leafDNS(t, "echo-h3", spiffeDest, []string{quicDestFQDN, "*." + quicDestFQDN})
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(p.caPEM))
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	reg := &peerRegistry{records: map[string]connRecord{}}
	srv := &http3.Server{
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
		}),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// r.TLS is the QUIC connection's verified state; the SAN comes
			// from the certificate the destination VERIFIED, never from a
			// header the client set.
			san, serial := "-", "-"
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				san, serial = leafFacts([][]byte{r.TLS.PeerCertificates[0].Raw})
			}
			// One record per QUIC connection (RemoteAddr is stable for the
			// connection's lifetime) so connID counts connections, as on TCP.
			if rec := reg.get(r.RemoteAddr); rec.id == 0 {
				reg.put(r.RemoteAddr, san, serial)
			}
			rec := reg.get(r.RemoteAddr)
			w.Header().Set("x-aether-peer-uri-san", san)
			w.Header().Set("x-aether-peer-serial", serial)
			w.Header().Set("x-aether-conn-id", fmt.Sprint(rec.id))
			w.Header().Set("x-aether-proto", r.Proto)
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(pc) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &destinationH3{addr: pc.LocalAddr().String(), srv: srv, reg: reg}
}

// quicTwin builds a source's `quic:` cluster exactly as the cache does
// (proxy.QUICClusterFrom over the h2 base), on a STATIC endpoint for the
// harness, with its SDS sources pointed at the harness SDS server.
func quicTwin(t *testing.T, name, sourceSpiffeID, destAddr string) *clusterv3.Cluster {
	t.Helper()
	return quicTwinWith(t, name, sourceSpiffeID, destAddr, defaultQUICOptions())
}

// quicTwinWith is quicTwin with the SNI and the server SAN pin taken from o.
func quicTwinWith(t *testing.T, name, sourceSpiffeID, destAddr string, o quicOptions) *clusterv3.Cluster {
	t.Helper()
	base := proxy.NewServiceCluster(meshClusterName, meshClusterName, meshClusterName, nil)
	base.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
	base.EdsClusterConfig = nil
	base.LbSubsetConfig = nil
	base.LoadAssignment = staticEndpoint(name, destAddr)
	cl := proxy.QUICClusterFrom(base, name, sourceSpiffeID, validationContextName, o.sanPin, o.sni, 0)
	rewriteQUICSDSToHarness(t, cl.GetTransportSocket())
	return cl
}

// rewriteQUICSDSToHarness is rewriteSDSToHarness for a QuicUpstreamTransport:
// the inner UpstreamTlsContext's SDS sources are pointed at the harness.
func rewriteQUICSDSToHarness(t *testing.T, ts *corev3.TransportSocket) {
	t.Helper()
	var q quicv3.QuicUpstreamTransport
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(&q))
	explicit := config.SDSConfigSourceFromCluster(sdsClusterName)
	ctx := q.GetUpstreamTlsContext()
	for _, sc := range ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
		sc.SdsConfig = explicit
	}
	if combined := ctx.GetCommonTlsContext().GetCombinedValidationContext(); combined != nil {
		combined.ValidationContextSdsSecretConfig.SdsConfig = explicit
	}
	ts.ConfigType = &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(&q)}
}

// selectingSourceListener is sourceListener with its route rewritten by
// proxy.ApplyQUICClusterSelection -- the production mechanism, not a copy.
func selectingSourceListener(t *testing.T, name, spiffeID string, port int, arms map[string]string) *listenerv3.Listener {
	t.Helper()
	l := sourceListener(name, spiffeID, port, true)
	filters := l.GetFilterChains()[0].GetFilters()
	hcmFilter := filters[len(filters)-1]
	var hcm hcmv3.HttpConnectionManager
	require.NoError(t, hcmFilter.GetTypedConfig().UnmarshalTo(&hcm))
	vh := hcm.GetRouteConfig().GetVirtualHosts()[0]
	require.Equal(t, 1, proxy.ApplyQUICClusterSelection(vh, meshClusterName, arms))
	hcmFilter.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&hcm)}
	return l
}

// startEnvoyQUIC runs the proxy with the h2 mesh cluster (the on_no_match
// fallback, pointed at the TCP destination), the two quic: twins (pointed at
// the HTTP/3 destination) and two source listeners whose routes select by
// identity through arms.
func startEnvoyQUIC(t *testing.T, p *pki, h2Addr, h3Addr string, arms map[string]string) *proxyHandle {
	t.Helper()
	return startEnvoyQUICWith(t, p, h2Addr, h3Addr, arms, defaultQUICOptions())
}

// startEnvoyQUICWith is startEnvoyQUIC with the twins, the runtime and the
// log plumbing taken from o.
func startEnvoyQUICWith(t *testing.T, p *pki, h2Addr, h3Addr string, arms map[string]string, o quicOptions) *proxyHandle {
	t.Helper()
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("locate envoy: %v", err)
	}
	sdsAddr := startSDS(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode})
	portA, portB := freePort(t), freePort(t)
	bs := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters: []*clusterv3.Cluster{
				meshCluster(t, h2Addr), sdsCluster(sdsAddr),
				quicTwinWith(t, quicTwinA, spiffeSourceA, h3Addr, o),
				quicTwinWith(t, quicTwinB, spiffeSourceB, h3Addr, o),
			},
			Listeners: []*listenerv3.Listener{
				selectingSourceListener(t, "source_a", spiffeSourceA, portA, arms),
				selectingSourceListener(t, "source_b", spiffeSourceB, portB, arms),
			},
		},
		LayeredRuntime: o.layeredRuntime(),
	}
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(bs)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bootstrap-quic.json")
	writeFile(t, path, data)
	t.Logf("bootstrap: %s", path)
	args := append([]string{"-c", path, "--concurrency", "1", "--use-dynamic-base-id", "--log-level", "warn"}, o.extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = o.output(t)
	cmd.Stderr = o.output(t)
	require.NoError(t, cmd.Start())
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

func selectionArms() map[string]string {
	return map[string]string{spiffeSourceA: quicTwinA, spiffeSourceB: quicTwinB}
}

// TestQUICSelectionPresentsEachSourceIdentity: with the arms as the cache
// builds them, every request from source-a arrives at the HTTP/3 destination
// carrying source-a's SVID, every request from source-b carries source-b's,
// over HTTP/3, and nothing reaches the h2 fallback destination.
func TestQUICSelectionPresentsEachSourceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	p := newPKI(t)
	h2 := startDestination(t, p)
	h3 := startDestinationH3(t, p)
	h := startEnvoyQUIC(t, p, h2.addr, h3.addr, selectionArms())
	fromA, fromB := exchangeProto(t, h, exchangeRounds)
	report(t, "A", observations(fromA))
	report(t, "B", observations(fromB))
	require.Len(t, fromA, exchangeRounds+1)
	require.Len(t, fromB, exchangeRounds+1)
	for i, o := range fromA {
		assert.Equalf(t, spiffeSourceA, o.peerURISAN, "request %d from source-a was verified as %q", i, o.peerURISAN)
		assert.Equalf(t, "HTTP/3.0", o.proto, "request %d from source-a did not ride HTTP/3", i)
	}
	for i, o := range fromB {
		assert.Equalf(t, spiffeSourceB, o.peerURISAN, "request %d from source-b was verified as %q (source-a's ID = the selection sent B down A's cluster; the node ID = a twin named the wrong secret)", i, o.peerURISAN)
		assert.Equalf(t, "HTTP/3.0", o.proto, "request %d from source-b did not ride HTTP/3", i)
	}
	assert.Empty(t, sharedConnections(observations(fromA), observations(fromB)), "two clusters must never share a QUIC connection")
}

// TestQUICSelectionArmsAreLoadBearing is the negative control: with the arms
// SWAPPED (a -> b's twin, b -> a's twin) the destination verifies source-b's
// SVID on source-a's requests. The requests still succeed -- that is the
// fail-open shape the positive test exists to catch -- so the assertion that
// would flag it in production is shown to fire here.
func TestQUICSelectionArmsAreLoadBearing(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	p := newPKI(t)
	h2 := startDestination(t, p)
	h3 := startDestinationH3(t, p)
	swapped := map[string]string{spiffeSourceA: quicTwinB, spiffeSourceB: quicTwinA}
	h := startEnvoyQUIC(t, p, h2.addr, h3.addr, swapped)
	fromA, fromB := exchangeProto(t, h, 2)
	report(t, "A(swapped)", observations(fromA))
	report(t, "B(swapped)", observations(fromB))
	for _, o := range fromA {
		assert.Equal(t, spiffeSourceB, o.peerURISAN, "with the arms swapped source-a must present source-b's identity; if it presents its own, the route is not what selects the identity and the positive test proves nothing")
		assert.Equal(t, "HTTP/3.0", o.proto)
	}
	for _, o := range fromB {
		assert.Equal(t, spiffeSourceA, o.peerURISAN)
	}
}

// protoObservation is observation plus the protocol the destination saw.
type protoObservation struct {
	observation
	proto string
}

func observations(in []protoObservation) []observation {
	out := make([]observation, 0, len(in))
	for _, o := range in {
		out = append(out, o.observation)
	}
	return out
}

func exchangeProto(t *testing.T, h *proxyHandle, rounds int) (fromA, fromB []protoObservation) {
	t.Helper()
	a := newSourceClient("source-a", h.addrA)
	b := newSourceClient("source-b", h.addrB)
	for i := 0; i <= rounds; i++ {
		fromA = append(fromA, a.callProto(t))
		fromB = append(fromB, b.callProto(t))
	}
	return fromA, fromB
}

func (c *sourceClient) callProto(t *testing.T) protoObservation {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base, nil)
	require.NoError(t, err)
	req.Host = "echo.demo.svc"
	var resp *http.Response
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err = c.http.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		var body string
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
			_ = resp.Body.Close()
		}
		require.True(t, time.Now().Before(deadline), "%s: no 200 from the proxy before the deadline (last: err=%v status=%v body=%q)", c.name, err, resp, body)
		time.Sleep(200 * time.Millisecond)
	}
	defer resp.Body.Close()
	var id uint64
	_, _ = fmt.Sscanf(resp.Header.Get("x-aether-conn-id"), "%d", &id)
	return protoObservation{
		observation: observation{peerURISAN: resp.Header.Get("x-aether-peer-uri-san"), peerSerial: resp.Header.Get("x-aether-peer-serial"), connID: id},
		proto:       resp.Header.Get("x-aether-proto"),
	}
}

// leafDNS is pki.leaf for a SERVER leaf that also carries DNS SANs -- the
// SVID shape issue #957's path B proposes (SPIRE ClusterSPIFFEID
// dnsNameTemplates), so the HTTP/3 destination can pass Envoy's QUIC client
// hostname check while the identity is still the URI SAN.
func (p *pki) leafDNS(t *testing.T, name, spiffeID string, dnsNames []string) (certPath, keyPath string) {
	t.Helper()
	uri, err := url.Parse(spiffeID)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{uri},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath = filepath.Join(p.dir, name+".crt.pem")
	keyPath = filepath.Join(p.dir, name+".key.pem")
	writeFile(t, certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}
