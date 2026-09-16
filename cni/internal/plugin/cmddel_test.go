package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"aethermesh.dev/cni/config"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CNI DEL against a missing / erroring agent (#796).
//
// The contract under test: a DEL that cannot REACH the agent completes anyway
// (containerd would otherwise retry forever while the Terminating pod holds its
// CPU request and starves the node's replacement DaemonSet pods), while a DEL
// an agent actively answers with an error is still retried — but only up to
// NetnsDelGiveUpAfter.

const testContainerID = "cid1"

// delTestArgs builds the CmdDel inputs for a mesh-managed pod.
func delTestArgs(socketPath, pinDir string, giveUpSeconds int) *skel.CmdArgs {
	stdin := fmt.Sprintf(
		`{"cniVersion":"1.0.0","name":"aether","type":"aether-cni","agent_cni_path":%q,"netns_pin_dir":%q,"netns_del_give_up_after_seconds":%d}`,
		socketPath, pinDir, giveUpSeconds)
	return &skel.CmdArgs{
		ContainerID: testContainerID,
		Netns:       "/proc/self/ns/net",
		IfName:      "eth0",
		Args:        "K8S_POD_NAMESPACE=demo;K8S_POD_NAME=app-1;K8S_POD_INFRA_CONTAINER_ID=" + testContainerID,
		StdinData:   []byte(stdin),
	}
}

// unpinCall records one scheduled detached unpin.
type unpinCall struct {
	target string
	delay  time.Duration
}

// stubSpawnUnpin replaces the detached-unpin spawn (no process is forked) and
// returns the slice the calls land in.
func stubSpawnUnpin(t *testing.T) *[]unpinCall {
	t.Helper()
	calls := &[]unpinCall{}
	orig := spawnDetachedUnpinFn
	spawnDetachedUnpinFn = func(_ *AetherPlugin, target string, delay time.Duration) error {
		*calls = append(*calls, unpinCall{target: target, delay: delay})
		return nil
	}
	t.Cleanup(func() { spawnDetachedUnpinFn = orig })
	return calls
}

// stubDelProbe replaces the DEL-side readiness wait and returns the call count.
func stubDelProbe(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	orig := delReadinessWait
	delReadinessWait = func(context.Context, string) error {
		*calls++
		return nil
	}
	t.Cleanup(func() { delReadinessWait = orig })
	return calls
}

// startUnixCNIServer serves svc on a real Unix socket (the plugin builds its own
// client from the netconf path, so bufconn cannot stand in here) and returns
// that path. The dir is short to stay inside the AF_UNIX path budget.
func startUnixCNIServer(t *testing.T, svc cniv1.CNIServiceServer) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cni-del-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "cni.sock")
	lis, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	server := grpc.NewServer()
	cniv1.RegisterCNIServiceServer(server, svc)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	return socketPath
}

// TestCmdDelAgentUnreachableSucceedsAndUnpins: the agent's socket is not there
// at all. Before #796 this returned the dial error and containerd retried the
// DEL until something else broke the loop.
func TestCmdDelAgentUnreachableSucceedsAndUnpins(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "absent.sock")
	pinDir := filepath.Join(dir, "netns")

	unpins := stubSpawnUnpin(t)
	probes := stubDelProbe(t)

	p := NewAetherPlugin(zap.NewNop())
	require.NoError(t, p.CmdDel(delTestArgs(socketPath, pinDir, 0)))

	// The pin is released on the ordinary delay, not held for an ACK nobody
	// can give.
	require.Len(t, *unpins, 1)
	assert.Equal(t, filepath.Join(pinDir, testContainerID), (*unpins)[0].target)
	assert.Equal(t, 60*time.Second, (*unpins)[0].delay)

	// No agent means no listener removal to confirm: waiting could only burn
	// the probe's whole timeout.
	assert.Zero(t, *probes)

	// The give-up marker belongs to the live-agent path only.
	assert.NoFileExists(t, filepath.Join(pinDir, testContainerID+delFailSuffix))
}

