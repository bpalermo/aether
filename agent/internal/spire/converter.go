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
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
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

// X509SVIDToTLSCertificateSecret converts a go-spiffe X.509 SVID (as returned by
// the Workload API source) to an Envoy TLS certificate Secret. The secret name is
// the full SPIFFE ID URI. This is used to serve the agent's own node identity to
// the proxy for node-originated upstream mTLS and the node-health listener, distinct
// from the per-pod workload SVIDs served via the Delegated Identity API.
func X509SVIDToTLSCertificateSecret(svid *x509svid.SVID) (*tlsv3.Secret, error) {
	if svid == nil {
		return nil, fmt.Errorf("svid is nil")
	}

	certPEM, keyPEM, err := svid.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshaling X.509 SVID: %w", err)
	}

	return &tlsv3.Secret{
		Name: svid.ID.String(),
		Type: &tlsv3.Secret_TlsCertificate{
			TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: certPEM},
				},
				PrivateKey: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: keyPEM},
				},
			},
		},
	}, nil
}

// BundleToValidationContextSecret converts a trust domain's CA certificates to
// an Envoy validation context Secret. trustDomain may be a bare trust domain
// name ("example.org") or a SPIFFE URI ("spiffe://example.org") — the SPIRE
// Delegated Identity bundle map is keyed by the latter. Either way the secret is
// named with the canonical SPIFFE URI (e.g. "spiffe://example.org") to match the
// validation context name referenced by inbound listeners and the SVID secret
// naming. The DER-encoded CA certs are PEM-encoded.
func BundleToValidationContextSecret(trustDomain string, derCACerts []byte) (*tlsv3.Secret, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("invalid trust domain %q: %w", trustDomain, err)
	}

	caPEM, err := derBundleToPEM(derCACerts)
	if err != nil {
		return nil, fmt.Errorf("converting CA bundle to PEM for %s: %w", trustDomain, err)
	}

	return &tlsv3.Secret{
		Name: td.IDString(),
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
