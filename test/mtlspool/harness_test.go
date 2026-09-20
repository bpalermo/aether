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
	"strings"
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
	setfilterstatenetv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	trustDomain = "aether.internal"

	// The two SOURCE workloads. Different ServiceAccounts, same node — the shape
	// #831 says is unsound and that nothing else in the tree exercises.
	spiffeSourceA = "spiffe://" + trustDomain + "/ns/demo/sa/source-a"
	spiffeSourceB = "spiffe://" + trustDomain + "/ns/demo/sa/source-b"

	// The node identity — the certificate mapper's default_value, i.e. what a
	// connection carrying NO source identity presents (before #842 it was the
	// transport_socket_matcher's on_no_match, for exactly the same connections).
	// Seeing this at the destination is therefore a DIFFERENT defect from seeing
	// the other workload's ID: it means the filter state never reached the
	// upstream, not that a pool leaked. Keeping it distinct is what lets this
	// test tell #686/#825 apart from #831.
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

	// validationContextName is the SDS name of the trust bundle, spelled the way
	// proxy.ValidationContextName renders it.
	validationContextName = "spiffe://" + trustDomain

	// sdsClusterName is the static cluster the harness points every SDS config
	// source at. Production uses `ads: {}` (the agent's own stream); the harness
	// has no ADS, so the sources are rewritten to an explicit api_config_source
	// (rewriteSDSToHarness) — the same substitution //test/envoy_validate makes,
	// and the only deviation from production config in this file.
	sdsClusterName = "sds_cluster"

	// envoyNodeID must match the key the harness SDS snapshot cache is set
	// under (cachev3.IDHash keys by node id).
	envoyNodeID = "test-node"
)

// factoryKeyLabel names the set_filter_state object factory a run uses, for the
// test log.
func factoryKeyLabel(hashable bool) string {
	if hashable {
		return "envoy.hashable_string"
	}
	return "envoy.string"
}

// ---------------------------------------------------------------------------
// Certificates
// ---------------------------------------------------------------------------

// pki is a throwaway trust domain: one CA, one leaf per identity, on disk in
// PEM. The leaves are handed to the harness SDS server (startSDS) as
// file-backed data sources, so delivery goes through a real
// SecretDiscoveryService while the key material stays in one process.
//
// SDS used to be deliberately un-modelled here, because the property under test
// was decided by pool selection long after the secret had resolved. Issue #842
// moved certificate resolution INTO the handshake, so it has to be real now.
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
	// peerSerial identifies WHICH certificate for that identity -- the
	// discriminator a rotation needs, since the SPIFFE ID does not change.
	peerSerial string
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
	// serial is the peer leaf's certificate serial number. The SAN is stable
	// across a rotation -- a rotated SVID carries the SAME SPIFFE ID -- so the
	// serial is the only thing that distinguishes "the selector is still using
	// the certificate it resolved an hour ago" from "the selector picked up the
	// new one". Every leaf this PKI issues gets a distinct serial.
	serial string
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

