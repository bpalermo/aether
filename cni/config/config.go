// Package config provides CNI plugin configuration parsing and data structures.
//
// The CNI plugin configuration is provided as JSON via stdin during plugin invocation.
// It includes the standard CNI PluginConf fields plus Aether-specific fields for
// agent communication and container runtime integration.
package config

import (
	"encoding/json"
	"fmt"

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

	// RuntimeConfig holds runtime-provided configuration like pod annotations
	RuntimeConfig *RuntimeConfig `json:"runtimeConfig,omitempty"`
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
