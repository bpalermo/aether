package proxy

import (
	"strings"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
)

// podResourceKeySeparator joins the namespace and the pod name in
// PodResourceKey. An underscore is legal in neither (a namespace is an RFC 1123
// label, a pod name an RFC 1123 subdomain: lower-case alphanumerics, '-', and
// for the pod name '.'), so the first underscore of a key always ends the
// namespace and two different pods never produce the same key.
const podResourceKeySeparator = "_"

// PodResourceKey returns "<namespace>_<pod>": the part of every per-pod xDS
// resource name that says WHICH pod it belongs to.
//
// It is the only place that decides how a pod is spelled in a name. Every
// per-pod listener (inbound_, inbound_…_h3, outbound_http_, capture_,
// capture_udp_), every per-pod cluster (app_…_<port>, health_, inboundready_),
// and every per-pod filter chain name (in_, in_tcp_, in_h3_, out_http_, capture_)
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
// STAT names are not built from it directly but from PodStatKey, which differs
// for a pod name that contains a dot. The chart extracts the aether.namespace
// and aether.pod stats tags from the listener stat prefixes
// (charts/aether/templates/agent-proxy-configmap.yaml), and its stats
// exclusions match the health_ and inboundready_ clusters' stat names; a change
// of either shape has to be made there too.
func PodResourceKey(cniPod *cniv1.CNIPod) string {
	return cniPod.GetNamespace() + podResourceKeySeparator + cniPod.GetName()
}

// podStatDot stands for a '.' of the pod name in a STAT name (issue #1636).
//
// A pod name is an RFC 1123 subdomain, so it may contain dots, and a dot is
// the separator of an Envoy stat name: "cluster.health_team-a_web.external-0.
// membership_healthy" reads, to everything that parses stat names, as a
// cluster "health_team-a_web" with a stat "external-0.membership_healthy". The
// chart's stats exclusions and tag extractors (agent-proxy-configmap.yaml) are
// such parsers: the exclusion of the probe clusters' dead "external.*" subtree
// matched that name, the gauge was never allocated, and the health gateway's
// health_check filter, which reads it, answered 503 for a healthy pod.
//
// '~' is legal in neither a namespace nor a pod name, so the replacement can be
// read back ('~' -> '.') and two different pods never share a stat name. It is
// not '_', which ends the namespace in the key, and not ':', which Envoy itself
// rewrites to '_' in a stat name. Measured on the pinned proxy: '~' is kept as
// it is in the stat name, in a tag value and in a Prometheus label value
// (//agent/test/envoy_validate, TestChartStatsConfigOnDottedPodNames).
const podStatDot = "~"

// PodStatKey returns how a pod is spelled in a STAT name: PodResourceKey with
// each dot of the pod name replaced by '~' ("team-a_web~external-0"). For a
// pod name without a dot, which is every name a workload controller generates,
// it IS PodResourceKey.
//
// Only stat names use it: the listeners' stat_prefix, the tcp_proxy and
// udp_proxy stat prefixes, and the probe clusters' alt_stat_name
// (podStatName). No resource is named by it. Envoy and go-control-plane key
// listeners and clusters by PodResourceKey, the health gateway's health_check
// filters and paths name the probe clusters by it, and the agent's liveness
// loop requests those paths.
//
// The value of the chart's aether.pod stats tag is therefore the pod name with
// '~' for each dot (docs/runbook.md, "Chart 2.5.2").
func PodStatKey(cniPod *cniv1.CNIPod) string {
	return podStatName(PodResourceKey(cniPod))
}

// podStatName turns a per-pod resource name ("inbound_<namespace>_<pod>",
// "health_<namespace>_<pod>") into the name its stats are keyed by. Only the
// pod name can contribute a dot: the prefixes are constants and a namespace is
// an RFC 1123 label.
func podStatName(resourceName string) string {
	return strings.ReplaceAll(resourceName, ".", podStatDot)
}

// podClusterAltStatName is the alt_stat_name of a per-pod probe cluster: empty
// when the cluster's name already is its stat name, so that a pod without a dot
// in its name keeps the cluster it has (an alt_stat_name equal to the name
// would still be a change of the cluster, on every node, at the upgrade).
func podClusterAltStatName(clusterName string) string {
	if stat := podStatName(clusterName); stat != clusterName {
		return stat
	}
	return ""
}
