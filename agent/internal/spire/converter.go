// Package spire provides a bridge between the SPIRE Delegated Identity API
// and Envoy's Secret Discovery Service (SDS) via go-control-plane.
//
// The bridge subscribes to X.509 SVIDs and trust bundles from the SPIRE Agent's
// admin socket using the Delegated Identity API, converts them to Envoy Secret
// resources, and pushes them into the xDS snapshot cache for delivery to Envoy
// proxies via ADS.
package spire

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
)

// SVIDToTLSCertificateSecret converts a SPIRE X509SVIDWithKey to an Envoy
// TLS certificate Secret. The secret name is the full SPIFFE ID URI.
// DER-encoded cert chain and PKCS#8 private key are PEM-encoded for Envoy.
func SVIDToTLSCertificateSecret(svid *delegatedidentityv1.X509SVIDWithKey) (*tlsv3.Secret, error) {
	if svid == nil {
		return nil, fmt.Errorf("svid is nil")
	}

	x509Svid := svid.GetX509Svid()
	if x509Svid == nil {
		return nil, fmt.Errorf("x509_svid is nil")
	}

	spiffeID := fmt.Sprintf("spiffe://%s%s", x509Svid.GetId().GetTrustDomain(), x509Svid.GetId().GetPath())

	certChainPEM, err := derCertChainToPEM(x509Svid.GetCertChain())
	if err != nil {
		return nil, fmt.Errorf("converting cert chain to PEM: %w", err)
	}

	keyPEM, err := derKeyToPEM(svid.GetX509SvidKey())
	if err != nil {
		return nil, fmt.Errorf("converting private key to PEM: %w", err)
	}

	return &tlsv3.Secret{
		Name: spiffeID,
		Type: &tlsv3.Secret_TlsCertificate{
			TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: certChainPEM},
				},
				PrivateKey: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: keyPEM},
				},
			},
		},
	}, nil
}

// BundleToValidationContextSecret converts a trust domain's CA certificates
// to an Envoy validation context Secret. The secret name is the trust domain
// URI (e.g., "spiffe://example.org"). The DER-encoded CA certs are PEM-encoded.
func BundleToValidationContextSecret(trustDomain string, derCACerts []byte) (*tlsv3.Secret, error) {
	caPEM, err := derBundleToPEM(derCACerts)
	if err != nil {
		return nil, fmt.Errorf("converting CA bundle to PEM for %s: %w", trustDomain, err)
	}

	return &tlsv3.Secret{
		Name: trustDomain,
		Type: &tlsv3.Secret_ValidationContext{
			ValidationContext: &tlsv3.CertificateValidationContext{
				TrustedCa: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: caPEM},
				},
			},
		},
	}, nil
}

// derCertChainToPEM converts a slice of DER-encoded certificates to
// concatenated PEM blocks.
func derCertChainToPEM(derCerts [][]byte) ([]byte, error) {
	if len(derCerts) == 0 {
		return nil, fmt.Errorf("empty certificate chain")
	}

	var pemBytes []byte
	for _, der := range derCerts {
		pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: der,
		})...)
	}
	return pemBytes, nil
}

// derKeyToPEM converts a DER-encoded PKCS#8 private key to PEM.
func derKeyToPEM(derKey []byte) ([]byte, error) {
	if len(derKey) == 0 {
		return nil, fmt.Errorf("empty private key")
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: derKey,
	}), nil
}

// derBundleToPEM parses concatenated DER-encoded X.509 certificates and
// returns them as concatenated PEM blocks.
func derBundleToPEM(derBundle []byte) ([]byte, error) {
	if len(derBundle) == 0 {
		return nil, fmt.Errorf("empty CA bundle")
	}

	certs, err := x509.ParseCertificates(derBundle)
	if err != nil {
		return nil, fmt.Errorf("parsing DER certificates: %w", err)
	}

	var pemBytes []byte
	for _, cert := range certs {
		pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert.Raw,
		})...)
	}
	return pemBytes, nil
}
