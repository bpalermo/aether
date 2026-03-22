// Package spire provides utilities for loading SPIRE X.509 SVID certificates
// for gRPC TLS configuration. It reads certificates from the SPIRE CSI-mounted
// directory (svid.pem, svid_key.pem, svid_bundle.pem).
package spire

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
)

// ServerTLSConfig loads the SPIRE X.509 SVID certificate, key, and CA bundle
// from certDir and returns a tls.Config for a gRPC server with mutual TLS.
func ServerTLSConfig(certDir string) (*tls.Config, error) {
	cert, pool, err := loadCertAndBundle(certDir)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig loads the SPIRE X.509 SVID certificate, key, and CA bundle
// from certDir and returns a tls.Config for a gRPC client with mutual TLS.
func ClientTLSConfig(certDir string) (*tls.Config, error) {
	cert, pool, err := loadCertAndBundle(certDir)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func loadCertAndBundle(certDir string) (tls.Certificate, *x509.CertPool, error) {
	certFile := filepath.Join(certDir, "svid.pem")
	keyFile := filepath.Join(certDir, "svid_key.pem")
	bundleFile := filepath.Join(certDir, "svid_bundle.pem")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("failed to load SPIRE SVID keypair: %w", err)
	}

	bundlePEM, err := os.ReadFile(bundleFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("failed to read SPIRE bundle: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundlePEM) {
		return tls.Certificate{}, nil, fmt.Errorf("failed to parse SPIRE bundle certificates")
	}

	return cert, pool, nil
}
