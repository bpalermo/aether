package cache

import (
	"sync"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
)

type SnapshotCache struct {
	cachev3.SnapshotCache

	log logr.Logger

	nodeName string

	listenerMu sync.RWMutex
	listeners  map[string]listenerEntry // keyed by container network namespace

	clusterMu sync.RWMutex
	clusters  map[string]clusterEntry // keyed by cluster name

	version *atomic.Uint64
}

// listenerEntry holds the inbound and outbound listeners for a single pod.
type listenerEntry struct {
	inbound  types.Resource
	outbound types.Resource
}

// clusterEntry holds the cluster and its associated load assignment.
type clusterEntry struct {
	cluster   *clusterv3.Cluster
	endpoints map[string]*endpointv3.LocalityLbEndpoints // keyed of IP
	vhost     *routev3.VirtualHost
}

func NewSnapshotCache(nodeName string, log logr.Logger) *SnapshotCache {
	return &SnapshotCache{
		SnapshotCache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		log:           log.WithName("cache"),
		nodeName:      nodeName,
		version:       atomic.NewUint64(0),
	}
}
