package cache

import (
	"context"
	"net/netip"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func serviceRangesGen(c *SnapshotCache) uint64 {
	c.captureMu.RLock()
	defer c.captureMu.RUnlock()
	return c.serviceRangesGen
}

func TestSetServiceRanges_Canonical(t *testing.T) {
	c := newTestCache("node-1")
	assert.Nil(t, c.ServiceRanges(), "no range is known before one is set")

	c.SetServiceRanges([]netip.Prefix{
		netip.MustParsePrefix("10.96.0.1/12"),
		netip.MustParsePrefix("fd00::/108"),
		netip.MustParsePrefix("10.0.0.0/24"),
	})
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("10.96.0.0/12"),
	}, c.ServiceRanges())
}

// The same range, in any order or spelling, is no change: a ServiceCIDR resync
// must not count as one (and, from P0-3 on, must not rebuild every pod listener).
func TestSetServiceRanges_Idempotent(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix("10.0.0.0/24")})
	gen := serviceRangesGen(c)
	assert.Equal(t, uint64(1), gen)

	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("10.96.0.5/12")})
	assert.Equal(t, gen, serviceRangesGen(c), "an equal range is not a change")

	c.SetServiceRanges(nil)
	assert.Equal(t, gen+1, serviceRangesGen(c))
	assert.Nil(t, c.ServiceRanges())
	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("fd00::/108")})
	assert.Equal(t, gen+1, serviceRangesGen(c), "an IPv6-only range is still no range")
}

// The getter hands out a copy.
func TestServiceRanges_Copy(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")})
	got := c.ServiceRanges()
	got[0] = netip.MustParsePrefix("192.0.2.0/24")
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")}, c.ServiceRanges())
}

// podCapture returns the capture listener the cache holds for the one pod of
// an a043Cache, as built (before any snapshot).
func podCapture(t *testing.T, c *SnapshotCache) *listenerv3.Listener {
	t.Helper()
	c.listenerMu.RLock()
	defer c.listenerMu.RUnlock()
	require.Len(t, c.listeners, 1)
	for _, e := range c.listeners {
		l, ok := e.capture.(*listenerv3.Listener)
		require.True(t, ok)
		return l
	}
	return nil
}

func drainDepChanged(c *SnapshotCache) {
	for len(c.depChanged) > 0 {
		<-c.depChanged
	}
}

// TestSetServiceRanges_RebuildsTheCaptureListener (proposal 044, Phase 0):
// a new range rebuilds every pod's capture listener with the Service-range
// branch and asks for a snapshot; the same range again rebuilds nothing; no
// range takes the branch away.
func TestSetServiceRanges_RebuildsTheCaptureListener(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, a043Fixture())
	require.NoError(t, c.generateSnapshot(ctx))
	drainDepChanged(c)

	const (
		httpOnly = "10.96.1.1" // httponly: 8080 HTTP, no capture service
		tcpOnly  = "10.96.1.7" // tcponly: 9000 raw TCP
		other    = "10.96.200.1"
		setIP    = "100.64.5.5"
	)
	tcpOwn := "cap_tcp_tcp:tcponly.demo." + a043Mesh + "_9000"
	before := a043CaptureListener(t, c)
	for _, conn := range []shpConn{shpRaw(other, 5432), shpRaw(setIP, 5432), shpRaw(httpOnly, 8080)} {
		require.Equal(t, "cap_passthrough", shpSelectChain(before, conn), "%+v before any range", conn)
	}

	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix(setIP + "/32")})
	assert.Positive(t, len(c.depChanged), "a new range asks for a snapshot")
	built := podCapture(t, c)
	require.NoError(t, c.generateSnapshot(ctx))
	l := a043CaptureListener(t, c)
	svc := proxy.CapturePassthroughServiceChainName
	for _, tc := range []struct {
		conn shpConn
		want string
	}{
		{shpHTTP(httpOnly, 8080), proxy.CaptureHTTPChainName},
		{shpRaw(httpOnly, 8080), svc},
		{shpRaw(tcpOnly, 9000), tcpOwn},
		{shpRaw(tcpOnly, 5555), svc},
		{shpRaw(other, 5432), svc},
		{shpRaw(setIP, 5432), svc},
		{shpRaw("192.0.2.1", 443), "cap_passthrough"},
	} {
		assert.Equal(t, tc.want, shpSelectChain(l, tc.conn), "%+v", tc.conn)
	}

	// The same range, spelled otherwise: nothing is rebuilt.
	drainDepChanged(c)
	c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix(setIP + "/32"), netip.MustParsePrefix("10.100.0.0/12")})
	assert.Zero(t, len(c.depChanged), "an equal range asks for nothing")
	assert.Same(t, built, podCapture(t, c), "an equal range must not rebuild the pod listeners")

	// No range: the listener is the one before the branch.
	c.SetServiceRanges(nil)
	assert.Positive(t, len(c.depChanged))
	require.NoError(t, c.generateSnapshot(ctx))
	assert.True(t, proto.Equal(before, a043CaptureListener(t, c)), "no range: the capture listener is the one built before any range")
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestCaptureInputsReady_WaitsForBoth: the capture gate opens once the
// mesh-Service projection and the Service range have both been delivered, in
// either order, a nil range and an empty projection included.
func TestCaptureInputsReady_WaitsForBoth(t *testing.T) {
	for _, rangeFirst := range []bool{true, false} {
		c := newTestCache("node-1")
		assert.False(t, isClosed(c.CaptureInputsReady()))
		if rangeFirst {
			c.SetServiceRanges(nil)
			assert.True(t, isClosed(c.ServiceRangeDelivered()), "a nil range is a delivery")
			assert.False(t, isClosed(c.CaptureInputsReady()), "the range alone does not open the gate")
			c.SetCaptureTCPServices(nil)
		} else {
			c.SetCaptureTCPServices(nil)
			assert.False(t, isClosed(c.CaptureInputsReady()), "the projection alone does not open the gate")
			c.SetServiceRanges(nil)
		}
		assert.True(t, isClosed(c.CaptureInputsReady()), "rangeFirst=%v", rangeFirst)
		// Later calls are harmless.
		c.SetServiceRanges([]netip.Prefix{netip.MustParsePrefix("10.96.0.0/12")})
		c.SetCaptureTCPServices(nil)
	}
}
