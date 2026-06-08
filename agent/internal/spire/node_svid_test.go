package spire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestX509SVID builds a minimal go-spiffe X.509 SVID with the given SPIFFE ID,
// as the Workload API source would return for the agent's node identity.
func newTestX509SVID(t *testing.T, id string) *x509svid.SVID {
	t.Helper()

	sid := spiffeid.RequireFromString(id)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"SPIRE"}},
		URIs:                  []*url.URL{sid.URL()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &x509svid.SVID{
		ID:           sid,
		Certificates: []*x509.Certificate{cert},
		PrivateKey:   key,
	}
}

func TestX509SVIDToTLSCertificateSecret(t *testing.T) {
	id := "spiffe://example.org/ns/aether-system/sa/aether-agent"
	secret, err := X509SVIDToTLSCertificateSecret(newTestX509SVID(t, id))
	require.NoError(t, err)

	assert.Equal(t, id, secret.GetName(), "secret name must be the node SPIFFE ID")
	cert := secret.GetTlsCertificate()
	require.NotNil(t, cert)
	assert.Equal(t, "CERTIFICATE", pemBlockType(cert.GetCertificateChain().GetInlineBytes()))
	assert.Equal(t, "PRIVATE KEY", pemBlockType(cert.GetPrivateKey().GetInlineBytes()))
}

func TestX509SVIDToTLSCertificateSecret_Nil(t *testing.T) {
	_, err := X509SVIDToTLSCertificateSecret(nil)
	require.Error(t, err)
}

func TestSecretsEqual(t *testing.T) {
	a, err := X509SVIDToTLSCertificateSecret(newTestX509SVID(t, "spiffe://example.org/ns/x/sa/a"))
	require.NoError(t, err)

	assert.True(t, secretsEqual(a, a), "identical secrets compare equal")

	b, err := X509SVIDToTLSCertificateSecret(newTestX509SVID(t, "spiffe://example.org/ns/x/sa/a"))
	require.NoError(t, err)
	assert.False(t, secretsEqual(a, b), "a rotated cert (new key) compares unequal")
}
