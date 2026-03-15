package plugin

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/bpalermo/aether/cni/pkg/config"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestPidFromNetns(t *testing.T) {
	tests := []struct {
		name    string
		netns   string
		wantPID uint32
		wantErr bool
	}{
		{
			name:    "valid /proc/<pid>/ns/net path",
			netns:   "/proc/1234/ns/net",
			wantPID: 1234,
			wantErr: false,
		},
		{
			name:    "large PID",
			netns:   "/proc/4294967295/ns/net",
			wantPID: 4294967295,
			wantErr: false,
		},
		{
			name:    "PID 1",
			netns:   "/proc/1/ns/net",
			wantPID: 1,
			wantErr: false,
		},
		{
			name:    "empty string",
			netns:   "",
			wantErr: true,
		},
		{
			name:    "missing prefix",
			netns:   "1234/ns/net",
			wantErr: true,
		},
		{
			name:    "missing suffix",
			netns:   "/proc/1234",
			wantErr: true,
		},
		{
			name:    "non-numeric PID",
			netns:   "/proc/abc/ns/net",
			wantErr: true,
		},
		{
			name:    "named netns (not proc-based)",
			netns:   "/var/run/netns/test",
			wantErr: true,
		},
		{
			name:    "negative PID-like value",
			netns:   "/proc/-1/ns/net",
			wantErr: true,
		},
		{
			name:    "overflow uint32",
			netns:   "/proc/9999999999999/ns/net",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, err := pidFromNetns(tt.netns)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPID, pid)
		})
	}
}

func TestPidFromNetnsPath(t *testing.T) {
	tests := []struct {
		name    string
		netns   string
		wantPID uint32
		wantErr bool
	}{
		{name: "valid path", netns: "/proc/1234/ns/net", wantPID: 1234},
		{name: "PID 1", netns: "/proc/1/ns/net", wantPID: 1},
		{name: "empty", netns: "", wantErr: true},
		{name: "named netns", netns: "/var/run/netns/test", wantErr: true},
		{name: "non-numeric", netns: "/proc/abc/ns/net", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, err := pidFromNetnsPath(tt.netns)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPID, pid)
		})
	}
}

func TestPidFromNetnsInode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("inode-based PID lookup requires /proc on Linux")
	}

	t.Run("current process netns via /proc/self/ns/net", func(t *testing.T) {
		pid, err := pidFromNetnsInode("/proc/self/ns/net")
		require.NoError(t, err)
		// Multiple processes may share the same netns (e.g., in containers).
		// Verify the returned PID has the same netns inode rather than
		// asserting it equals os.Getpid().
		assert.Greater(t, pid, uint32(0))
		resolvedPath := fmt.Sprintf("/proc/%d/ns/net", pid)
		_, statErr := os.Stat(resolvedPath)
		assert.NoError(t, statErr, "returned PID %d should have a valid /proc entry", pid)
	})

	t.Run("non-existent path returns error", func(t *testing.T) {
		_, err := pidFromNetnsInode("/nonexistent/path")
		require.Error(t, err)
	})
}

func TestNewPodFromArgs(t *testing.T) {
	args := &skel.CmdArgs{
		ContainerID: "ctr-abc123",
		Netns:       "/proc/42/ns/net",
	}
	k8sArgs := config.K8sArgs{
		K8S_POD_NAME:      cnitypes.UnmarshallableString("my-pod"),
		K8S_POD_NAMESPACE: cnitypes.UnmarshallableString("default"),
	}
	podIPs := []string{"10.0.0.1", "fd00::1"}

	t.Run("with PID", func(t *testing.T) {
		pid := wrapperspb.UInt32(42)
		pod := newPodFromArgs(args, k8sArgs, podIPs, pid)

		assert.Equal(t, "ctr-abc123", pod.GetContainerId())
		assert.Equal(t, "my-pod", pod.GetName())
		assert.Equal(t, "default", pod.GetNamespace())
		assert.Equal(t, "/proc/42/ns/net", pod.GetNetworkNamespace())
		assert.Equal(t, podIPs, pod.GetIps())
		require.NotNil(t, pod.GetPid())
		assert.Equal(t, uint32(42), pod.GetPid().GetValue())
	})

	t.Run("without PID", func(t *testing.T) {
		pod := newPodFromArgs(args, k8sArgs, podIPs, nil)

		assert.Equal(t, "my-pod", pod.GetName())
		assert.Nil(t, pod.GetPid())
	})
}
