// Package spiretest provides a fake SPIRE Workload API server for tests.
//
// It exists so every binary that waits for an SVID (issue #740) can prove the
// same two things hermetically, without a container or a real SPIRE agent: that
// startup returns immediately while the Workload API is refusing to attest, and
// that identity is folded in the moment it starts serving. The refusal is a
// switch (StartServing) rather than a delay, because the window being reproduced
// is exactly "the agent socket is there, but no identity has been issued yet".
package spiretest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TrustDomain is the trust domain of every SVID this package mints.
const TrustDomain = "example.org"

// FakeWorkloadAPI is a SPIRE Workload API server whose FetchX509SVID refuses to
// serve — exactly as a SPIRE agent that is up but cannot attest the workload yet
// — until StartServing is called.
type FakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	spiffeID string
	svid     *workload.X509SVID

	serving   atomic.Bool
	fetches   atomic.Int64
	badHeader atomic.Int64
}

// StartServing makes the fake issue its SVID to every subsequent fetch.
func (f *FakeWorkloadAPI) StartServing() { f.serving.Store(true) }

// SpiffeID is the SPIFFE ID of the SVID this fake serves.
func (f *FakeWorkloadAPI) SpiffeID() string { return f.spiffeID }

// Fetches counts the FetchX509SVID calls received so far.
func (f *FakeWorkloadAPI) Fetches() int64 { return f.fetches.Load() }

// BadHeaders counts calls that arrived without the Workload API security header.
// A real SPIRE agent rejects those, so a non-zero count means the client is
// misconfigured.
func (f *FakeWorkloadAPI) BadHeaders() int64 { return f.badHeader.Load() }

// FetchX509SVID implements the Workload API.
func (f *FakeWorkloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	f.fetches.Add(1)

	// The Workload API's security header. go-spiffe sets it on every call; a
	// real SPIRE agent rejects calls without it, so the fake asserts it too.
	md, _ := metadata.FromIncomingContext(stream.Context())
	if values := md.Get("workload.spiffe.io"); len(values) != 1 || values[0] != "true" {
		f.badHeader.Add(1)
		return status.Error(codes.InvalidArgument, "missing workload.spiffe.io header")
	}

	if !f.serving.Load() {
		return status.Error(codes.Unavailable, "no identity issued for this workload yet")
	}
	if err := stream.Send(&workload.X509SVIDResponse{Svids: []*workload.X509SVID{f.svid}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

// Start serves a FakeWorkloadAPI carrying spiffeID on a temporary UDS and returns
// it with the socket path. It is not serving yet: call StartServing to release
// the SVID.
//
// os.MkdirTemp("") keeps the path inside the ~108-byte AF_UNIX budget, which a
// Bazel sandbox path would blow (same reason as agent/internal/spire's fake SPIRE
// agent).
func Start(t *testing.T, spiffeID string) (*FakeWorkloadAPI, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "wlapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "workload.sock")

	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	fake := &FakeWorkloadAPI{spiffeID: spiffeID, svid: NewWorkloadSVID(t, spiffeID)}
	srv := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return fake, sock
}

// UnservedSocket returns a path inside a temporary directory that nothing is
// listening on: the "SPIRE is not there at all" half of the #740 matrix.
func UnservedSocket(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "wlapi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "absent.sock")
}

// NewWorkloadSVID mints a CA plus a leaf carrying the SPIFFE ID as its URI SAN
// and marshals them the way the Workload API returns them (DER chain, PKCS#8
// key, DER bundle), so go-spiffe's own parser accepts them.
func NewWorkloadSVID(t *testing.T, id string) *workload.X509SVID {
	t.Helper()

	sid := spiffeid.RequireFromString(id)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"SPIRE test CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{Organization: []string{"SPIRE test leaf"}},
		URIs:                  []*url.URL{sid.URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	require.NoError(t, err)

	return &workload.X509SVID{
		SpiffeId:    id,
		X509Svid:    leafDER,
		X509SvidKey: keyDER,
		Bundle:      caDER,
	}
}
