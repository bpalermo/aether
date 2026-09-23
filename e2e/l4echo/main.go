// Command l4echo is the test backend for the L4 (Gateway API TCPRoute /
// TLSRoute / UDPRoute) e2e harness, e2e/l4routes.sh.
//
// Every mode answers with the same marker text, so the harness can tell which
// workload served a probe purely from the payload it read back. That payload
// marker is the primary assertion vehicle: it needs no Envoy admin access, it
// survives a proxy hot restart (raw counters do not), and it distinguishes
// "the weighted split picked backend B" from "the connection failed" without
// interpreting an exit code.
//
// Modes (the harness deploys one process per workload):
//
//	--mode=tcp  (:9000)  read one line, write "<text> <line>\n", CLOSE.
//	--mode=tls  (:9443)  HTTPS with a self-signed cert minted at start-up;
//	                     every request answers "<text>\n".
//	--mode=udp  (:9001)  reply "<text> <datagram>" to the sender.
//
// All three modes are exercised: --mode=tcp by the TCPRoute leg, --mode=tls by
// the TLSRoute leg (which needs the PARENT to speak TLS too, so that an
// unmatched SNI falling through to the floor can be asserted positively rather
// than as an absence), and --mode=udp by the UDPRoute leg.
//
// Closing after the echo is deliberate and is the whole reason this is not
// istio/tcp-echo-server: that image holds the connection open, so a probe can
// only be scored from the captured payload after a full client timeout. Every
// probe here costs one round trip.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	// Per-mode default listen addresses. The harness relies on these being the
	// pod's endpoint.aether.io/port for the tcp/tls modes: the mesh advertises
	// the endpoint at that port and the "tcp:" cluster dials it directly.
	defaultTCPListen = ":9000"
	defaultTLSListen = ":9443"
	defaultUDPListen = ":9001"

	// readTimeout bounds a client that connects and then says nothing, so a
	// stuck probe cannot pin a goroutine for the life of the pod.
	readTimeout = 30 * time.Second

	// maxDatagram is the largest UDP payload read in one go; the harness sends
	// short markers.
	maxDatagram = 2048

	// certLifetime for the self-signed TLS cert. Longer than any e2e run.
	certLifetime = 24 * time.Hour
)

func main() {
	mode := flag.String("mode", "tcp", "listener mode: tcp, tls, or udp")
	listen := flag.String("listen", "", "listen address (default: :9000 tcp, :9443 tls, :9001 udp)")
	text := flag.String("text", "l4echo", "marker written back to the caller so probes can tell workloads apart")
	// SANs are only meaningful for --mode=tls: the TLSRoute leg dials the
	// backend with an SNI of its own choosing, and Go's client would reject a
	// cert that does not cover it (the harness uses -k, but a real client and
	// any future strict probe should not have to).
	sans := flag.String("tls-sans", "", "comma-separated DNS SANs for the self-signed cert (--mode=tls only)")
	flag.Parse()

	addr := *listen
	if addr == "" {
		switch *mode {
		case "tcp":
			addr = defaultTCPListen
		case "tls":
			addr = defaultTLSListen
		case "udp":
			addr = defaultUDPListen
		}
	}

	if err := run(*mode, addr, *text, splitSANs(*sans)); err != nil {
		log.Fatalf("l4echo: %v", err)
	}
}

func splitSANs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func run(mode, addr, text string, sans []string) error {
	host, _ := os.Hostname()
	log.Printf("l4echo: mode=%s addr=%s text=%q pod=%s", mode, addr, text, host)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	switch mode {
	case "tcp":
		return serveTCP(addr, text, stop)
	case "tls":
		return serveTLS(addr, text, sans, stop)
	case "udp":
		return serveUDP(addr, text, stop)
	default:
		return fmt.Errorf("unknown --mode %q (want tcp, tls, or udp)", mode)
	}
}

// serveTCP reads one newline-terminated line per connection, echoes
// "<text> <line>" and closes. Closing is what lets the harness score a probe
// from the payload alone, with no client-side timeout in the happy path.
func serveTCP(addr, text string, stop <-chan os.Signal) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", addr, err)
	}
	go closeOnSignal(ln, stop)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go echoOnce(conn, text)
	}
}

func echoOnce(conn net.Conn, text string) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(readTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	// A client that closes without a newline still gets an answer for whatever
	// it did send — the reply, not the request framing, is what is asserted on.
	if err != nil && line == "" {
		return
	}
	_, _ = fmt.Fprintf(conn, "%s %s\n", text, strings.TrimRight(line, "\r\n"))
}

// serveTLS answers every request with the marker over HTTPS. The cert is minted
// at start-up and covers the supplied SANs plus the pod hostname; the harness
// dials with -k, so the cert is about completing the handshake, not identity.
func serveTLS(addr, text string, sans []string, stop <-chan os.Signal) error {
	cert, err := selfSignedCert(sans)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s\n", text)
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServeTLS("", "")
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case err := <-errCh:
		return err
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

func selfSignedCert(sans []string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}
	host, _ := os.Hostname()
	dns := append([]string{"localhost"}, sans...)
	if host != "" {
		dns = append(dns, host)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "l4echo"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dns,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// serveUDP echoes "<text> <datagram>" back to the sender.
func serveUDP(addr, text string, stop <-chan os.Signal) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", addr, err)
	}
	go closeOnSignal(conn, stop)

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		reply := fmt.Sprintf("%s %s\n", text, strings.TrimRight(string(buf[:n]), "\r\n"))
		if _, err := conn.WriteToUDP([]byte(reply), from); err != nil {
			log.Printf("l4echo: reply to %s failed: %v", from, err)
		}
	}
}

// closeOnSignal closes the listener on SIGTERM so the accept loop returns and
// the process exits 0 instead of being SIGKILLed at the end of the grace period.
func closeOnSignal(c interface{ Close() error }, stop <-chan os.Signal) {
	sig := <-stop
	log.Printf("l4echo: got %s, closing listener", sig)
	_ = c.Close()
}