// TestCmdDelAgentErrorRetriesAndMarks: a LIVE agent answered with an error. It
// still owns the pod's xDS resources and will ACK a retry, so the DEL fails
// back to the runtime and the netns stays pinned — but the first failure is
// recorded so the retry loop is bounded.
func TestCmdDelAgentErrorRetriesAndMarks(t *testing.T) {
	socketPath := startUnixCNIServer(t, &failingCNIService{
		removePodErr: status.Error(codes.Internal, "registry write failed"),
	})
	pinDir := t.TempDir()

	unpins := stubSpawnUnpin(t)
	probes := stubDelProbe(t)

	p := NewAetherPlugin(zap.NewNop())
	err := p.CmdDel(delTestArgs(socketPath, pinDir, 0))
	require.Error(t, err)

	assert.Empty(t, *unpins, "the pin must be held while a live agent can still ACK")
	assert.Zero(t, *probes)

	marker := filepath.Join(pinDir, testContainerID+delFailSuffix)
	require.FileExists(t, marker)
	first, readErr := readDelFailure(marker)
	require.NoError(t, readErr)
	assert.WithinDuration(t, time.Now(), first, time.Minute)
}

// TestCmdDelAgentErrorPastGiveUpDegrades: the same erroring agent, but this
// container's DEL has been failing for longer than NetnsDelGiveUpAfter. The
// plugin stops pinning the node on it and takes the unreachable path.
func TestCmdDelAgentErrorPastGiveUpDegrades(t *testing.T) {
	socketPath := startUnixCNIServer(t, &failingCNIService{
		removePodErr: status.Error(codes.Internal, "registry write failed"),
	})
	pinDir := t.TempDir()
	marker := filepath.Join(pinDir, testContainerID+delFailSuffix)
	require.NoError(t, os.WriteFile(marker,
		[]byte(time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339Nano)), 0o600))

	unpins := stubSpawnUnpin(t)
	probes := stubDelProbe(t)

	p := NewAetherPlugin(zap.NewNop())
	// giveUpSeconds 0 = the 5m default, and the marker is 10m old.
	require.NoError(t, p.CmdDel(delTestArgs(socketPath, pinDir, 0)))

	require.Len(t, *unpins, 1)
	assert.Equal(t, filepath.Join(pinDir, testContainerID), (*unpins)[0].target)
	assert.Zero(t, *probes)
	assert.NoFileExists(t, marker, "the marker dies with the DEL it bounded")
}

// TestCmdDelAgentErrorWithinGiveUpStillRetries: the marker exists but is young,
// so nothing degrades yet.
func TestCmdDelAgentErrorWithinGiveUpStillRetries(t *testing.T) {
	socketPath := startUnixCNIServer(t, &failingCNIService{
		removePodErr: status.Error(codes.Internal, "registry write failed"),
	})
	pinDir := t.TempDir()
	marker := filepath.Join(pinDir, testContainerID+delFailSuffix)
	written := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339Nano)
	require.NoError(t, os.WriteFile(marker, []byte(written), 0o600))

	unpins := stubSpawnUnpin(t)

	p := NewAetherPlugin(zap.NewNop())
	require.Error(t, p.CmdDel(delTestArgs(socketPath, pinDir, 0)))

	assert.Empty(t, *unpins)
	// The FIRST failure's timestamp is what the bound is measured from, so a
	// later attempt must not refresh it.
	kept, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, written, string(kept))
}

// TestCmdDelAgentOKUnchanged: the ordinary path is untouched — the agent ACKs,
// the DEL waits for the listener to go away, then schedules the unpin.
func TestCmdDelAgentOKUnchanged(t *testing.T) {
	svc := &mockCNIService{}
	socketPath := startUnixCNIServer(t, svc)
	pinDir := t.TempDir()

	unpins := stubSpawnUnpin(t)
	probes := stubDelProbe(t)

	p := NewAetherPlugin(zap.NewNop())
	require.NoError(t, p.CmdDel(delTestArgs(socketPath, pinDir, 0)))

	assert.True(t, svc.removePodCalled)
	assert.Equal(t, "app-1", svc.lastRemovedName)
	assert.Equal(t, 1, *probes)
	require.Len(t, *unpins, 1)
	assert.Equal(t, filepath.Join(pinDir, testContainerID), (*unpins)[0].target)
	assert.NoFileExists(t, filepath.Join(pinDir, testContainerID+delFailSuffix))
}

