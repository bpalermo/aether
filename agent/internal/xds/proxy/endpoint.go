package proxy

import (
	"slices"
	"sort"

	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
)

const (
	// envoyFilterMetadataSubsetNamespace is the Envoy metadata namespace for load balancing subsets
	envoyFilterMetadataSubsetNamespace = "envoy.lb"

	// subsetClusterKey is the metadata key for the cluster name
	subsetClusterKey = "cluster"
	// subsetIPKey is the metadata key for the endpoint IP address
	subsetIPKey = "ip"
	// subsetPodNamespaceKey is the metadata key for the pod namespace
	subsetPodNamespaceKey = "namespace"
	// subsetPodNameKey is the metadata key for the pod name
	subsetPodNameKey = "pod"
	// subsetWaypointKey marks an endpoint reached via the per-node east/west
	// waypoint (proposal 019). It lives in the envoy.lb metadata namespace so the
	// cluster's transport-socket matcher (EndpointMetadataInput) can branch on it
	// to present the structured waypoint SNI; it is never a subset selector key.
	subsetWaypointKey = "waypoint"
	// subsetWaypointValue is the value stamped under subsetWaypointKey.
	subsetWaypointValue = "true"
)

// NewClusterLoadAssignment creates an empty cluster load assignment for a service.
// Endpoints should be added to the Endpoints field.
func NewClusterLoadAssignment(serviceName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

// LoadAssignmentAlias returns base's load assignment re-published under name:
// the EDS resource of a cluster that has the SAME membership as base but must
// not subscribe to base's resource name. Every such cluster points its
// eds_cluster_config.service_name at its own cluster name and the cache
// publishes this copy in the same snapshot pass that emits the cluster:
//
//   - the QUIC twins (QUICClusterFrom, aether#1008);
//   - the HTTP port-alias clusters "<fqdn>:<port>" (aether#1013);
//   - the TCP floor "tcp:<fqdn>" and its primary-port alias
//     "tcp:<fqdn>:<port>" (aether#1013).
//
// Why a copy instead of a shared name: Envoy's delta-ADS WatchMap deduplicates
// subscription interest per (type_url, resource name). A cluster added in a
// LATER CDS update than a sibling already subscribed to the same EDS name adds
// nothing to resource_names_subscribe, no request goes out, the control plane
// has nothing to answer (the resource did not change), and the new cluster
// warms for the full initial_fetch_timeout (15 s). The same mechanism as the
// SDS outage of #842. A delta-ADS subscriber must never share a resource name
// with an already-subscribed sibling.
//
// Endpoints, named endpoints and policy are the base's -- including the health
// status the agent writes itself and every locality/metadata field -- so the
// alias sees exactly the membership the base cluster does.
//
// The endpoint slice elements and the policy are SHARED with base, not cloned:
// load assignments are never mutated after they are built (a changed endpoint
// set builds a new one, see SnapshotCache.RemoveEndpoint), and the copy is
// rebuilt from the base on every snapshot, so identical inputs marshal to
// identical bytes. Every ClusterLoadAssignment field is carried; the proxy
// tests pin the field set so a new upstream field cannot be dropped silently.
func LoadAssignmentAlias(base *endpointv3.ClusterLoadAssignment, name string) *endpointv3.ClusterLoadAssignment {
	if base == nil {
		return nil
	}
	return &endpointv3.ClusterLoadAssignment{
		ClusterName:    name,
		Endpoints:      slices.Clone(base.GetEndpoints()),
		NamedEndpoints: base.GetNamedEndpoints(),
		Policy:         base.GetPolicy(),
	}
}

// SortLocalityLbEndpoints orders a load assignment's endpoints by their first
// endpoint's address. Endpoint order is part of the EDS resource's bytes, which
// the delta-xDS cache hashes to decide whether the resource changed — callers
// that rebuild assignments from maps (or from registry listings with unstable
// order) must sort so an unchanged endpoint set never hashes as changed.
func SortLocalityLbEndpoints(endpoints []*endpointv3.LocalityLbEndpoints) {
	sort.Slice(endpoints, func(i, j int) bool {
		return localityLbEndpointsKey(endpoints[i]) < localityLbEndpointsKey(endpoints[j])
	})
}

// localityLbEndpointsKey returns a stable ordering key for a LocalityLbEndpoints
// (the generators here emit one LbEndpoint per entry, keyed by its address).
func localityLbEndpointsKey(lle *endpointv3.LocalityLbEndpoints) string {
	if len(lle.GetLbEndpoints()) == 0 {
		return ""
	}
	addr := lle.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	return addr.GetAddress()
}
