// Package spire provides a bridge between the SPIFFE Broker API and Envoy's
// Secret Discovery Service (SDS) via go-control-plane.
//
// The bridge subscribes to X.509 SVIDs for the pods on this node through the
// SPIRE agent's SPIFFE Broker Endpoint, builds validation contexts from the
// agent's own Workload API bundle plus the federated bundles those streams
// carry, converts both to Envoy Secret resources, and pushes them into the xDS
// snapshot cache for delivery to Envoy proxies via ADS.
package spire

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// SVIDToTLSCertificateSecret converts a SPIFFE Broker API X509SVID to an Envoy
// TLS certificate Secret. The secret name is the full SPIFFE ID URI, exactly as
// the broker reports it.
//
// Both wire fields are single byte strings, not lists: x509_svid is the ASN.1
// DER certificate chain with the leaf FIRST and any intermediates concatenated
// after it, and x509_svid_key is an unencrypted PKCS#8 DER private key
// (brokerapi.proto, X509SVID). Envoy wants PEM, so the chain is parsed and
// re-emitted as one PEM block per certificate, preserving order.
func SVIDToTLSCertificateSecret(svid *brokerpb.X509SVID) (*tlsv3.Secret, error) {
	if svid == nil {
		return nil, fmt.Errorf("svid is nil")
	}

	spiffeID := svid.GetSpiffeId()
	if spiffeID == "" {
		return nil, fmt.Errorf("spiffe_id is empty")
	}
	if _, err := spiffeid.FromString(spiffeID); err != nil {
		return nil, fmt.Errorf("invalid spiffe_id %q: %w", spiffeID, err)
	}

	certChainPEM, err := derCertChainToPEM(svid.GetX509Svid())
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
// from the per-pod workload SVIDs served via the SPIFFE Broker API.
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
// name ("example.org") or a SPIFFE URI ("spiffe://example.org") — the Broker
// API's federated_bundles map is keyed by the latter. Either way the secret is
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

// derCertChainToPEM converts a Broker API certificate chain — concatenated
// ASN.1 DER certificates in one byte string, leaf first — to concatenated PEM
// blocks in the same order.
//
// Unlike the old delegated-identity shape (a repeated bytes field, one entry per
// certificate) the boundaries are not given, so the chain MUST be parsed to be
// split. Parsing is not optional anyway: a chain Envoy cannot load is better
// rejected here, where the error names the SVID, than accepted into SDS.
func derCertChainToPEM(derChain []byte) ([]byte, error) {
	if len(derChain) == 0 {
		return nil, fmt.Errorf("empty certificate chain")
	}

	certs, err := x509.ParseCertificates(derChain)
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
