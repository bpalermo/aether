// Package config provides CNI plugin configuration parsing and data structures.
//
// The CNI plugin configuration is provided as JSON via stdin during plugin invocation.
// It includes the standard CNI PluginConf fields plus Aether-specific fields for
// agent communication and container runtime integration.
package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	agentConstans "aethermesh.dev/agent/constants"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
)

// AetherConf represents the CNI plugin configuration for Aether.
//
// It extends the standard CNI PluginConf with Aether-specific fields:
//   - AgentCNIPath: Path to the Unix domain socket where the Aether agent
//     listens for pod add/remove requests. If omitted, defaults to the
//     default socket path defined in constants.
//   - CRISocket: Path to the container runtime interface (CRI) socket for
//     retrieving container process information via CRI APIs when PID cannot
//     be determined from the network namespace path.
//   - RuntimeConfig: Optional runtime configuration passed by the container runtime,
//     including pod annotations.
type AetherConf struct {
	types.PluginConf
	// AgentCNIPath is the path to the Aether agent's CNI gRPC socket
	AgentCNIPath string `json:"agent_cni_path"`
	// CRISocket is the path to the container runtime interface socket
	CRISocket string `json:"cri_socket"`

	// NetnsPinDisabled turns off netns pinning (on by default): Envoy dials
	// local pods inside their netns by filepath, so a dial racing the runtime's
	// netns removal opens a path that is already gone. CNI ADD bind-mounts the
	// netns to an aether-owned path that outlives the runtime's teardown, so
	// those late dials still work.
	//
	// It used to be a crash (Envoy 1.38 dereferenced a nullptr connection;
	// e2e findings 2026-06-10, #245). On the pinned proxy snapshot it is a
	// clean per-request failure — envoyproxy/envoy#45975 for the pool dial and
	// #46503 for the active health checkers — so the pin is now a
	// data-plane-quality measure, not a crash guard, and nothing waits on it.
	NetnsPinDisabled bool `json:"netns_pin_disabled"`
	// NetnsPinDir is where CNI ADD bind-mounts each pod's netns. Must be a
	// host path visible to the aether-proxy container (which mounts /run/aether
	// with HostToContainer propagation). Empty = /run/aether/netns.
	NetnsPinDir string `json:"netns_pin_dir"`
	// NetnsUnpinDelaySeconds is how long the detached unpinner waits before
	// releasing the netns, covering Envoy's deferred cluster destruction, its
	// connection-pool drains and a hot-restart successor re-creating the pod's
	// listeners — all of which can still touch the netns for a few seconds
	// after the pod is gone. The delay runs out of process, so it never slows
	// pod teardown, and CNI DEL schedules it whether or not the agent ACKed
	// the removal (#796).
	// 0 = 10s default; negative = no delay.
	NetnsUnpinDelaySeconds int `json:"netns_unpin_delay_seconds"`
	// NetnsDelGiveUpAfterSeconds bounds how long CNI DEL keeps failing back to
	// the runtime when a *reachable* agent answers the removal with an error.
	// containerd retries a failed DEL indefinitely, and a pod whose sandbox
	// cannot be torn down keeps its CPU request — that is how one wedged DEL
	// starved a node of its replacement agent/proxy/mesh-dns pods for 12m47s
	// (#796). Past this bound the plugin degrades to the agent-unreachable
	// path: it unpins on the normal delay, returns success, and leaves the
	// reconciliation to the agent's ghost sweep. The first failure's timestamp
	// is kept in a "<pin>.delfail" marker next to the netns pin, because the
	// plugin process lives for exactly one CNI call.
	// 0 = 5m default; negative = give up on the first failure.
	NetnsDelGiveUpAfterSeconds int `json:"netns_del_give_up_after_seconds"`

	// ReadinessProbeDisabled turns off the in-netns data-plane readiness probe
	// (on by default): after the agent confirms a pod's xDS config, CNI ADD
	// probes the pod's outbound capture listener from inside its netns until
	// the proxy's health_check filter answers 200, proving the data plane is
	// actually serving (socket bound in the netns, workers accepting) before
	// pod start completes. CNI DEL probes until the listener socket is gone.
	// Best-effort: a probe timeout is logged, never fails the CNI operation.
	ReadinessProbeDisabled bool `json:"readiness_probe_disabled"`

	// CaptureRedirectAllDefault makes redirect-all the DEFAULT for managed pods
	// (proposal 022, M2-default Step 4 — the "flip"). When true, every non-ignored
	// pod on the node gets the broad redirect-all capture (ALL outbound non-local
	// TCP into the capture listener :18001; Envoy's ORIGINAL_DST recovers the real
	// destination and non-mesh egress passes through in plain TCP) UNLESS it
	// carries the capture.aether.io/redirect-all="false" opt-out annotation. This
	// is the Istio-style "capture what the app sends" posture, with zero per-pod
	// config. When false, redirect-all is per-pod opt-in via the same annotation.
	//
	// The scoped mesh-ClusterIP:18081 capture redirect (proposal 018, Phase 3a)
	// is UNCONDITIONAL (proposal 031) — the Envoy side always carries the capture
	// listener and the passthrough fallback chain, so this is the single
	// remaining node-wide capture knob.
	//
	// Redirect-all exclusions installed to prevent loops and proxy self-traffic:
	//   - loopback (127.0.0.0/8) skipped — the :18081 fast-lane is untouched
	//   - the capture port itself (:18001 TCP) skipped — prevents re-entry
	//   - established/related connections skipped via conntrack (RELATED,ESTABLISHED)
	//   - DNS (:53 UDP+TCP) NOT excluded — passes through Envoy if MeshDNSEnabled=false,
	//     or remains DNAT'd to the mesh-DNS resolver if MeshDNSEnabled=true
	CaptureRedirectAllDefault bool `json:"capture_redirect_all_default"`

	// MeshDNSEnabled installs, inside each pod's netns, an nft DNAT of outbound DNS
	// (UDP+TCP :53, non-loopback) -> the node agent's resolver at HostIP:18054
	// (proposal 018, mesh-global FQDN). Off by default; pairs with the agent's
	// --mesh-dns.
	MeshDNSEnabled bool `json:"mesh_dns_enabled"`
	// HostIP is the node IP the mesh-DNS DNAT targets (the agent's host-local
	// resolver). Written by cni-install from the downward-API HOST_IP.
	HostIP string `json:"host_ip,omitempty"`

	// OTLPEndpoint enables OTel telemetry (traces + metrics) pushed to the
	// given OTLP gRPC collector (host:port, insecure). The plugin binary is
	// exec'd by the container runtime, so its environment is the runtime's,
	// not a pod's — the endpoint travels in the netconf (written by
	// cni-install) instead. Empty = the standard OTEL_EXPORTER_OTLP_* env
	// vars, if the runtime happens to set them; otherwise telemetry is off.
	OTLPEndpoint string `json:"otlp_endpoint,omitempty"`

	// RuntimeConfig holds runtime-provided configuration like pod annotations
	RuntimeConfig *RuntimeConfig `json:"runtimeConfig,omitempty"`
}

