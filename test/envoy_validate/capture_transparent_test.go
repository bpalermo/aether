package envoy_validate

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCaptureTCPListenerIsTransparentAndKeepsOriginalDst pins the pair of
// listener properties TPROXY capture rests on (proposal 038), on the fixture
// that stock Envoy validates:
//
//   - transparent: the CNI's prerouting `tproxy to :18001` assigns a diverted
//     packet only to a socket with IP_TRANSPARENT; without it the packet finds
//     no socket and the client gets a reset. Envoy applies the option per worker
//     socket at PREBIND inside the pod netns.
//   - original_dst STILL present, first in the listener filters: it is what sets
//     localAddressRestored(), which the ORIGINAL_DST passthrough cluster requires.
//     On a diverted flow SO_ORIGINAL_DST succeeds (tracked, not NATed) and returns
//     the same VIP:port getsockname would; the IP_TRANSPARENT fallback in
//     utility.cc is a backstop. Dropping the filter would break the passthrough
//     silently -- the ORIGINAL_DST cluster would simply have no host.
//
// These are asserted together because they are a pair, and a future "simplify:
// transparent makes original_dst redundant" would be wrong for the second
// reason above.
func TestCaptureTCPListenerIsTransparentAndKeepsOriginalDst(t *testing.T) {
	bs, err := buildCaptureBootstrap()
	require.NoError(t, err)

	var found int
	for _, l := range bs.GetStaticResources().GetListeners() {
		if l.GetName() != proxy.CaptureListenerName(testPod()) {
			continue
		}
		found++
		assert.True(t, l.GetTransparent().GetValue(),
			"capture listener must set transparent: tproxy will not assign to a non-transparent socket")
		assert.True(t, l.GetUseOriginalDst().GetValue(), "use_original_dst must stay set")
		require.NotEmpty(t, l.GetListenerFilters(), "listener filters must be present")
		assert.Equal(t, "envoy.filters.listener.original_dst", l.GetListenerFilters()[0].GetName(),
			"original_dst must remain the FIRST listener filter: the passthrough cluster depends on localAddressRestored()")
		// The socket is still a plain TCP bind in the pod netns on the capture port.
		sa := l.GetAddress().GetSocketAddress()
		assert.NotEmpty(t, sa.GetNetworkNamespaceFilepath(), "bound into the pod netns")
		assert.Equal(t, "0.0.0.0", sa.GetAddress())
	}
	require.Equal(t, 1, found, "exactly one capture listener in the fixture (anti-vacuity)")
}
