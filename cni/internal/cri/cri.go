package cri

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	defaultCRISocket = "unix:///run/containerd/containerd.sock"
)

// GetContainerPID queries the CRI for the container's PID using the container ID.
func GetContainerPID(ctx context.Context, criSocket, containerID string) (uint32, error) {
	if criSocket == "" {
		criSocket = defaultCRISocket
	}

	conn, err := grpc.NewClient(criSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to create CRI client: %w", err)
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)

	client := runtimeapi.NewRuntimeServiceClient(conn)

	// ContainerStatus with verbose=true includes the PID in the info map
	resp, err := client.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{
		ContainerId: containerID,
		Verbose:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get container status: %w", err)
	}

	// The PID is in resp.Info["pid"] for some runtimes,
	// or you need to parse resp.Info["info"] as JSON
	return parsePIDFromInfo(resp.Info)
}

// containerdInfo represents the nested JSON in the "info" key.
type containerdInfo struct {
	PID int `json:"pid"`
}

func parsePIDFromInfo(info map[string]string) (uint32, error) {
	// Some runtimes put pid directly
	if pidStr, ok := info["pid"]; ok {
		pid, err := strconv.ParseUint(pidStr, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("failed to parse pid %q: %w", pidStr, err)
		}
		return uint32(pid), nil
	}

	// containerd puts it in a JSON blob under "info"
	infoJSON, ok := info["info"]
	if !ok {
		return 0, fmt.Errorf("no 'info' or 'pid' key in container status info")
	}

	var ci containerdInfo
	if err := json.Unmarshal([]byte(infoJSON), &ci); err != nil {
		return 0, fmt.Errorf("failed to unmarshal info JSON: %w", err)
	}

	if ci.PID == 0 {
		return 0, fmt.Errorf("container PID is 0 (container may not be running)")
	}

	return uint32(ci.PID), nil
}
