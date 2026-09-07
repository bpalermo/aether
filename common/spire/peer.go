package spire

import "strings"

// peerIdentityMarkers are the substrings go-spiffe puts in a TLS handshake
// error when the PEER could not present a usable mesh identity — most often
// because the peer process is itself still waiting for SPIRE to issue its first
// SVID (#740). They are matched against the whole error string, so a gRPC status
// carrying the handshake failure in its message matches too:
//
//	rpc error: code = Unavailable desc = connection error: desc = "transport:
//	authentication handshake failed: x509svid: could not get X509 bundle"
//
// go-spiffe exposes nothing structural here — spiffetls/tlsconfig builds these
// with fmt.Errorf and x509svid's verifier wraps them again — so substring
// matching on the two markers is the discriminator. "x509svid:" is the package
// prefix every one of those errors carries; "could not get X509 bundle" is the
// exact shape observed on the rev210 roll and is kept explicitly so a future
// go-spiffe that drops the package prefix still classifies it.
var peerIdentityMarkers = []string{
	"could not get X509 bundle",
	"x509svid:",
}

// IsPeerIdentityUnavailable reports whether err is a TLS handshake that failed
// because the PEER has no mesh identity yet.
//
// It is deliberately about the peer, not about us: a caller that has not got its
// own SVID yet knows that locally (it holds the source) and should answer that
// question first — see the registrar client's identityPending. What is left for
// this to classify is the other side's startup, which is a transient of exactly
// the same kind: not a fault of either party, self-healing within seconds, and
// wrong to report as an error or to count against an error budget.
func IsPeerIdentityUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range peerIdentityMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
