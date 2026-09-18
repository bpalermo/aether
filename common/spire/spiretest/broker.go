package spiretest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// BrokerSecurityHeader is the static gRPC metadata the SPIFFE Broker Endpoint
// specification requires on every request (SPIFFE_Broker_Endpoint.md §3). The
// fake rejects a call without it, exactly as a real SPIRE agent does, so a
// client that forgets the interceptor fails its very first test.
const (
	BrokerSecurityHeader      = "broker.spiffe.io"
	BrokerSecurityHeaderValue = "true"
)

// DefaultBrokerServerID is the SPIFFE ID the fake broker presents by default: a
// SPIRE agent identity, which is what a client is expected to authorize.
const DefaultBrokerServerID = "spiffe://" + TrustDomain + "/spire/agent/k8s_psat/demo/node-a"

// CA mints X.509 SVIDs for tests. Unlike NewWorkloadSVID it keeps the signing
// key, so both ends of a mutually-authenticated connection can be issued
// identities that chain to the same bundle.
type CA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	serial atomic.Int64
}

// NewCA returns a self-signed test CA.
func NewCA(t *testing.T) *CA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"SPIRE test CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &CA{cert: cert, key: key}
}

// Bundle returns the trust bundle for td containing this CA.
func (c *CA) Bundle(td spiffeid.TrustDomain) *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(td, []*x509.Certificate{c.cert})
}

// BundleDER returns this CA's certificate as the concatenated ASN.1 DER the
// Broker API carries in its bundle fields.
func (c *CA) BundleDER() []byte {
	return c.cert.Raw
}

