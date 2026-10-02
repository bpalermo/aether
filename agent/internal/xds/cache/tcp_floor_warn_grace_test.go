package cache

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const tcpFloorWarning = "TCP mesh services are configured but have no capture chains"

// TestTCPFloorWarningSparesTheStartupIdentityWait is issue #1123's guard on
// its own startup change: the node agent now builds its local pods' listeners
// from storage while it is still waiting for its SVID, so with SPIRE on every
// restart builds capture listeners without identity for a moment. That is the
// designed startup state (the identity gate holds the socket, the spire-svid
// check owns a missing SVID), and the #877 WARN must not fire on every
// restart for it. It still fires past the grace, and immediately with SPIRE
// off, where it is the steady-state outage it was written for.
func TestTCPFloorWarningSparesTheStartupIdentityWait(t *testing.T) {
	newCache := func(spire bool) (*SnapshotCache, *bytes.Buffer) {
		var buf bytes.Buffer
		c := NewSnapshotCache("node-1", slog.New(slog.NewTextHandler(&buf, nil)))
		c.SetSpireEnabled(spire)
		return c, &buf
	}

	t.Run("SPIRE on, inside the startup grace: silent", func(t *testing.T) {
		c, buf := newCache(true)
		c.warnTCPFloorWithoutIdentity(3)
		assert.NotContains(t, buf.String(), tcpFloorWarning)
	})

	t.Run("SPIRE on, past the grace: warns", func(t *testing.T) {
		c, buf := newCache(true)
		c.createdAt = time.Now().Add(-2 * tcpFloorStartupGrace)
		c.warnTCPFloorWithoutIdentity(3)
		assert.Equal(t, 1, strings.Count(buf.String(), tcpFloorWarning))
	})

	t.Run("SPIRE off: warns at once", func(t *testing.T) {
		c, buf := newCache(false)
		c.warnTCPFloorWithoutIdentity(3)
		assert.Equal(t, 1, strings.Count(buf.String(), tcpFloorWarning))
	})
}