func (r *peerRegistry) put(remote, san, serial string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[remote] = connRecord{id: r.seq.Add(1), san: san, serial: serial}
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
					san, serial := leafFacts(rawCerts)
					reg.put(remote, san, serial)
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
			w.Header().Set("x-aether-peer-serial", rec.serial)
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

// leafFacts returns the first URI SAN of the leaf client certificate and its
// serial number, or "-" for either. A SPIFFE SVID carries exactly one URI SAN.
//
// Both are read from the certificate the destination VERIFIED, not from
// anything the client asserted.
func leafFacts(rawCerts [][]byte) (san, serial string) {
	if len(rawCerts) == 0 {
		return "-", "-"
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "-", "-"
	}
	san, serial = "-", "-"
	if len(leaf.URIs) > 0 {
		san = leaf.URIs[0].String()
	}
	if leaf.SerialNumber != nil {
		serial = leaf.SerialNumber.String()
	}
	return san, serial
}

// ---------------------------------------------------------------------------
// SDS
// ---------------------------------------------------------------------------

// startSDS runs a REAL SDS server — go-control-plane's snapshot cache behind
// the v3 SecretDiscoveryService — serving one TLS certificate per identity,
// NAMED BY THAT IDENTITY'S SPIFFE ID, plus the trust bundle.
//
// Before issue #842 this harness used file-backed certificates, because the
// property under test (which certificate an upstream CONNECTION ends up
// carrying) was decided by pool selection long after the secret had resolved.
// That is no longer true: the certificate is now chosen DURING the handshake by
// the on-demand selector, which derives a secret name from filter state and
// starts an SDS fetch for it. Remove SDS from the harness and there is nothing
// left to test.
//
// It also pins the property the whole mechanism rests on and nothing else
// checks: AETHER'S SDS SECRET NAMES ARE SPIFFE IDs. The mapper returns the
// filter-state string verbatim as the secret name, so a control plane whose
// secrets were named anything else would silently serve every connection the
// default certificate.
func startSDS(t *testing.T, p *pki, identities []string) string {
	t.Helper()

	secrets := secretResources(t, p, identities)

	snapshot, err := cachev3.NewSnapshot("1", map[resourcev3.Type][]types.Resource{
		resourcev3.SecretType: secrets,
	})
	if err != nil {
		t.Fatalf("build SDS snapshot: %v", err)
	}
	cache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	if err := cache.SetSnapshot(context.Background(), envoyNodeID, snapshot); err != nil {
		t.Fatalf("set SDS snapshot: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for SDS: %v", err)
	}
	gs := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), cache, nil))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	return ln.Addr().String()
}

// secretResources builds the xDS Secret resources a control plane serves for
// this harness: one TLS certificate per identity, NAMED BY THAT IDENTITY'S
// SPIFFE ID, plus the trust bundle. Shared by the standalone SDS server
// (startSDS) and by the ADS control plane (ads_sds_test.go), so both deliver
// byte-identical secrets and the only variable between them is the transport.
func secretResources(t *testing.T, p *pki, identities []string) []types.Resource {
	return secretResourcesGen(t, p, identities, 1)
}

// secretResourcesGen is secretResources with a GENERATION: every identity is
// re-issued as a fresh certificate, under the same SPIFFE ID and therefore the
// same secret name, but with a new serial and its own files on disk.
//
// That is what an SVID rotation is. The distinct file paths matter: the secrets
// are delivered as file data sources, so re-issuing over the same path would
// mutate what the PREVIOUS generation's resource points at and the test would
// pass without anything having been delivered.
func secretResourcesGen(t *testing.T, p *pki, identities []string, gen int) []types.Resource {
	t.Helper()

	secrets := make([]types.Resource, 0, len(identities)+1)
	for _, id := range identities {
		certPath, keyPath := p.leaf(t, fmt.Sprintf("%s-g%d", leafFileName(id), gen), id, false)
		secrets = append(secrets, &tlsv3.Secret{
			Name: id, // the SPIFFE ID IS the secret name
			Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: fileDataSource(certPath),
				PrivateKey:       fileDataSource(keyPath),
			}},
		})
	}
	return append(secrets, &tlsv3.Secret{
		Name: validationContextName,
		Type: &tlsv3.Secret_ValidationContext{ValidationContext: &tlsv3.CertificateValidationContext{
			// SAN matchers stay INLINE in the upstream context (aether layers
			// them over the SDS-rotated bundle via a combined validation
			// context), so the SDS-served half carries only the trust anchor.
			TrustedCa: fileDataSource(p.caPath),
		}},
	})
}

// leafFileName turns a SPIFFE ID into a filesystem-safe leaf name.
func leafFileName(spiffeID string) string {
	return strings.NewReplacer("://", "_", "/", "_").Replace(spiffeID)
}

// sdsCluster is the static cluster the rewritten SDS config sources point at.
func sdsCluster(addr string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 sdsClusterName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(5 * time.Second),
		LoadAssignment:       staticEndpoint(sdsClusterName, addr),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
	}
}