// SVID mints a leaf SVID for the given SPIFFE ID, signed by this CA.
func (c *CA) SVID(t *testing.T, id string) *x509svid.SVID {
	t.Helper()

	sid := spiffeid.RequireFromString(id)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(c.serial.Add(1) + 1),
		Subject:               pkix.Name{Organization: []string{"SPIRE test leaf"}},
		URIs:                  []*url.URL{sid.URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &x509svid.SVID{ID: sid, Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

// BrokerSVID mints a Broker API X509SVID for the given SPIFFE ID: the wire
// shape, with the chain and the bundle as concatenated DER and the key as
// PKCS#8 DER. version distinguishes one generation of an SVID from the next in
// tests that assert ordering; it is encoded in the certificate serial number.
func (c *CA) BrokerSVID(t *testing.T, id string, version int) *brokerpb.X509SVID {
	t.Helper()

	sid := spiffeid.RequireFromString(id)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(int64(version) + 1),
		Subject:               pkix.Name{Organization: []string{"SPIRE test leaf"}},
		URIs:                  []*url.URL{sid.URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return &brokerpb.X509SVID{
		SpiffeId:    id,
		X509Svid:    der,
		X509SvidKey: keyDER,
		Bundle:      c.cert.Raw,
	}
}

// Identity is a static x509svid.Source + x509bundle.Source, the shape
// common/spire.WaitingSource has once SPIRE has issued an SVID. It also
// announces updates, so a bridge that wakes on Updated() can be driven.
type Identity struct {
	mu      sync.Mutex
	svid    *x509svid.SVID
	bundles *x509bundle.Set

	updated chan struct{}
}

// Identity mints an identity for id backed by this CA, trusting this CA's
// bundle plus any extra bundles (used to test that a peer in another trust
// domain is rejected by the authorizer rather than by chain verification).
func (c *CA) Identity(t *testing.T, id string, extra ...*x509bundle.Bundle) *Identity {
	t.Helper()

	svid := c.SVID(t, id)
	bundles := []*x509bundle.Bundle{c.Bundle(svid.ID.TrustDomain())}
	bundles = append(bundles, extra...)

	return &Identity{
		svid:    svid,
		bundles: x509bundle.NewSet(bundles...),
		updated: make(chan struct{}, 1),
	}
}

// NewPendingIdentity returns an Identity that has NOTHING yet: every accessor
// fails, which is the state common/spire.WaitingSource is in while SPIRE is
// still coming up (issue #740). Call Arrive to publish one.
func NewPendingIdentity() *Identity {
	return &Identity{updated: make(chan struct{}, 1)}
}

// Arrive publishes an identity and wakes whoever is watching, as go-spiffe does
// on the first Workload API update.
func (i *Identity) Arrive(svid *x509svid.SVID, bundles ...*x509bundle.Bundle) {
	i.mu.Lock()
	i.svid = svid
	i.bundles = x509bundle.NewSet(bundles...)
	i.mu.Unlock()
	select {
	case i.updated <- struct{}{}:
	default:
	}
}

// GetX509SVID implements x509svid.Source.
func (i *Identity) GetX509SVID() (*x509svid.SVID, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.svid == nil {
		return nil, fmt.Errorf("no SVID yet")
	}
	return i.svid, nil
}

// GetX509BundleForTrustDomain implements x509bundle.Source.
func (i *Identity) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	i.mu.Lock()
	bundles := i.bundles
	i.mu.Unlock()
	if bundles == nil {
		return nil, fmt.Errorf("no trust bundle yet")
	}
	return bundles.GetX509BundleForTrustDomain(td)
}

// Updated implements the update announcement a WaitingSource makes.
func (i *Identity) Updated() <-chan struct{} { return i.updated }

// BrokerEntry is what the fake broker returns for one resolved reference.
type BrokerEntry struct {
	// SVIDs are the X.509-SVIDs the referenced pod is entitled to.
	SVIDs []*brokerpb.X509SVID
	// FederatedBundles are the foreign trust bundles carried alongside them,
	// keyed by the foreign trust domain's SPIFFE URI (as SPIRE keys them).
	FederatedBundles map[string][]byte
}

// FakeBroker is a SPIFFE Broker Endpoint that resolves KubernetesObjectReferences
// from a table. It enforces the parts of the specification a client can get
// wrong: mutual TLS, and the mandatory security header.
type FakeBroker struct {
	brokerpb.UnimplementedAPIServer

	mu sync.Mutex
	// entries maps "<namespace>/<name>" to the response for that pod.
	entries map[string]*BrokerEntry
	// notFoundFor counts down per key: while positive the reference is reported
	// NotFound, which reproduces the CNI-ADD-beats-the-kubelet-list race.
	notFoundFor map[string]int
	// denyFor marks keys the broker refuses with PermissionDenied.
	denyFor map[string]bool
	// closeAfterFirst ends the first stream for a key after one response,
	// simulating a SPIRE agent restart.
	closeAfterFirst map[string]bool
	// pending holds the live streams per key so a test can push a rotation.
	pending map[string][]chan *brokerpb.SubscribeToX509SVIDResponse
	// lastRef is the most recent reference the fake decoded.
	lastRef *brokerpb.KubernetesObjectReference

	subscribes atomic.Int64
	badHeader  atomic.Int64
}

// SetEntry registers (or replaces) the response for a pod reference.
func (f *FakeBroker) SetEntry(namespace, name string, entry *BrokerEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[brokerKey(namespace, name)] = entry
}

// SetNotFoundFor makes the next n subscribes for a pod reference fail with
// NotFound before it resolves.
func (f *FakeBroker) SetNotFoundFor(namespace, name string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notFoundFor[brokerKey(namespace, name)] = n
}

// SetPermissionDeniedFor makes every subscribe for a pod reference fail with
// PermissionDenied.
func (f *FakeBroker) SetPermissionDeniedFor(namespace, name string, denied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denyFor[brokerKey(namespace, name)] = denied
}

// SetCloseAfterFirst makes the FIRST stream for a pod reference end after one
// response, so a client's re-subscribe path can be exercised.
func (f *FakeBroker) SetCloseAfterFirst(namespace, name string, close bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeAfterFirst[brokerKey(namespace, name)] = close
}

// Rotate pushes a new response onto every live stream for a pod reference and
// makes it the entry future subscribes resolve to. It reports how many live
// streams received it.
func (f *FakeBroker) Rotate(namespace, name string, entry *BrokerEntry) int {
	f.mu.Lock()
	key := brokerKey(namespace, name)
	f.entries[key] = entry
	streams := append([]chan *brokerpb.SubscribeToX509SVIDResponse(nil), f.pending[key]...)
	f.mu.Unlock()

	sent := 0
	for _, ch := range streams {
		select {
		case ch <- entryResponse(entry):
			sent++
		default:
		}
	}
	return sent
}

// Subscribes counts the SubscribeToX509SVID calls received so far, including
// the ones that were rejected.
func (f *FakeBroker) Subscribes() int64 { return f.subscribes.Load() }

// BadHeaders counts calls that arrived without the broker security header.
// A real SPIRE agent rejects those with InvalidArgument, so a non-zero count
// means the client is missing its interceptor.
func (f *FakeBroker) BadHeaders() int64 { return f.badHeader.Load() }

// SubscribeToX509SVID implements the Broker API's X.509-SVID profile.
func (f *FakeBroker) SubscribeToX509SVID(req *brokerpb.SubscribeToX509SVIDRequest, stream grpc.ServerStreamingServer[brokerpb.SubscribeToX509SVIDResponse]) error {
	f.subscribes.Add(1)

	if err := f.checkSecurityHeader(stream.Context()); err != nil {
		return err
	}

	ref, err := decodePodReference(req)
	if err != nil {
		return err
	}
	key := brokerKey(ref.GetKey().GetNamespace(), ref.GetKey().GetName())

	f.mu.Lock()
	f.lastRef = ref
	f.mu.Unlock()

	entry, closeAfterFirst, err := f.resolve(key)
	if err != nil {
		return err
	}

	updates := make(chan *brokerpb.SubscribeToX509SVIDResponse, 8)
	f.mu.Lock()
	f.pending[key] = append(f.pending[key], updates)
	f.mu.Unlock()
	defer f.removeStream(key, updates)

	if err := stream.Send(entryResponse(entry)); err != nil {
		return err
	}
	if closeAfterFirst {
		return nil // simulated SPIRE agent restart
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case resp := <-updates:
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// checkSecurityHeader enforces the specification's SSRF guard: a request without
// exactly one `broker.spiffe.io: true` metadata value is InvalidArgument.
func (f *FakeBroker) checkSecurityHeader(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get(BrokerSecurityHeader); len(values) != 1 || values[0] != BrokerSecurityHeaderValue {
		f.badHeader.Add(1)
		return status.Error(codes.InvalidArgument, "security header missing from request")
	}
	return nil
}

// resolve applies the NotFound / PermissionDenied table and returns the entry.
func (f *FakeBroker) resolve(key string) (*BrokerEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.denyFor[key] {
		return nil, false, status.Errorf(codes.PermissionDenied,
			"Kubernetes authorizer does not allow the broker to use impersonate-via-spire for %s", key)
	}
	if remaining := f.notFoundFor[key]; remaining > 0 {
		f.notFoundFor[key] = remaining - 1
		return nil, false, status.Errorf(codes.NotFound, "pod %s not found on agent node", key)
	}
	entry, ok := f.entries[key]
	if !ok {
		return nil, false, status.Errorf(codes.NotFound, "pod %s not found on agent node", key)
	}

	closeAfterFirst := f.closeAfterFirst[key]
	if closeAfterFirst {
		// Only the FIRST stream closes early; later ones stay open.
		f.closeAfterFirst[key] = false
	}
	return entry, closeAfterFirst, nil
}

// removeStream drops a finished stream from the live set.
func (f *FakeBroker) removeStream(key string, ch chan *brokerpb.SubscribeToX509SVIDResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	live := f.pending[key]
	for i, candidate := range live {
		if candidate == ch {
			f.pending[key] = append(live[:i], live[i+1:]...)
			return
		}
	}
}

// entryResponse renders an entry as a Broker API response. A nil entry is a
// legitimate steady state: a reference that resolves to a pod with no
// registration entries yet gets an empty response, not an error.
func entryResponse(entry *BrokerEntry) *brokerpb.SubscribeToX509SVIDResponse {
	if entry == nil {
		return &brokerpb.SubscribeToX509SVIDResponse{}
	}
	return &brokerpb.SubscribeToX509SVIDResponse{
		Svids:            entry.SVIDs,
		FederatedBundles: entry.FederatedBundles,
	}
}

// decodePodReference unpacks the request's WorkloadReference and checks it is
// the pods/core KubernetesObjectReference the aether agent is meant to send.
func decodePodReference(req *brokerpb.SubscribeToX509SVIDRequest) (*brokerpb.KubernetesObjectReference, error) {
	packed := req.GetReference().GetReference()
	if packed == nil {
		return nil, status.Error(codes.InvalidArgument, "workload reference must be provided")
	}
	msg, err := packed.UnmarshalNew()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported reference type: %s", packed.GetTypeUrl())
	}
	ref, ok := msg.(*brokerpb.KubernetesObjectReference)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported reference type: %s", packed.GetTypeUrl())
	}
	if ref.GetType().GetPlural() != "pods" || ref.GetType().GetGroup() != "core" {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported object type %s.%s",
			ref.GetType().GetPlural(), ref.GetType().GetGroup())
	}
	if ref.GetKey().GetName() == "" && ref.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "object reference is missing key and UID")
	}
	return ref, nil
}

