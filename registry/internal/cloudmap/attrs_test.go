package cloudmap

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/servicediscovery/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullEndpoint returns a ServiceEndpoint with all fields populated for round-trip tests.
func fullEndpoint() *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.1",
		ClusterName: "cluster-a",
		Port:        8080,
		Weight:      100,
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: "us-east-1",
			Zone:   "us-east-1a",
		},
		Metadata: map[string]string{
			"env":     "prod",
			"version": "v1.2.3",
		},
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      "abc123",
			NetworkNamespace: "/var/run/netns/abc",
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "my-pod-xyz",
			NodeName:  "node-1",
		},
	}
}

func TestMarshalAttrsUnmarshalEndpointRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		protocol registryv1.Service_Protocol
		endpoint *registryv1.ServiceEndpoint
		// expected is the endpoint we expect back from unmarshalEndpoint.
		// If nil, we expect the input endpoint to round-trip unchanged.
		expected *registryv1.ServiceEndpoint
	}{
		{
			name:     "full endpoint with HTTP protocol",
			protocol: registryv1.Service_HTTP,
			endpoint: fullEndpoint(),
		},
		{
			name:     "full endpoint with unspecified protocol",
			protocol: registryv1.Service_PROTOCOL_UNSPECIFIED,
			endpoint: fullEndpoint(),
		},
		{
			name:     "minimal endpoint — only required fields",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "192.168.1.1",
				ClusterName: "my-cluster",
				Port:        443,
				Weight:      1,
			},
		},
		{
			name:     "zero weight and zero port",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "172.16.0.1",
				ClusterName: "cluster-b",
				Port:        0,
				Weight:      0,
			},
		},
		{
			name:     "nil locality — locality omitted from attrs",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.1.2.3",
				ClusterName: "cluster-c",
				Port:        9090,
				Weight:      50,
				Locality:    nil,
			},
		},
		{
			name:     "nil metadata map — no metadata attr",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.2.3.4",
				ClusterName: "cluster-d",
				Port:        80,
				Weight:      10,
				Metadata:    nil,
			},
		},
		{
			name:     "nil container metadata — no container attrs",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:               "10.3.4.5",
				ClusterName:      "cluster-e",
				Port:             80,
				Weight:           1,
				ContainerMetadata: nil,
			},
		},
		{
			name:     "nil kubernetes metadata — no k8s attrs",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:                 "10.4.5.6",
				ClusterName:        "cluster-f",
				Port:               80,
				Weight:             1,
				KubernetesMetadata: nil,
			},
		},
		{
			name:     "locality with only region",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.5.6.7",
				ClusterName: "cluster-g",
				Port:        8080,
				Weight:      1,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "eu-west-1",
					Zone:   "",
				},
			},
			// Zone is empty so the Locality struct will have only Region set.
			// unmarshalEndpoint sets Locality when region != "" || zone != "", so we get it back.
			expected: &registryv1.ServiceEndpoint{
				Ip:          "10.5.6.7",
				ClusterName: "cluster-g",
				Port:        8080,
				Weight:      1,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "eu-west-1",
					Zone:   "",
				},
			},
		},
		{
			name:     "locality with only zone",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.6.7.8",
				ClusterName: "cluster-h",
				Port:        8080,
				Weight:      1,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "",
					Zone:   "ap-southeast-1a",
				},
			},
			expected: &registryv1.ServiceEndpoint{
				Ip:          "10.6.7.8",
				ClusterName: "cluster-h",
				Port:        8080,
				Weight:      1,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "",
					Zone:   "ap-southeast-1a",
				},
			},
		},
		{
			name:     "locality with both region and zone empty — locality not restored",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.7.8.9",
				ClusterName: "cluster-i",
				Port:        8080,
				Weight:      1,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "",
					Zone:   "",
				},
			},
			// marshalAttrs skips empty region/zone; unmarshalEndpoint won't reconstruct
			// the Locality pointer when both are absent.
			expected: &registryv1.ServiceEndpoint{
				Ip:          "10.7.8.9",
				ClusterName: "cluster-i",
				Port:        8080,
				Weight:      1,
				Locality:    nil,
			},
		},
		{
			name:     "empty metadata map — no metadata attr",
			protocol: registryv1.Service_HTTP,
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.8.9.10",
				ClusterName: "cluster-j",
				Port:        80,
				Weight:      1,
				Metadata:    map[string]string{},
			},
			// Empty map is treated like nil: marshalAttrs only writes when len > 0.
			expected: &registryv1.ServiceEndpoint{
				Ip:          "10.8.9.10",
				ClusterName: "cluster-j",
				Port:        80,
				Weight:      1,
				Metadata:    nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := marshalAttrs(tt.protocol, tt.endpoint)

			got, err := unmarshalEndpoint(attrs)
			require.NoError(t, err)

			want := tt.expected
			if want == nil {
				want = tt.endpoint
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestMarshalAttrsContainsRequiredKeys(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "my-cluster",
		Port:        8080,
		Weight:      1,
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	assert.Equal(t, "1.2.3.4", attrs[attrIPv4])
	assert.Equal(t, "8080", attrs[attrPort])
	assert.Equal(t, "my-cluster", attrs[attrCluster])
	assert.Equal(t, registryv1.Service_HTTP.String(), attrs[attrProtocol])
	assert.Equal(t, "1", attrs[attrWeight])
	assert.Equal(t, controllerName, attrs[attrController])
}

func TestMarshalAttrsLocalityKeys(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "c",
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: "us-west-2",
			Zone:   "us-west-2b",
		},
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	assert.Equal(t, "us-west-2", attrs[attrRegion])
	assert.Equal(t, "us-west-2b", attrs[attrZone])
}

func TestMarshalAttrsNilLocalityOmitsKeys(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "c",
		Locality:    nil,
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	_, hasRegion := attrs[attrRegion]
	_, hasZone := attrs[attrZone]
	assert.False(t, hasRegion, "region attr should be absent when locality is nil")
	assert.False(t, hasZone, "zone attr should be absent when locality is nil")
}

func TestMarshalAttrsKubernetesMetadataKeys(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "c",
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "kube-system",
			PodName:   "coredns-abc",
			NodeName:  "node-42",
		},
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	assert.Equal(t, "kube-system", attrs[attrK8sNamespace])
	assert.Equal(t, "coredns-abc", attrs[attrK8sPod])
	assert.Equal(t, "node-42", attrs[attrK8sNode])
}

