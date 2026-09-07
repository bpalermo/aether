package spire

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestIsPeerIdentityUnavailable pins the discriminator against the shapes the
// error actually arrives in. The first case is the literal one observed on the
// rev210 roll (2026-09-07 20:03:45Z) — a gRPC status wrapping a handshake that
// the registrar's side rejected because it had no SVID yet.
func TestIsPeerIdentityUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil is not a peer identity failure",
			err:  nil,
		},
		{
			name: "the gRPC status observed on the rev210 roll",
			err: status.Error(codes.Unavailable,
				`connection error: desc = "transport: authentication handshake failed: x509svid: could not get X509 bundle"`),
			want: true,
		},
		{
			name: "the bare go-spiffe verifier error",
			err:  errors.New("x509svid: could not get X509 bundle for trust domain aether.internal"),
			want: true,
		},
		{
			name: "the bundle phrase without the package prefix",
			err:  errors.New("could not get X509 bundle"),
			want: true,
		},
		{
			name: "any x509svid error counts",
			err:  fmt.Errorf("wrapped: %w", errors.New("x509svid: no X509SVIDs received")),
			want: true,
		},
		{
			// OUR OWN wait, which the caller answers locally before asking this.
			// It must not be mistaken for the peer's, or a client with no
			// certificate would blame the server.
			name: "our own pending identity is not the peer's",
			err:  ErrNoSVIDYet,
		},
		{
			// The failure the ERROR path exists for (#700) must stay an ERROR.
			name: "a registrar that will not serve is still a failure",
			err:  status.Error(codes.Unavailable, "connection error: desc = \"transport: Error while dialing: dial tcp: connect: connection refused\""),
		},
		{
			name: "a drain GOAWAY is not an identity failure",
			err:  status.Error(codes.Unavailable, "closing transport due to: EOF, received prior goaway: code: NO_ERROR"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsPeerIdentityUnavailable(tc.err))
		})
	}
}
