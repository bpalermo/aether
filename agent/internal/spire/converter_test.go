package spire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	typesv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
)

// generateTestCert creates a self-signed X.509 certificate and returns its
// DER-encoded bytes along with the private key used to sign it.
func generateTestCert(t *testing.T) (derBytes []byte, privKey *ecdsa.PrivateKey) {
	t.Helper()

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test Org"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	derBytes, err = x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	require.NoError(t, err)

	return derBytes, privKey
}

// generateTestPKCS8Key generates a random ECDSA private key encoded in
// PKCS#8 DER format, as SPIRE would provide.
func generateTestPKCS8Key(t *testing.T) []byte {
	t.Helper()

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	derKey, err := x509.MarshalPKCS8PrivateKey(privKey)
	require.NoError(t, err)

	return derKey
}

// pemToCertType decodes a PEM block from the given bytes and returns the type
// header (e.g. "CERTIFICATE" or "PRIVATE KEY").
func pemBlockType(data []byte) string {
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	return block.Type
}

// countPEMBlocks returns the number of PEM blocks in the given byte slice.
func countPEMBlocks(data []byte) int {
	count := 0
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		count++
	}
	return count
}

func TestSVIDToTLSCertificateSecret(t *testing.T) {
	validDERCert, _ := generateTestCert(t)
	validDERKey := generateTestPKCS8Key(t)

	tests := []struct {
		name        string
		svid        *delegatedidentityv1.X509SVIDWithKey
		wantErr     bool
		wantErrMsg  string
		wantName    string
		wantCertPEM bool
		wantKeyPEM  bool
	}{
		{
			name: "converts valid SVID with trust domain and path",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid: &typesv1.X509SVID{
					Id: &typesv1.SPIFFEID{
						TrustDomain: "example.org",
						Path:        "/ns/default/sa/web",
					},
					CertChain: [][]byte{validDERCert},
				},
				X509SvidKey: validDERKey,
			},
			wantErr:     false,
			wantName:    "spiffe://example.org/ns/default/sa/web",
			wantCertPEM: true,
			wantKeyPEM:  true,
		},
		{
			name: "converts valid SVID with no path",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid: &typesv1.X509SVID{
					Id: &typesv1.SPIFFEID{
						TrustDomain: "example.org",
						Path:        "",
					},
					CertChain: [][]byte{validDERCert},
				},
				X509SvidKey: validDERKey,
			},
			wantErr:     false,
			wantName:    "spiffe://example.org",
			wantCertPEM: true,
			wantKeyPEM:  true,
		},
		{
			name: "converts valid SVID with multi-cert chain",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid: &typesv1.X509SVID{
					Id: &typesv1.SPIFFEID{
						TrustDomain: "cluster.local",
						Path:        "/workload/api",
					},
					CertChain: [][]byte{validDERCert, validDERCert},
				},
				X509SvidKey: validDERKey,
			},
			wantErr:     false,
			wantName:    "spiffe://cluster.local/workload/api",
			wantCertPEM: true,
			wantKeyPEM:  true,
		},
		{
			name:       "returns error when svid is nil",
			svid:       nil,
			wantErr:    true,
			wantErrMsg: "svid is nil",
		},
		{
			name: "returns error when x509_svid field is nil",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid:    nil,
				X509SvidKey: validDERKey,
			},
			wantErr:    true,
			wantErrMsg: "x509_svid is nil",
		},
		{
			name: "returns error when cert chain is empty",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid: &typesv1.X509SVID{
					Id: &typesv1.SPIFFEID{
						TrustDomain: "example.org",
						Path:        "/workload",
					},
					CertChain: [][]byte{},
				},
				X509SvidKey: validDERKey,
			},
			wantErr:    true,
			wantErrMsg: "converting cert chain to PEM",
		},
		{
			name: "returns error when private key is empty",
			svid: &delegatedidentityv1.X509SVIDWithKey{
				X509Svid: &typesv1.X509SVID{
					Id: &typesv1.SPIFFEID{
						TrustDomain: "example.org",
						Path:        "/workload",
					},
					CertChain: [][]byte{validDERCert},
				},
				X509SvidKey: []byte{},
			},
			wantErr:    true,
			wantErrMsg: "converting private key to PEM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SVIDToTLSCertificateSecret(tt.svid)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)

			assert.Equal(t, tt.wantName, got.GetName())

			tlsCert := got.GetTlsCertificate()
			require.NotNil(t, tlsCert)

			certChain := tlsCert.GetCertificateChain()
			require.NotNil(t, certChain)
			certBytes := certChain.GetInlineBytes()
			require.NotEmpty(t, certBytes)
			assert.Equal(t, "CERTIFICATE", pemBlockType(certBytes))

			privKey := tlsCert.GetPrivateKey()
			require.NotNil(t, privKey)
			keyBytes := privKey.GetInlineBytes()
			require.NotEmpty(t, keyBytes)
			assert.Equal(t, "PRIVATE KEY", pemBlockType(keyBytes))
		})
	}
}