func TestMarshalAttrsContainerMetadataKeys(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "c",
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      "ctr-xyz",
			NetworkNamespace: "/var/run/netns/xyz",
		},
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	assert.Equal(t, "ctr-xyz", attrs[attrContainerID])
	assert.Equal(t, "/var/run/netns/xyz", attrs[attrNetworkNS])
}

func TestMarshalAttrsMetadataJSON(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "1.2.3.4",
		ClusterName: "c",
		Metadata: map[string]string{
			"key1": "val1",
		},
	}

	attrs := marshalAttrs(registryv1.Service_HTTP, ep)

	assert.NotEmpty(t, attrs[attrMetadata], "metadata attr should be present")
	// Verify the JSON round-trips to the original map via unmarshal.
	got, err := unmarshalEndpoint(attrs)
	require.NoError(t, err)
	assert.Equal(t, ep.GetMetadata(), got.GetMetadata())
}

func TestUnmarshalEndpointErrors(t *testing.T) {
	tests := []struct {
		name    string
		attrs   map[string]string
		wantErr bool
	}{
		{
			name:    "missing IPv4 attribute returns error",
			attrs:   map[string]string{},
			wantErr: true,
		},
		{
			name: "empty IPv4 attribute returns error",
			attrs: map[string]string{
				attrIPv4: "",
			},
			wantErr: true,
		},
		{
			name: "valid minimal attrs succeeds",
			attrs: map[string]string{
				attrIPv4: "10.0.0.1",
			},
			wantErr: false,
		},
		{
			name: "invalid port is parsed as zero without error",
			attrs: map[string]string{
				attrIPv4: "10.0.0.1",
				attrPort: "not-a-number",
			},
			wantErr: false,
		},
		{
			name: "invalid weight is parsed as zero without error",
			attrs: map[string]string{
				attrIPv4:   "10.0.0.1",
				attrWeight: "not-a-number",
			},
			wantErr: false,
		},
		{
			name: "invalid metadata JSON is silently ignored",
			attrs: map[string]string{
				attrIPv4:    "10.0.0.1",
				attrMetadata: "{invalid json}",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unmarshalEndpoint(tt.attrs)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, got)
			}
		})
	}
}

