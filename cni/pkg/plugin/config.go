package plugin

import (
	"encoding/json"
	"fmt"

	"github.com/bpalermo/aether/cni/pkg/constants"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
)

type AetherConf struct {
	types.PluginConf
	RegistryPath string `json:"registry_path"`

	RuntimeConfig *RuntimeConfig `json:"runtimeConfig,omitempty"`
}

type RuntimeConfig struct {
	PodAnnotations *map[string]string `json:"io.kubernetes.cri.pod-annotations,omitempty"`
}

// K8sArgs is the valid CNI_ARGS used for Kubernetes
// The field names need to match exact keys in containerd args for unmarshalling
// https://github.com/containerd/containerd/blob/ced9b18c231a28990617bc0a4b8ce2e81ee2ffa1/pkg/cri/server/sandbox_run.go#L526-L532
type K8sArgs struct {
	types.CommonArgs

	// K8S_POD_NAME is pod's name
	K8S_POD_NAME types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_NAMESPACE is pod's namespace
	K8S_POD_NAMESPACE types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_INFRA_CONTAINER_ID is pod's sandbox id
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString // nolint: revive, stylecheck
	// K8S_POD_UID
	K8S_POD_UID types.UnmarshallableString // nolint: revive, stylecheck
}

func NewConf(stdinData []byte) (AetherConf, error) {
	c := AetherConf{}
	if err := json.Unmarshal(stdinData, &c); err != nil {
		return c, fmt.Errorf("failed to load netconf: %w %q", err, string(stdinData))
	}
	if err := version.ParsePrevResult(&c.PluginConf); err != nil {
		return c, err
	}

	if c.RegistryPath == "" {
		c.RegistryPath = constants.DefaultRegistryPath
	}

	return c, nil
}
