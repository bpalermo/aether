// Package constants defines agent-specific constants for socket paths and directory defaults.
package constants

import "aethermesh.dev/common/constants"

const (
	// DefaultHostCNIRegistryDir is the default directory for storing CNI registry data on the host
	DefaultHostCNIRegistryDir = "/host" + constants.CNIDefaultRegistryPath

	// DefaultHostCNINetDir is the host's CNI network-config directory as mounted
	// into the agent container. The agent watches the active conflist there and
	// re-asserts aether's chained plugin entry whenever a competing writer strips
	// it (#645). Same mount the cni-install init container uses, but read-write.
	DefaultHostCNINetDir = "/host" + constants.CNIDefaultNetDir

	// DefaultEdgeRegistryDir is the edge's pod-local (always-empty) registry dir.
	// The edge has no host CNI mount, so runEdge points its empty local store
	// here (the node hostPath doesn't exist in the edge pod).
	DefaultEdgeRegistryDir = constants.CNIDefaultRegistryPath

	// DefaultMeshDNSSnapshotPath is the default host-persistent file the in-process
	// mesh-DNS resolver persists its last-known record table to (and warm-loads at
	// boot). It lives in a dedicated SUBDIRECTORY under the CNI registry hostPath so
	// it survives a rolling agent restart (a new pod) yet stays out of the CNI pod
	// store's top-level *.json scan (setupStorage/loadAll unmarshals every top-level
	// .json there as a CNIPod — a subdir is skipped), closing the mesh_dns cold
	// window (Fix 1).
	DefaultMeshDNSSnapshotPath = DefaultHostCNIRegistryDir + "/mesh-dns/records.json"

	// DefaultXdsSocketPath is the default Unix domain socket path for the xDS server
	DefaultXdsSocketPath = "/run/aether/xds.sock"
	// DefaultCNISocketPath is the default Unix domain socket path for the CNI server
	DefaultCNISocketPath = "/run/aether/cni.sock"
	// DefaultProxyHealthSocketPath is the Unix domain socket where the proxy's
	// agent-programmed health gateway listener exposes per-pod app health
	// (health_check filters over the health_<pod> clusters, served on worker
	// threads). The liveness loop probes it instead of the admin interface.
	// /run/aether is shared between the agent and proxy containers.
	DefaultProxyHealthSocketPath = "/run/aether/health.sock"

	// DefaultAgentLockPath is the node-ownership lock (proposal 041). The agent
	// that owns this node holds an exclusive flock(2) on it for its whole life;
	// a surge-rolled successor starts as a standby blocked on it and takes the
	// node over the instant the kernel releases it (the owner's exit, however it
	// dies). On /run/aether, the hostPath both agent pods of a surge mount.
	DefaultAgentLockPath = "/run/aether/agent.lock"

	// DefaultAgentHealthSocketPath is where the agent serves /healthz and
	// /readyz for its exec probes (agent/cmd/agent-ready) when the chart runs
	// it with --health-socket (proposal 041). In the pod's OWN /tmp emptyDir,
	// never a host path: during a surge roll two agent pods share the node, and
	// a kubelet probe answered by the other pod's agent would be meaningless.
	DefaultAgentHealthSocketPath = "/tmp/aether-agent-health.sock"

	// DefaultSpireBrokerSocketPath is the default path to the SPIRE agent's
	// SPIFFE Broker Endpoint socket, as the aether chart mounts it (proposal 036).
	// The SPIRE chart puts it on the node at
	// /run/spire/agent/sockets/csi.spiffe.io/broker/broker.sock.
	DefaultSpireBrokerSocketPath = "/run/spire/broker-sockets/broker.sock"
	// DefaultSpireWorkloadSocketPath is the default SPIRE Workload API UDS socket (csi.spiffe.io mount)
	DefaultSpireWorkloadSocketPath = "/run/secrets/workload-spiffe-uds/socket"
)