func TestSVIDToTLSCertificateSecret_MultiCertChainCount(t *testing.T) {
	certDER1, _ := generateTestCert(t)
	certDER2, _ := generateTestCert(t)
	keyDER := generateTestPKCS8Key(t)

	svid := &delegatedidentityv1.X509SVIDWithKey{
		X509Svid: &typesv1.X509SVID{
			Id: &typesv1.SPIFFEID{
				TrustDomain: "example.org",
				Path:        "/workload",
			},
			CertChain: [][]byte{certDER1, certDER2},
		},
		X509SvidKey: keyDER,
	}

	got, err := SVIDToTLSCertificateSecret(svid)
	require.NoError(t, err)
	require.NotNil(t, got)

	certChainBytes := got.GetTlsCertificate().GetCertificateChain().GetInlineBytes()
	assert.Equal(t, 2, countPEMBlocks(certChainBytes), "expected 2 PEM certificate blocks for a 2-cert chain")
}

func TestBundleToValidationContextSecret(t *testing.T) {
	validDERCert, _ := generateTestCert(t)

	tests := []struct {
		name        string
		trustDomain string
		derCACerts  []byte
		wantErr     bool
		wantErrMsg  string
		wantName    string
	}{
		{
			name:        "converts valid DER CA bundle to validation context secret",
			trustDomain: "spiffe://example.org",
			derCACerts:  validDERCert,
			wantErr:     false,
			wantName:    "spiffe://example.org",
		},
		{
			name:        "uses trust domain as secret name verbatim",
			trustDomain: "example.org",
			derCACerts:  validDERCert,
			wantErr:     false,
			wantName:    "example.org",
		},
		{
			name:        "returns error when CA bundle is empty",
			trustDomain: "spiffe://example.org",
			derCACerts:  []byte{},
			wantErr:     true,
			wantErrMsg:  "converting CA bundle to PEM",
		},
		{
			name:        "returns error when CA bundle is nil",
			trustDomain: "spiffe://example.org",
			derCACerts:  nil,
			wantErr:     true,
			wantErrMsg:  "converting CA bundle to PEM",
		},
		{
			name:        "returns error when CA bundle contains malformed DER data",
			trustDomain: "spiffe://example.org",
			derCACerts:  []byte("not-valid-der-data"),
			wantErr:     true,
			wantErrMsg:  "converting CA bundle to PEM",
		},
		{
			name:        "includes trust domain in error message on failure",
			trustDomain: "spiffe://bad-domain.io",
			derCACerts:  []byte("garbage"),
			wantErr:     true,
			wantErrMsg:  "spiffe://bad-domain.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BundleToValidationContextSecret(tt.trustDomain, tt.derCACerts)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)

			assert.Equal(t, tt.wantName, got.GetName())

			validationCtx := got.GetValidationContext()
			require.NotNil(t, validationCtx)

			trustedCA := validationCtx.GetTrustedCa()
			require.NotNil(t, trustedCA)

			caBytes := trustedCA.GetInlineBytes()
			require.NotEmpty(t, caBytes)
			assert.Equal(t, "CERTIFICATE", pemBlockType(caBytes))
		})
	}
}

