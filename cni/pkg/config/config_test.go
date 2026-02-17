package config

import (
	"encoding/json"
	"testing"

	agentConstans "github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConf(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedConf  AetherConf
		expectedError bool
		errorContains string
	}{
		{
			name: "valid config with all fields",
			input: `{
				"cniVersion": "1.0.0",
				"name": "aether-network",
				"type": "aether",
				"agent_cni_path": "/custom/path/cni.sock",
				"runtimeConfig": {
					"io.kubernetes.cri.pod-annotations": {
						"annotation1": "value1",
						"annotation2": "value2"
					}
				}
			}`,
			expectedConf: AetherConf{
				PluginConf: types.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "aether-network",
					Type:       "aether",
				},
				AgentCNIPath: "/custom/path/cni.sock",
				RuntimeConfig: &RuntimeConfig{
					PodAnnotations: &map[string]string{
						"annotation1": "value1",
						"annotation2": "value2",
					},
				},
			},
			expectedError: false,
		},
		{
			name: "config without agent_cni_path uses default",
			input: `{
				"cniVersion": "1.0.0",
				"name": "aether-network",
				"type": "aether"
			}`,
			expectedConf: AetherConf{
				PluginConf: types.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "aether-network",
					Type:       "aether",
				},
				AgentCNIPath: agentConstans.DefaultCNISocketPath,
			},
			expectedError: false,
		},
		{
			name: "config with empty agent_cni_path uses default",
			input: `{
				"cniVersion": "1.0.0",
				"name": "aether-network",
				"type": "aether",
				"agent_cni_path": ""
			}`,
			expectedConf: AetherConf{
				PluginConf: types.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "aether-network",
					Type:       "aether",
				},
				AgentCNIPath: agentConstans.DefaultCNISocketPath,
			},
			expectedError: false,
		},
		{
			name: "config without runtime config",
			input: `{
				"cniVersion": "1.0.0",
				"name": "aether-network",
				"type": "aether",
				"agent_cni_path": "/var/run/aether/cni.sock"
			}`,
			expectedConf: AetherConf{
				PluginConf: types.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "aether-network",
					Type:       "aether",
				},
				AgentCNIPath:  "/var/run/aether/cni.sock",
				RuntimeConfig: nil,
			},
			expectedError: false,
		},
		{
			name: "config with prevResult",
			input: `{
				"cniVersion": "1.0.0",
				"name": "aether-network",
				"type": "aether",
				"agent_cni_path": "/var/run/aether/cni.sock",
				"prevResult": {
					"cniVersion": "1.0.0",
					"interfaces": []
				}
			}`,
			expectedConf: AetherConf{
				PluginConf: types.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "aether-network",
					Type:       "aether",
				},
				AgentCNIPath: "/var/run/aether/cni.sock",
			},
			expectedError: false,
		},
		{
			name:          "invalid JSON",
			input:         `{invalid json}`,
			expectedError: true,
			errorContains: "failed to load netconf",
		},
		{
			name:          "empty input",
			input:         ``,
			expectedError: true,
			errorContains: "failed to load netconf",
		},
		{
			name:  "null input",
			input: `null`,
			expectedConf: AetherConf{
				AgentCNIPath: "/run/aether/cni.sock",
			},
			expectedError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf, err := NewConf([]byte(tt.input))

			if tt.expectedError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedConf.CNIVersion, conf.CNIVersion)
			assert.Equal(t, tt.expectedConf.Name, conf.Name)
			assert.Equal(t, tt.expectedConf.Type, conf.Type)
			assert.Equal(t, tt.expectedConf.AgentCNIPath, conf.AgentCNIPath)

			if tt.expectedConf.RuntimeConfig != nil {
				require.NotNil(t, conf.RuntimeConfig)
				if tt.expectedConf.RuntimeConfig.PodAnnotations != nil {
					require.NotNil(t, conf.RuntimeConfig.PodAnnotations)
					assert.Equal(t, *tt.expectedConf.RuntimeConfig.PodAnnotations, *conf.RuntimeConfig.PodAnnotations)
				}
			} else {
				assert.Nil(t, conf.RuntimeConfig)
			}
		})
	}
}

func TestAetherConf_Marshal(t *testing.T) {
	annotations := map[string]string{
		"app": "test",
		"env": "dev",
	}

	conf := AetherConf{
		PluginConf: types.PluginConf{
			CNIVersion: "1.0.0",
			Name:       "test-network",
			Type:       "aether",
		},
		AgentCNIPath: "/test/path/cni.sock",
		RuntimeConfig: &RuntimeConfig{
			PodAnnotations: &annotations,
		},
	}

	data, err := json.Marshal(conf)
	require.NoError(t, err)

	var unmarshalled AetherConf
	err = json.Unmarshal(data, &unmarshalled)
	require.NoError(t, err)

	assert.Equal(t, conf.CNIVersion, unmarshalled.CNIVersion)
	assert.Equal(t, conf.Name, unmarshalled.Name)
	assert.Equal(t, conf.Type, unmarshalled.Type)
	assert.Equal(t, conf.AgentCNIPath, unmarshalled.AgentCNIPath)
	require.NotNil(t, unmarshalled.RuntimeConfig)
	require.NotNil(t, unmarshalled.RuntimeConfig.PodAnnotations)
	assert.Equal(t, annotations, *unmarshalled.RuntimeConfig.PodAnnotations)
}

func TestK8sArgs(t *testing.T) {
	args := K8sArgs{
		CommonArgs:                 types.CommonArgs{},
		K8S_POD_NAME:               types.UnmarshallableString("test-pod"),
		K8S_POD_NAMESPACE:          types.UnmarshallableString("default"),
		K8S_POD_INFRA_CONTAINER_ID: types.UnmarshallableString("infra123"),
		K8S_POD_UID:                types.UnmarshallableString("uid-123-456"),
	}

	// Verify fields are set correctly
	assert.Equal(t, types.UnmarshallableString("test-pod"), args.K8S_POD_NAME)
	assert.Equal(t, types.UnmarshallableString("default"), args.K8S_POD_NAMESPACE)
	assert.Equal(t, types.UnmarshallableString("infra123"), args.K8S_POD_INFRA_CONTAINER_ID)
	assert.Equal(t, types.UnmarshallableString("uid-123-456"), args.K8S_POD_UID)
}

func TestRuntimeConfig_NilSafety(t *testing.T) {
	// Test that nil RuntimeConfig doesn't cause issues
	conf := AetherConf{
		PluginConf: types.PluginConf{
			CNIVersion: "1.0.0",
			Name:       "test",
			Type:       "aether",
		},
		RuntimeConfig: nil,
	}

	data, err := json.Marshal(conf)
	require.NoError(t, err)

	var unmarshalled AetherConf
	err = json.Unmarshal(data, &unmarshalled)
	require.NoError(t, err)
	assert.Nil(t, unmarshalled.RuntimeConfig)
}
