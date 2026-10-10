package proxy

import (
	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

// podResourceKeySeparator joins the namespace and the pod name in
// PodResourceKey. An underscore is legal in neither (a namespace is an RFC 1123
// label, a pod name an RFC 1123 subdomain: lower-case alphanumerics, '-', and
// for the pod name '.'), so the first underscore of a key always ends the
// namespace and two different pods never produce the same key.
const podResourceKeySeparator = "_"

// PodResourceKey returns "<namespace>_<pod>": the part of every per-pod xDS
// resource name and stat prefix that says WHICH pod it belongs to.
//
// It is the only place that decides how a pod is spelled in a name. Every
// per-pod listener (inbound_, inbound_…_h3, outbound_http_, capture_,
// capture_udp_), every per-pod cluster (app_…_<port>, health_, inboundready_),
// every per-pod stat prefix (out_http_, in_tcp_, and the listeners' own) and
// every per-pod filter chain name (in_, in_tcp_, in_h3_, out_http_, capture_)
// is built from it. //agent/internal/xds/proxy:proxy_test
// (TestPerPodResourceNamesCarryTheNamespace) is the inventory.
//
// The pod NAME alone is not a key (issue #1584). Two pods of one name in two
// namespaces on one node (two StatefulSets called "web", each with a "web-0")
// then shared every one of those names. go-control-plane keeps one resource
// per name, and which pod's it kept was decided by map order on every snapshot
// build, separately for listeners and for clusters: one pod lost its
// listeners, and the other's inbound listener could route to an app cluster
// that dials the first pod's loopback.
//
// The chart extracts the aether.namespace and aether.pod stats tags from the
// listener stat prefixes built here (charts/aether/templates/
// agent-proxy-configmap.yaml), and its stats exclusions match the health_ and
// inboundready_ cluster names; a change of this shape has to be made there too.
func PodResourceKey(cniPod *cniv1.CNIPod) string {
	return cniPod.GetNamespace() + podResourceKeySeparator + cniPod.GetName()
}