// defaultNetnsPinDir lives under /run/aether, which the aether-proxy DaemonSet
// already mounts with HostToContainer propagation, so pinned netns mounts made
// by the (host-side) plugin become visible to Envoy without chart changes.
const defaultNetnsPinDir = "/run/aether/netns"

// defaultNetnsUnpinDelay covers the post-removal dial window observed on
// talos-main: health checkers / connection pools dialed up to ~13s after the
// listener and clusters were removed from the snapshot under roll churn. Such a
// dial through an already-released pin used to segfault Envoy 1.38; on the
// pinned snapshot it is a clean failure (envoyproxy/envoy#45975, #46503), so
// the delay now buys those dials a working netns rather than the node's life.
// The unpin runs as a detached process, so a generous delay does not slow pod
// teardown.
const defaultNetnsUnpinDelay = 60 * time.Second

// defaultNetnsDelGiveUpAfter bounds the DEL retry loop against a live-but-erroring
// agent. Five minutes is long enough for an agent rolling on the node to come back
// and ACK the removal normally, and short enough that a pod stuck Terminating
// cannot hold its CPU request past the point where the node's own DaemonSet pods
// fail to schedule (#796).
const defaultNetnsDelGiveUpAfter = 5 * time.Minute

// NetnsPinPath returns the pin target for a container (sandbox) ID.
func (c AetherConf) NetnsPinPath(containerID string) string {
	dir := c.NetnsPinDir
	if dir == "" {
		dir = defaultNetnsPinDir
	}
	return filepath.Join(dir, containerID)
}

// NetnsUnpinDelay returns the effective unpin delay.
func (c AetherConf) NetnsUnpinDelay() time.Duration {
	switch {
	case c.NetnsUnpinDelaySeconds == 0:
		return defaultNetnsUnpinDelay
	case c.NetnsUnpinDelaySeconds < 0:
		return 0
	default:
		return time.Duration(c.NetnsUnpinDelaySeconds) * time.Second
	}
}

// NetnsDelGiveUpAfter returns the effective bound on the CNI DEL retry loop.
func (c AetherConf) NetnsDelGiveUpAfter() time.Duration {
	switch {
	case c.NetnsDelGiveUpAfterSeconds == 0:
		return defaultNetnsDelGiveUpAfter
	case c.NetnsDelGiveUpAfterSeconds < 0:
		return 0
	default:
		return time.Duration(c.NetnsDelGiveUpAfterSeconds) * time.Second
	}
}

// RuntimeConfig holds container runtime-provided configuration passed to the CNI plugin.
type RuntimeConfig struct {
	// PodAnnotations contains Kubernetes pod annotations
	PodAnnotations *map[string]string `json:"io.kubernetes.cri.pod-annotations,omitempty"`
}

// K8sArgs represents Kubernetes-specific CNI arguments passed via the CNI_ARGS environment variable.
// The field names must exactly match the keys in containerd's args for proper unmarshalling.
// See https://github.com/containerd/containerd/blob/main/pkg/cri/server/sandbox_run.go
type K8sArgs struct {
	types.CommonArgs

	// K8S_POD_NAME is the pod's name in Kubernetes
	K8S_POD_NAME types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_NAMESPACE is the pod's namespace in Kubernetes
	K8S_POD_NAMESPACE types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_INFRA_CONTAINER_ID is the pod's sandbox (infrastructure) container ID
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_UID is the pod's unique identifier in Kubernetes
	K8S_POD_UID types.UnmarshallableString // nolint: revive, stylecheck
}

// NewConf parses CNI configuration from JSON-formatted stdin data.
// It unmarshals the data into AetherConf, sets the agent CNI path default if not provided,
// and parses the previous plugin result using the standard CNI version negotiation.
// Returns an error if the JSON is invalid or the previous result cannot be parsed.
func NewConf(stdinData []byte) (AetherConf, error) {
	c := AetherConf{}
	if err := json.Unmarshal(stdinData, &c); err != nil {
		return c, fmt.Errorf("failed to load netconf: %w %q", err, string(stdinData))
	}
	if err := version.ParsePrevResult(&c.PluginConf); err != nil {
		return c, err
	}

	if c.AgentCNIPath == "" {
		c.AgentCNIPath = agentConstans.DefaultCNISocketPath
	}

	return c, nil
}
