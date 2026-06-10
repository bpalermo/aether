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

	agentConstans "github.com/bpalermo/aether/agent/constants"
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
	// local pods inside their netns by filepath, and a dial racing the
	// runtime's netns removal segfaults Envoy (upstream bug; e2e findings
	// 2026-06-10). CNI ADD bind-mounts the netns to an aether-owned path that
	// stays valid until the agent has confirmed the pod's xDS resources are
	// gone, so late dials fail gracefully instead of crashing the proxy.
	NetnsPinDisabled bool `json:"netns_pin_disabled"`
	// NetnsPinDir is where CNI ADD bind-mounts each pod's netns. Must be a
	// host path visible to the aether-proxy container (which mounts /run/aether
	// with HostToContainer propagation). Empty = /run/aether/netns.
	NetnsPinDir string `json:"netns_pin_dir"`
	// NetnsUnpinDelaySeconds is how long CNI DEL waits after the agent has
	// deregistered the pod (and Envoy acked the listener removal) before
	// unpinning the netns, covering Envoy's deferred cluster destruction and
	// connection-pool drains that can still dial for a few seconds.
	// 0 = 10s default; negative = no delay.
	NetnsUnpinDelaySeconds int `json:"netns_unpin_delay_seconds"`

	// RuntimeConfig holds runtime-provided configuration like pod annotations
	RuntimeConfig *RuntimeConfig `json:"runtimeConfig,omitempty"`
}

// defaultNetnsPinDir lives under /run/aether, which the aether-proxy DaemonSet
// already mounts with HostToContainer propagation, so pinned netns mounts made
// by the (host-side) plugin become visible to Envoy without chart changes.
const defaultNetnsPinDir = "/run/aether/netns"

// defaultNetnsUnpinDelay covers the post-removal dial window observed on
// talos-main (health checkers / connection pools dialing up to ~5-9s after the
// listener and clusters were removed from the snapshot).
const defaultNetnsUnpinDelay = 10 * time.Second

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
