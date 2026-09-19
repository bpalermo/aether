package spire

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUnreachableBrokerEscalatesFromWarnToError is #766's second site. A brief
// Unavailable is designed — a restarted agent resubscribes before its own SVID
// has landed, and a SPIRE agent restart takes the socket away for half a minute —
// and used to cost one ERROR per managed pod per attempt. It is WARN until the
// subscription has been failing for brokerUnreachableErrorAfter, and ERROR after.
func TestUnreachableBrokerEscalatesFromWarnToError(t *testing.T) {
	var out bytes.Buffer
	b := newOrderingTestBridge(&recordingStore{})
	b.log = slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	unavailable := status.Error(codes.Unavailable, "transport: authentication handshake failed: no SPIRE SVID yet")

	var failingSince time.Time
	retry := b.reportSubscribeError(t.Context(), unavailable, testWorkload, testPodRef, &failingSince)
	assert.True(t, retry.retry)
	assert.False(t, failingSince.IsZero(), "the first failure starts the clock")
	assert.Contains(t, out.String(), `"level":"WARN"`)
	assert.NotContains(t, out.String(), `"level":"ERROR"`)

	out.Reset()
	failingSince = time.Now().Add(-brokerUnreachableErrorAfter - time.Second)
	b.reportSubscribeError(t.Context(), unavailable, testWorkload, testPodRef, &failingSince)
	assert.Contains(t, out.String(), `"level":"ERROR"`, "an endpoint that stays unreachable past the dwell is a fault")
}
