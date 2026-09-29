package cache

import (
	"maps"

	"aethermesh.dev/agent/internal/xds/proxy"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// CreateDeltaWatch answers an on-demand subscription for a `quic:` twin the
// stream already holds (issue #1049).
//
// Envoy's ODCDS manager opens one singleton subscription per cluster name, on
// the same delta ADS stream as the wildcard CDS subscription, and a request
// that routes to a twin opens it whenever the twin is not an ACTIVE cluster --
// including when the wildcard has already delivered it and it is still
// warming. go-control-plane answers a named subscribe only when the resource's
// version differs from what the stream was last sent, so a twin the wildcard
// delivered earlier gets no response at all. The per-name subscription then
// waits out its initial-fetch timeout (15 s) and Envoy reports the twin
// missing: `cm odcds: cluster quic:... not found during on-demand discovery`
// on main-worker-03 at 15:45:04.7, 15.0 s after the k6 loader's first request
// at 15:44:49.7, while the agent served that twin continuously from 15:44:46.1.
// A request still waiting on the name at that moment is failed as missing.
//
// So every `quic:` name a request newly subscribes to is treated as unsent: its
// returned version is blanked for the response computation, exactly as
// go-control-plane itself does for a name explicitly unsubscribed from a
// wildcard stream. The twin, when the snapshot carries it, goes out in the
// response to the subscribe itself, which is the answer the subscription is
// waiting for. When the snapshot does not carry it nothing changes: a name
// the stream was never sent stays unanswered (held until the twin is
// published), and one it was sent is already in removed_resources from the
// snapshot that dropped it. This never adds an absent answer.
//
// The stream's own subscription state is not touched: the wrapper gives the
// cache a private copy of the returned-version map (the Subscription contract
// forbids altering the original), and the server records the versions the
// response actually carried as usual.
func (c *SnapshotCache) CreateDeltaWatch(req *cachev3.DeltaRequest, sub cachev3.Subscription, value chan cachev3.DeltaResponse) (func(), error) {
	if req.GetTypeUrl() == resourcev3.ClusterType {
		sub = resendSubscribedTwins(req.GetResourceNamesSubscribe(), sub)
	}
	return c.SnapshotCache.CreateDeltaWatch(req, sub, value)
}

// resendSubscribedTwins returns sub, or -- when any of the newly subscribed
// names is a `quic:` twin the stream was already sent -- a view of it whose
// returned versions have those names blanked, so the response re-sends them.
func resendSubscribedTwins(subscribed []string, sub cachev3.Subscription) cachev3.Subscription {
	returned := sub.ReturnedResources()
	var resend []string
	for _, name := range subscribed {
		if !proxy.IsQUICClusterName(name) {
			continue
		}
		if _, sent := returned[name]; sent {
			resend = append(resend, name)
		}
	}
	if len(resend) == 0 {
		return sub
	}
	blanked := maps.Clone(returned)
	for _, name := range resend {
		blanked[name] = ""
	}
	return resentSubscription{Subscription: sub, returned: blanked}
}

// resentSubscription is a Subscription whose returned-version map is replaced.
type resentSubscription struct {
	cachev3.Subscription
	returned map[string]string
}

func (s resentSubscription) ReturnedResources() map[string]string { return s.returned }
