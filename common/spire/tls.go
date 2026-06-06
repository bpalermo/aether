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

// ServerTLSConfig returns a mutual-TLS server config that presents the workload
// SVID from src and authorizes peers belonging to trustDomain.
func ServerTLSConfig(src *Source, trustDomain spiffeid.TrustDomain) *tls.Config {
	return tlsconfig.MTLSServerConfig(src, src, tlsconfig.AuthorizeMemberOf(trustDomain))
}

// ClientTLSConfig returns a mutual-TLS client config that presents the workload
// SVID from src and authorizes peers belonging to trustDomain.
func ClientTLSConfig(src *Source, trustDomain spiffeid.TrustDomain) *tls.Config {
	return tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeMemberOf(trustDomain))
}

// socketAddr normalises a Workload API endpoint, defaulting a bare filesystem
// path to a unix:// address.
func socketAddr(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	return "unix://" + path
}
