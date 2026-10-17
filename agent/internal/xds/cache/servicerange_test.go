package cache

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
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
