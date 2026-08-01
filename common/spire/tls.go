// Package spire provides utilities for building mutual-TLS configurations backed
// by the SPIRE Workload API. The X.509 SVID and trust bundle are fetched in
// memory over the Workload API socket (mounted by the csi.spiffe.io driver) and
// kept up to date automatically — no certificate files are read from disk.
package spire

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Source is an auto-rotating X.509 SVID source backed by the Workload API.
// Callers own the returned source and must Close it on shutdown.
type Source = workloadapi.X509Source

// NewSource connects to the SPIRE Workload API at socketPath and returns an
// X.509 SVID source that watches for rotations. socketPath may be a filesystem
// path to the agent UDS (e.g. /run/secrets/workload-spiffe-uds/socket) or a
// full endpoint address (unix://… / tcp://…).
func NewSource(ctx context.Context, socketPath string) (*Source, error) {
	src, err := workloadapi.NewX509Source(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socketAddr(socketPath))))
	if err != nil {
		return nil, fmt.Errorf("failed to create SPIRE Workload API source: %w", err)
	}
	return src, nil
}

// RootCATrustDomain is the sentinel trust-domain value meaning "authorize any
// peer presenting an SVID that chains to the SPIRE root CA bundle", without
// restricting the SPIFFE ID's trust domain. An empty trust domain is treated the
// same way.
const RootCATrustDomain = "ROOTCA"

// TrustDomainFromSource returns the SPIFFE trust domain name of the workload SVID
// served by src (e.g. "example.org"). Use it to construct SPIFFE IDs and SDS
// resource names from the real trust domain SPIRE issues into, rather than from a
// configured value (which may be the RootCATrustDomain authorization sentinel).
// The source has already fetched its first SVID by the time NewSource returns.
func TrustDomainFromSource(src *Source) (string, error) {
	svid, err := src.GetX509SVID()
	if err != nil {
		return "", fmt.Errorf("fetching workload SVID for trust domain: %w", err)
	}
	return svid.ID.TrustDomain().Name(), nil
}

// ServerTLSConfig returns a mutual-TLS server config that presents the workload
// SVID from src and authorizes peers belonging to trustDomain (or any valid peer
// when trustDomain is empty / RootCATrustDomain).
func ServerTLSConfig(src *Source, trustDomain string) (*tls.Config, error) {
	auth, err := authorizer(trustDomain)
	if err != nil {
		return nil, err
	}
	return tlsconfig.MTLSServerConfig(src, src, auth), nil
}

// WebhookServerCert returns a tls.Config mutator (suitable for
// controller-runtime's webhook.Options.TLSOpts) that makes the server present the
// workload SVID from src via GetCertificate, with no client-certificate
// requirement. The caller of a webhook is the kube-apiserver — not a SPIFFE peer
// — so this is one-way TLS: the apiserver verifies the SVID against the SPIRE
// trust bundle (injected as the webhook caBundle) and the Service DNS name, which
// the SVID must carry as a DNS SAN (set dnsNames on the SPIRE registration
// entry). Setting GetCertificate here makes controller-runtime skip its CertDir
// file watcher entirely.
func WebhookServerCert(src *Source) func(*tls.Config) {
	return func(cfg *tls.Config) {
		cfg.GetCertificate = tlsconfig.GetCertificate(src)
		cfg.ClientAuth = tls.NoClientCert
		if cfg.MinVersion < tls.VersionTLS12 {
			cfg.MinVersion = tls.VersionTLS12
		}
	}
}

// TrustBundlePEM returns the PEM-encoded X.509 trust bundle for the SVID's own
// trust domain, suitable for use as a webhook/CRD caBundle. It reflects the
// current bundle in src and should be re-read after each rotation (src.Updated()).
func TrustBundlePEM(src *Source) ([]byte, error) {
	svid, err := src.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("fetching workload SVID for trust bundle: %w", err)
	}
	bundle, err := src.GetX509BundleForTrustDomain(svid.ID.TrustDomain())
	if err != nil {
		return nil, fmt.Errorf("fetching X.509 bundle: %w", err)
	}
	pem, err := bundle.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshalling X.509 bundle: %w", err)
	}
	return pem, nil
}

// ClientTLSConfig returns a mutual-TLS client config that presents the workload
// SVID from src and authorizes peers belonging to trustDomain (or any valid peer
// when trustDomain is empty / RootCATrustDomain).
func ClientTLSConfig(src *Source, trustDomain string) (*tls.Config, error) {
	auth, err := authorizer(trustDomain)
	if err != nil {
		return nil, err
	}
	return tlsconfig.MTLSClientConfig(src, src, auth), nil
}

// authorizer builds the peer authorizer for the given trust domain. The empty
// string and RootCATrustDomain both authorize any valid SVID; otherwise the peer
// must belong to the named trust domain.
func authorizer(trustDomain string) (tlsconfig.Authorizer, error) {
	if trustDomain == "" || trustDomain == RootCATrustDomain {
		return tlsconfig.AuthorizeAny(), nil
	}
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("invalid SPIRE trust domain %q: %w", trustDomain, err)
	}
	return tlsconfig.AuthorizeMemberOf(td), nil
}

// socketAddr normalises a Workload API endpoint, defaulting a bare filesystem
// path to a unix:// address.
func socketAddr(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	return "unix://" + path
}
