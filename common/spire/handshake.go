package spire

import (
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReconnectWindow is how long after this workload's own identity arrives a
// failed mTLS attempt is still attributed to the connection catching up rather
// than to either party's identity.
//
// It exists because the error text cannot answer the question. Every attempt
// made before the SVID landed failed the handshake, so the gRPC ClientConn has
// a cached transport failure and a backoff of its own; the next RPC is answered
// from that cache — with the PRE-identity error string — for as long as it takes
// the connection to redial. #740's wake (NotifyIdentityReady →
// ResetConnectBackoff) makes that a redial rather than a wait, but not an
// instant one: on the rev211 deploy roll (2026-09-07 20:47Z) the reset landed at
// 20:47:27.911 and the watch stream connected at 20:47:29.017, and every failure
// in between carried an error written before identity existed.
//
// A few seconds, therefore: long enough to cover a redial plus a handshake,
// short enough that a registrar which is genuinely down is misreported for one
// log line and then escalates normally.
const ReconnectWindow = 5 * time.Second

// HandshakeClass says why an mTLS attempt against a mesh peer failed, in the
// only order that is sound (see ClassifyHandshake).
type HandshakeClass int

const (
	// HandshakeFailure is a real failure: log it, count it, escalate it. The
	// zero value, so an unclassifiable error keeps the alarm it had before.
	HandshakeFailure HandshakeClass = iota
	// HandshakeOwnIdentityPending: this workload has no SVID yet, so it cannot
	// present a certificate or verify the peer's. Nothing about the peer is
	// known from such an attempt.
	HandshakeOwnIdentityPending
	// HandshakeReconnecting: identity arrived moments ago and the connection
	// has not finished re-establishing itself on top of it.
	HandshakeReconnecting
	// HandshakePeerIdentityPending: our identity is in hand and settled, and
	// the handshake still fails the way a peer without an SVID makes it fail.
	HandshakePeerIdentityPending
)

// ClassifyHandshake classifies a failed mTLS attempt from what the caller knows
// LOCALLY — whether its own identity is ready, and how long it has been ready —
// before falling back to the error text.
//
// The order is the whole point, and it is the order in which the answers are
// trustworthy:
//
//  1. Our own identity. A workload without an SVID fails every handshake for a
//     reason that has nothing to do with the far side.
//  2. The reconnect window. Right after identity arrives the transport is still
//     carrying the failure it collected before, so an Unavailable here is the
//     connection catching up (see ReconnectWindow).
//  3. The peer. Only once our identity is in hand AND settled does a handshake
//     failure say anything about the other side.
//
// Why local state and not the error text: the marker
// `x509svid: could not get X509 bundle` that IsPeerIdentityUnavailable matches
// is raised by go-spiffe's own verifier —
// svid/x509svid/verify.go, `bundleSource.GetX509BundleForTrustDomain` — against
// the LOCAL bundle source, so the identical string is produced whether it was
// our source or the peer's process that was short. (A peer that cannot present
// a certificate at all reaches us as a `remote error: tls: …` alert instead.)
// The text names no party, and after a failed pre-identity attempt it is not
// even current. That leaves local state as the only sound discriminator, which
// is what this function keys on.
//
// sinceIdentity is how long ago this workload's identity became usable; pass a
// negative duration when that is unknown (SPIRE disabled, or a client never told
// about identity) to disable the reconnect window. A nil err is not a failure
// and classifies as HandshakeFailure — callers only call this on an error.
func ClassifyHandshake(err error, identityReady bool, sinceIdentity time.Duration) HandshakeClass {
	if err == nil {
		return HandshakeFailure
	}
	if !identityReady {
		return HandshakeOwnIdentityPending
	}
	if sinceIdentity >= 0 && sinceIdentity <= ReconnectWindow && isTransportUnavailable(err) {
		return HandshakeReconnecting
	}
	if IsPeerIdentityUnavailable(err) {
		return HandshakePeerIdentityPending
	}
	return HandshakeFailure
}

// isTransportUnavailable reports whether err is the shape a connection that is
// not up (yet) fails RPCs with: a gRPC Unavailable status, or a handshake error
// that never got as far as a status.
func isTransportUnavailable(err error) bool {
	return status.Code(err) == codes.Unavailable || IsPeerIdentityUnavailable(err)
}
