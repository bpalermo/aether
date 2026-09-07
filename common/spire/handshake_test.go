package spire

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errBundle is the string every mTLS failure in this area arrives as, whichever
// party was short of an identity: go-spiffe's verifier raises it against the
// LOCAL bundle source (svid/x509svid/verify.go), so it names nobody.
var errBundle = status.Error(codes.Unavailable,
	`connection error: desc = "transport: authentication handshake failed: x509svid: could not get X509 bundle"`)

// TestClassifyHandshake is issue #740, PR 5: the rev211 deploy roll
// (2026-09-07 20:47Z) produced this exact error twice on main-worker-02 within
// 600ms, and both times it was attributed to the registrar — once while the
// AGENT had no SVID, once while the agent's own connection was still
// re-establishing itself after acquiring one. The registrar had had its identity
// for a minute by then. Order of interrogation, not error text, is what tells
// the three apart.
func TestClassifyHandshake(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		identityReady bool
		sinceIdentity time.Duration
		want          HandshakeClass
	}{
		{
			// 20:47:27.338 on the rev211 roll: logged as "the registrar has no
			// identity yet" while it was this agent's own SVID that was pending.
			name:          "our own identity is pending",
			err:           errBundle,
			identityReady: false,
			sinceIdentity: -1,
			want:          HandshakeOwnIdentityPending,
		},
		{
			// Our own pending identity outranks everything, including an error
			// that says nothing about identity at all: without a certificate
			// this client learns nothing about the far side.
			name:          "our own identity is pending, whatever the error says",
			err:           errors.New("connection refused"),
			identityReady: false,
			sinceIdentity: -1,
			want:          HandshakeOwnIdentityPending,
		},
		{
			// 20:47:27.912: 85ms after the SVID landed, ResetConnectBackoff
			// already called, the watch stream 1.1s from connecting.
			name:          "identity just arrived and the connection is catching up",
			err:           errBundle,
			identityReady: true,
			sinceIdentity: 85 * time.Millisecond,
			want:          HandshakeReconnecting,
		},
		{
			name:          "a plain Unavailable inside the reconnect window is the reconnect",
			err:           status.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing: dial tcp: connect: connection refused\""),
			identityReady: true,
			sinceIdentity: time.Second,
			want:          HandshakeReconnecting,
		},
		{
			name:          "the window is bounded",
			err:           errBundle,
			identityReady: true,
			sinceIdentity: ReconnectWindow + time.Millisecond,
			want:          HandshakePeerIdentityPending,
		},
		{
			// PR 4's case, now reached only when it is the only one left: our
			// identity is in hand and has been for a while.
			name:          "the peer has no identity yet",
			err:           errBundle,
			identityReady: true,
			sinceIdentity: time.Minute,
			want:          HandshakePeerIdentityPending,
		},
		{
			// SPIRE disabled, or a client never told when identity arrived: no
			// window, and the pre-#740 classification stands.
			name:          "an unknown identity age disables the window",
			err:           errBundle,
			identityReady: true,
			sinceIdentity: -1,
			want:          HandshakePeerIdentityPending,
		},
		{
			// The condition the ERROR path exists for (#700) must survive all
			// three deferrals once identity is settled.
			name:          "a registrar that will not serve is still a failure",
			err:           status.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing: dial tcp: connect: connection refused\""),
			identityReady: true,
			sinceIdentity: time.Minute,
			want:          HandshakeFailure,
		},
		{
			name:          "a non-transport failure is still a failure",
			err:           errors.New("registry: malformed response"),
			identityReady: true,
			sinceIdentity: time.Millisecond,
			want:          HandshakeFailure,
		},
		{
			name:          "nil is not a failure to classify",
			err:           nil,
			identityReady: false,
			sinceIdentity: -1,
			want:          HandshakeFailure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyHandshake(tc.err, tc.identityReady, tc.sinceIdentity))
		})
	}
}