// rewriteSDSToHarness repoints every SDS config source inside an upstream TLS
// context at the harness's static SDS cluster: the validation context's, and —
// the one a naive field walk misses — the on-demand certificate SELECTOR's,
// which lives inside a typed Any and has to be unpacked to be reached.
//
// This is the ONLY place the harness diverges from the config the agent emits.
func rewriteSDSToHarness(t *testing.T, ts *corev3.TransportSocket) {
	t.Helper()

	var ctx tlsv3.UpstreamTlsContext
	if err := ts.GetTypedConfig().UnmarshalTo(&ctx); err != nil {
		t.Fatalf("unmarshal upstream TLS context: %v", err)
	}
	explicit := config.SDSConfigSourceFromCluster(sdsClusterName)

	for _, sc := range ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
		sc.SdsConfig = explicit
	}
	if combined := ctx.GetCommonTlsContext().GetCombinedValidationContext(); combined != nil {
		combined.ValidationContextSdsSecretConfig.SdsConfig = explicit
	}
	if sc := ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig(); sc != nil {
		sc.SdsConfig = explicit
	}
	if sel := ctx.GetCommonTlsContext().GetCustomTlsCertificateSelector(); sel != nil {
		var onDemand on_demand_secretv3.Config
		if err := sel.GetTypedConfig().UnmarshalTo(&onDemand); err != nil {
			t.Fatalf("unmarshal on_demand_secret selector: %v", err)
		}
		onDemand.ConfigSource = explicit
		sel.TypedConfig = config.TypedConfig(&onDemand)
	}

	ts.ConfigType = &corev3.TransportSocket_TypedConfig{TypedConfig: config.TypedConfig(&ctx)}
}

// ---------------------------------------------------------------------------
// Envoy
// ---------------------------------------------------------------------------

// sourceListener models one source pod's egress path: a filter chain that
// stamps the source-attribution filter states with PRODUCTION's builder, then
// an HCM routing everything at the shared mesh cluster.
//
// hashable is the ONE variable the negative control changes — see
// downgradeToNonHashable.
func sourceListener(name, spiffeID string, port int, hashable bool) *listenerv3.Listener {
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
			Filters: append(sourceFilterStates(spiffeID, hashable), &listenerv3.Filter{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}),
		}},
	}
}

// sourceFilterStates returns production's source-attribution filters, optionally
// DOWNGRADED so the certificate-mapper key is built by the non-hashable
// "envoy.string" object factory instead of "envoy.hashable_string".
//
// The downgrade is applied to production's own output rather than spelled from
// scratch, so the negative control differs from the positive case in EXACTLY
// one field of one filter. That is what makes the comparison mean something:
// the object is still there, still shared with the upstream, still found by the
// certificate mapper (HashableString and StringAccessorImpl both satisfy the
// mapper's dynamic_cast to Router::StringAccessor) — the ONLY thing that
// changes is whether CommonUpstreamTransportSocketFactory::hashKey can see it,
// because that gate is a dynamic_cast to Envoy::Hashable.
func sourceFilterStates(spiffeID string, hashable bool) []*listenerv3.Filter {
	filters := proxy.BuildSourceFilterStates(spiffeID)
	if hashable {
		return filters
	}
	for _, f := range filters {
		downgradeToNonHashable(f)
	}
	return filters
}

func downgradeToNonHashable(f *listenerv3.Filter) {
	var cfg setfilterstatenetv3.Config
	if err := f.GetTypedConfig().UnmarshalTo(&cfg); err != nil {
		return // not a set_filter_state filter
	}
	changed := false
	for _, v := range cfg.GetOnNewConnection() {
		if v.GetObjectKey() != proxy.SourceIdentityCertMapperFilterStateKey {
			continue
		}
		v.FactoryKey = "envoy.string"
		changed = true
	}
	if !changed {
		return
	}
	f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&cfg)}
}