func TestDerCertChainToPEM(t *testing.T) {
	cert1DER, _ := generateTestCert(t)
	cert2DER, _ := generateTestCert(t)

	tests := []struct {
		name           string
		derCerts       [][]byte
		wantErr        bool
		wantErrMsg     string
		wantBlockCount int
		wantBlockType  string
	}{
		{
			name:           "converts single DER cert to PEM",
			derCerts:       [][]byte{cert1DER},
			wantErr:        false,
			wantBlockCount: 1,
			wantBlockType:  "CERTIFICATE",
		},
		{
			name:           "converts multiple DER certs to concatenated PEM",
			derCerts:       [][]byte{cert1DER, cert2DER},
			wantErr:        false,
			wantBlockCount: 2,
			wantBlockType:  "CERTIFICATE",
		},
		{
			name:           "preserves arbitrary bytes as PEM payload without parsing",
			derCerts:       [][]byte{[]byte("arbitrary-bytes")},
			wantErr:        false,
			wantBlockCount: 1,
			wantBlockType:  "CERTIFICATE",
		},
		{
			name:       "returns error for empty cert chain",
			derCerts:   [][]byte{},
			wantErr:    true,
			wantErrMsg: "empty certificate chain",
		},
		{
			name:       "returns error for nil cert chain",
			derCerts:   nil,
			wantErr:    true,
			wantErrMsg: "empty certificate chain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := derCertChainToPEM(tt.derCerts)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, got)
			assert.Equal(t, tt.wantBlockCount, countPEMBlocks(got))
			assert.Equal(t, tt.wantBlockType, pemBlockType(got))
			assert.True(t, strings.HasSuffix(string(got), "\n"), "PEM output should end with newline")
		})
	}
}

func TestDerKeyToPEM(t *testing.T) {
	validKeyDER := generateTestPKCS8Key(t)

	tests := []struct {
		name          string
		derKey        []byte
		wantErr       bool
		wantErrMsg    string
		wantBlockType string
	}{
		{
			name:          "converts PKCS8 DER key to PEM",
			derKey:        validKeyDER,
			wantErr:       false,
			wantBlockType: "PRIVATE KEY",
		},
		{
			name:          "wraps arbitrary bytes as PRIVATE KEY PEM without parsing",
			derKey:        []byte("any-bytes"),
			wantErr:       false,
			wantBlockType: "PRIVATE KEY",
		},
		{
			name:       "returns error for empty key",
			derKey:     []byte{},
			wantErr:    true,
			wantErrMsg: "empty private key",
		},
		{
			name:       "returns error for nil key",
			derKey:     nil,
			wantErr:    true,
			wantErrMsg: "empty private key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := derKeyToPEM(tt.derKey)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, got)
			assert.Equal(t, tt.wantBlockType, pemBlockType(got))
			assert.Equal(t, 1, countPEMBlocks(got), "expected exactly one PEM block for a single key")
		})
	}
}

func TestDerBundleToPEM(t *testing.T) {
	cert1DER, _ := generateTestCert(t)
	cert2DER, _ := generateTestCert(t)

	// Build a concatenated DER bundle with two certificates.
	twoCertBundle := append(cert1DER, cert2DER...)

	tests := []struct {
		name           string
		derBundle      []byte
		wantErr        bool
		wantErrMsg     string
		wantBlockCount int
		wantBlockType  string
	}{
		{
			name:           "converts single-cert DER bundle to PEM",
			derBundle:      cert1DER,
			wantErr:        false,
			wantBlockCount: 1,
			wantBlockType:  "CERTIFICATE",
		},
		{
			name:           "converts two-cert concatenated DER bundle to PEM",
			derBundle:      twoCertBundle,
			wantErr:        false,
			wantBlockCount: 2,
			wantBlockType:  "CERTIFICATE",
		},
		{
			name:       "returns error for empty bundle",
			derBundle:  []byte{},
			wantErr:    true,
			wantErrMsg: "empty CA bundle",
		},
		{
			name:       "returns error for nil bundle",
			derBundle:  nil,
			wantErr:    true,
			wantErrMsg: "empty CA bundle",
		},
		{
			name:       "returns error for malformed DER data",
			derBundle:  []byte("not-valid-der-data"),
			wantErr:    true,
			wantErrMsg: "parsing DER certificates",
		},
		{
			name:       "returns error for truncated DER data",
			derBundle:  cert1DER[:len(cert1DER)/2],
			wantErr:    true,
			wantErrMsg: "parsing DER certificates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := derBundleToPEM(tt.derBundle)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				assert.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, got)
			assert.Equal(t, tt.wantBlockCount, countPEMBlocks(got))
			assert.Equal(t, tt.wantBlockType, pemBlockType(got))
		})
	}
}
