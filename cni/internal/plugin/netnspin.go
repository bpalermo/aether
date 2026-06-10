package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/bpalermo/aether/cni/config"
	"go.uber.org/zap"
)

// Netns pinning (e2e findings 2026-06-10, finding 1).
//
// The agent points Envoy at each local pod via the pod's netns *filepath*
// (upstream source-address network_namespace_filepath). Envoy 1.38's error path
// for a vanished netns returns a nullptr connection and the caller segfaults,
// so any dial racing the runtime's netns teardown takes down the whole node
// proxy — and a stale path left in agent storage makes the proxy unbootable
// (cold-start health checkers dial immediately).
//
// CNI ADD therefore bind-mounts the runtime netns to an aether-owned pin path
// and registers *that* path with the agent. The bind mount keeps the netns
// alive (and the path open-able) independent of the runtime's teardown; CNI DEL
// unpins only after the agent has deregistered the pod and Envoy acked the
// config removal, plus a grace delay for deferred dials. A late dial against a
// pinned-but-dead netns fails with a clean connection error instead of ENOENT.

// pinNetns bind-mounts netns onto the pin path for containerID and returns the
// pinned path. A pre-existing pin for the same container (retried ADD) is
// replaced.
func (p *AetherPlugin) pinNetns(conf config.AetherConf, netns, containerID string) (string, error) {
	target := conf.NetnsPinPath(containerID)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("creating netns pin dir: %w", err)
	}

	// Retried ADD: drop any previous pin before re-mounting.
	_ = syscall.Unmount(target, 0)
	_ = os.Remove(target)

	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating netns pin mount point: %w", err)
	}
	_ = f.Close()

	if err := syscall.Mount(netns, target, "", syscall.MS_BIND, ""); err != nil {
		_ = os.Remove(target)
		return "", fmt.Errorf("bind-mounting netns %s to %s: %w", netns, target, err)
	}
	return target, nil
}

// unpinNetns removes the pin for containerID. A missing pin is not an error
// (pinning may have failed at ADD, or a previous DEL already cleaned up). EBUSY
// (a dial mid-setns) falls back to a lazy detach: the path disappears now and
// the namespace is released when its last opener closes it.
func (p *AetherPlugin) unpinNetns(conf config.AetherConf, containerID string) error {
	target := conf.NetnsPinPath(containerID)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return nil
	}
	// Unmount errors (EINVAL: not a mount point because pin creation failed
	// mid-way) are judged by the Remove that follows: a still-mounted target
	// fails Remove with EBUSY, which is the real failure signal.
	unmountErr := syscall.Unmount(target, 0)
	if unmountErr != nil {
		// EBUSY (a dial mid-setns): lazily detach so the path disappears now and
		// the namespace is released when its last opener closes it.
		if errors.Is(unmountErr, syscall.EBUSY) {
			_ = syscall.Unmount(target, syscall.MNT_DETACH)
		}
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing netns pin %s (unmount: %v): %w", target, unmountErr, err)
	}
	return nil
}

// gcAttachments is the CNI GC payload subset listing still-valid attachments.
type gcAttachments struct {
	ValidAttachments []struct {
		ContainerID string `json:"containerID"`
	} `json:"cni.dev/valid-attachments"`
}

// sweepNetnsPins unpins every entry in the pin dir whose container ID is not in
// the valid set — orphans left by DELs that never completed (e.g. the agent was
// down, so the pin was deliberately retained). Best-effort.
func (p *AetherPlugin) sweepNetnsPins(conf config.AetherConf, stdinData []byte) {
	var gc gcAttachments
	if err := json.Unmarshal(stdinData, &gc); err != nil {
		p.logger.Warn("netns pin sweep: failed to parse GC payload", zap.Error(err))
		return
	}
	valid := make(map[string]struct{}, len(gc.ValidAttachments))
	for _, a := range gc.ValidAttachments {
		valid[a.ContainerID] = struct{}{}
	}

	dir := filepath.Dir(conf.NetnsPinPath("x"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			p.logger.Warn("netns pin sweep: failed to read pin dir", zap.String("dir", dir), zap.Error(err))
		}
		return
	}
	for _, e := range entries {
		if _, ok := valid[e.Name()]; ok {
			continue
		}
		if err := p.unpinNetns(conf, e.Name()); err != nil {
			p.logger.Warn("netns pin sweep: failed to unpin orphan", zap.String("containerID", e.Name()), zap.Error(err))
		} else {
			p.logger.Info("netns pin sweep: unpinned orphan", zap.String("containerID", e.Name()))
		}
	}
}