// meshCluster is the real node-proxy mesh service cluster: proxy.NewServiceCluster
// plus proxy.InjectUpstreamMTLS — the same two calls the agent's snapshot makes —
// rewritten from EDS to STATIC so it resolves without a control plane, with its
// SDS config sources repointed at the harness SDS server.
//
// Since issue #842 there is nothing per-identity left to model: the cluster
// carries ONE transport socket whose custom_tls_certificate_selector resolves
// the client certificate per connection from filter state. The harness
// therefore no longer hand-builds transport_socket_matches — there are none —
// which also removes the last place it could have drifted from production's
// certificate wiring.
//
// connection_pool_per_downstream_connection is likewise no longer a parameter.
// It is simply absent, because production no longer sets it; its absence is
// half of what this file exists to check.
func meshCluster(t *testing.T, destAddr string) *clusterv3.Cluster {
	t.Helper()

	cl := newMeshCluster(t, destAddr)
	rewriteSDSToHarness(t, cl.GetTransportSocket())
	return cl
}

// newMeshCluster is meshCluster WITHOUT the SDS rewrite: every config source is
// still production's `ads: {}`. The ADS harness (ads_sds_test.go) uses it,
// because repointing the selector at a bespoke api_config_source is precisely
// the substitution that hid issue #842's production failure.
func newMeshCluster(t *testing.T, destAddr string) *clusterv3.Cluster {
	t.Helper()

	cl := proxy.NewServiceCluster(meshClusterName, meshClusterName, meshClusterName, nil)

	// EDS -> STATIC. Everything else about the cluster is production's.
	cl.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
	cl.EdsClusterConfig = nil
	// Subset selectors key off endpoint metadata the static endpoint below does
	// not carry; the criteria-less fallback would cover it, but dropping them
	// keeps the config to the one dimension under test.
	cl.LbSubsetConfig = nil
	cl.LoadAssignment = staticEndpoint(meshClusterName, destAddr)

	if cl.GetConnectionPoolPerDownstreamConnection() {
		t.Fatal("production re-enabled connection_pool_per_downstream_connection: " +
			"this harness would then pass for the wrong reason, because per-downstream " +
			"pools separate the two sources whatever the pool key contains")
	}

	// PRODUCTION's upstream mTLS, unmodified. There is exactly one socket, so
	// the SNI is identical across every upstream connection by construction —
	// which preserves what the old hand-built matches had to be careful about:
	// serverNameOverride IS in the pool hash, so a per-source SNI would separate
	// the pools for an unrelated reason and silently destroy this test's power.
	proxy.InjectUpstreamMTLS(cl, spiffeNode, validationContextName, []string{spiffeDest}, upstreamSNI, "")
	if cl.GetTransportSocket() == nil {
		t.Fatal("InjectUpstreamMTLS produced no transport socket")
	}

	return cl
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
// against it, alongside a real SDS server serving one certificate per identity.
//
// hashable selects the ONE variable under test: whether the source-identity
// filter-state object is built by "envoy.hashable_string" (production) or by
// the non-hashable "envoy.string". Everything else — the cluster, the sockets,
// the SNI, the certificates, the request pattern — is identical between the two
// runs, so any difference in the result comes from the pool key and nothing
// else.
//
// --concurrency 1 is LOAD-BEARING. Connection pools are per worker thread, so
// with more than one worker the two source connections can land on different
// workers and get separate pools for a reason unrelated to the pool key. The
// production node proxy runs many workers and therefore leaks only between
// sources that happen to share one; pinning to a single worker makes the
// property deterministic instead of probabilistic. It is also what makes
// "exactly two upstream connections" a meaningful assertion: with N workers the
// correct answer would be "between 2 and 2N".
func startEnvoy(t *testing.T, p *pki, destAddr string, hashable bool) *proxyHandle {
	t.Helper()

	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}

	sdsAddr := startSDS(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode})

	portA, portB := freePort(t), freePort(t)
	bs := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters: []*clusterv3.Cluster{meshCluster(t, destAddr), sdsCluster(sdsAddr)},
			Listeners: []*listenerv3.Listener{
				sourceListener("source_a", spiffeSourceA, portA, hashable),
				sourceListener("source_b", spiffeSourceB, portB, hashable),
			},
		},
	}

	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	writeFile(t, path, data)
	t.Logf("bootstrap: %s (source identity factory=%s)", path, factoryKeyLabel(hashable))

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
		peerSerial: resp.Header.Get("x-aether-peer-serial"),
		connID:     id,
	}
}