// TestCmdDelClearsStaleMarkerOnSuccess: a DEL that failed against a live agent
// and then succeeded must not leave its marker behind, or the NEXT container to
// reuse that ID would start its bound already expired.
func TestCmdDelClearsStaleMarkerOnSuccess(t *testing.T) {
	socketPath := startUnixCNIServer(t, &mockCNIService{})
	pinDir := t.TempDir()
	marker := filepath.Join(pinDir, testContainerID+delFailSuffix)
	require.NoError(t, os.WriteFile(marker,
		[]byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)), 0o600))

	stubSpawnUnpin(t)
	stubDelProbe(t)

	p := NewAetherPlugin(zap.NewNop())
	require.NoError(t, p.CmdDel(delTestArgs(socketPath, pinDir, 0)))
	assert.NoFileExists(t, marker)
}

// TestSweepNetnsPinsHandlesDelMarkers: CNI GC must not mistake a marker for a
// pin. Unpinning one would silently reset the give-up bound of a DEL still
// being retried; leaving an orphan's behind would expire the bound of whatever
// container next reuses the ID.
func TestSweepNetnsPinsHandlesDelMarkers(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	conf := config.AetherConf{NetnsPinDir: t.TempDir()}

	keepPin := conf.NetnsPinPath("keep")
	keepMarker := keepPin + delFailSuffix
	orphanPin := conf.NetnsPinPath("orphan")
	orphanMarker := orphanPin + delFailSuffix
	for _, f := range []string{keepPin, keepMarker, orphanPin, orphanMarker} {
		require.NoError(t, os.WriteFile(f, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600))
	}

	p.sweepNetnsPins(conf, []byte(`{"cni.dev/valid-attachments":[{"containerID":"keep"}]}`))

	assert.FileExists(t, keepPin)
	assert.FileExists(t, keepMarker, "a valid attachment's DEL may still be in flight")
	assert.NoFileExists(t, orphanPin)
	assert.NoFileExists(t, orphanMarker)
}

// TestAgentUnreachableClassification pins the classifier itself: a status code
// that means "nobody answered" degrades the DEL, anything the agent actually
// answered does not.
func TestAgentUnreachableClassification(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "cni.sock")
	require.NoError(t, os.WriteFile(live, nil, 0o600))
	conf := config.AetherConf{AgentCNIPath: live}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unavailable", status.Error(codes.Unavailable, "connection refused"), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), true},
		{"canceled", status.Error(codes.Canceled, "context canceled"), true},
		{"internal", status.Error(codes.Internal, "boom"), false},
		{"invalid argument", status.Error(codes.InvalidArgument, "bad pod"), false},
		{"non-status error", fmt.Errorf("removing pod was not successful: %v", "RESULT_UNKNOWN"), false},
		// The plugin wraps every RPC error before returning it.
		{"wrapped unavailable", fmt.Errorf("failed to remove pod from agent: %w", status.Error(codes.Unavailable, "no transport")), true},
		{"wrapped internal", fmt.Errorf("failed to remove pod from agent: %w", status.Error(codes.Internal, "boom")), false},
		{"unwrapped ENOENT", fmt.Errorf("dialing: %w", os.ErrNotExist), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, agentUnreachable(conf, tc.err))
		})
	}

	// A socket that is not on disk at all short-circuits every code: the agent
	// pod is simply not running.
	gone := config.AetherConf{AgentCNIPath: filepath.Join(dir, "absent.sock")}
	assert.True(t, agentUnreachable(gone, status.Error(codes.Internal, "boom")))
	assert.False(t, agentUnreachable(gone, nil))
}