func TestUnmarshalEndpointFromSummary(t *testing.T) {
	tests := []struct {
		name    string
		summary types.HttpInstanceSummary
		want    *registryv1.ServiceEndpoint
		wantErr bool
	}{
		{
			name: "summary with all attrs produces correct endpoint",
			summary: types.HttpInstanceSummary{
				Attributes: map[string]string{
					attrIPv4:    "10.0.1.1",
					attrPort:    "9090",
					attrCluster: "cluster-z",
					attrWeight:  "50",
					attrRegion:  "us-east-1",
					attrZone:    "us-east-1b",
				},
			},
			want: &registryv1.ServiceEndpoint{
				Ip:          "10.0.1.1",
				Port:        9090,
				ClusterName: "cluster-z",
				Weight:      50,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "us-east-1",
					Zone:   "us-east-1b",
				},
			},
			wantErr: false,
		},
		{
			name: "summary without IPv4 returns error",
			summary: types.HttpInstanceSummary{
				Attributes: map[string]string{
					attrPort: "8080",
				},
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unmarshalEndpointFromSummary(tt.summary)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestProtocolFromAttrs(t *testing.T) {
	tests := []struct {
		name     string
		attrs    map[string]string
		expected registryv1.Service_Protocol
	}{
		{
			name: "HTTP protocol string resolves to Service_HTTP",
			attrs: map[string]string{
				attrProtocol: registryv1.Service_HTTP.String(),
			},
			expected: registryv1.Service_HTTP,
		},
		{
			name: "PROTOCOL_UNSPECIFIED string resolves to Service_PROTOCOL_UNSPECIFIED",
			attrs: map[string]string{
				attrProtocol: registryv1.Service_PROTOCOL_UNSPECIFIED.String(),
			},
			expected: registryv1.Service_PROTOCOL_UNSPECIFIED,
		},
		{
			name:     "missing protocol attr returns PROTOCOL_UNSPECIFIED",
			attrs:    map[string]string{},
			expected: registryv1.Service_PROTOCOL_UNSPECIFIED,
		},
		{
			name: "unknown protocol string returns PROTOCOL_UNSPECIFIED",
			attrs: map[string]string{
				attrProtocol: "GRPC",
			},
			expected: registryv1.Service_PROTOCOL_UNSPECIFIED,
		},
		{
			name: "empty protocol string returns PROTOCOL_UNSPECIFIED",
			attrs: map[string]string{
				attrProtocol: "",
			},
			expected: registryv1.Service_PROTOCOL_UNSPECIFIED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := protocolFromAttrs(tt.attrs)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestInstanceID(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		ip          string
		expected    string
	}{
		{
			name:        "standard cluster and IP",
			clusterName: "cluster-a",
			ip:          "10.0.0.1",
			expected:    "cluster-a/10.0.0.1",
		},
		{
			name:        "empty cluster name",
			clusterName: "",
			ip:          "10.0.0.1",
			expected:    "/10.0.0.1",
		},
		{
			name:        "empty IP",
			clusterName: "cluster-a",
			ip:          "",
			expected:    "cluster-a/",
		},
		{
			name:        "both empty",
			clusterName: "",
			ip:          "",
			expected:    "/",
		},
		{
			name:        "cluster name with slashes",
			clusterName: "a/b",
			ip:          "1.2.3.4",
			expected:    "a/b/1.2.3.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := instanceID(tt.clusterName, tt.ip)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestInstanceIDFormat(t *testing.T) {
	// Verify the ID uses the expected "clusterName/ip" separator format.
	got := instanceID("prod-cluster", "192.168.0.5")
	assert.Equal(t, fmt.Sprintf("%s/%s", "prod-cluster", "192.168.0.5"), got)
}