// LastReference returns a copy of the most recent reference the fake decoded,
// so a test can assert the UID was sent alongside the key.
func (f *FakeBroker) LastReference() *brokerpb.KubernetesObjectReference {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastRef == nil {
		return nil
	}
	return proto.Clone(f.lastRef).(*brokerpb.KubernetesObjectReference)
}

func brokerKey(namespace, name string) string { return namespace + "/" + name }

// StartBroker serves a FakeBroker over mutual TLS on a temporary UDS and returns
// it with the socket path. The server presents serverID (a SPIRE agent identity
// by default) and accepts any client in ca's trust domain; the client is
// expected to authorize the server itself.
//
// os.MkdirTemp("") keeps the path inside the ~108-byte AF_UNIX budget, which a
// Bazel sandbox path would blow.
func StartBroker(t *testing.T, ca *CA, serverID string) (*FakeBroker, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "broker")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "broker.sock")

	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	fake := NewFakeBroker()
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(brokerServerTLS(t, ca, serverID))))
	brokerpb.RegisterAPIServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return fake, sock
}

// NewFakeBroker returns an empty fake with its tables initialised.
func NewFakeBroker() *FakeBroker {
	return &FakeBroker{
		entries:         make(map[string]*BrokerEntry),
		notFoundFor:     make(map[string]int),
		denyFor:         make(map[string]bool),
		closeAfterFirst: make(map[string]bool),
		pending:         make(map[string][]chan *brokerpb.SubscribeToX509SVIDResponse),
	}
}

// brokerServerTLS builds the server side of the mutually-authenticated endpoint.
func brokerServerTLS(t *testing.T, ca *CA, serverID string) *tls.Config {
	t.Helper()
	identity := ca.Identity(t, serverID)
	cfg := tlsconfig.MTLSServerConfig(identity, identity, tlsconfig.AuthorizeAny())
	// Session resumption would skip the peer-authorization callback on later
	// connections, exactly as SPIRE's own endpoint disables it.
	cfg.SessionTicketsDisabled = true
	return cfg
}
