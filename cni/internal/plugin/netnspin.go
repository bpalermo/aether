package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aethermesh.dev/cni/config"
	"go.uber.org/zap"
)

// Netns pinning (e2e findings 2026-06-10, finding 1).
//
// The agent points Envoy at each local pod via the pod's netns *filepath*
// (upstream source-address network_namespace_filepath), so any dial racing the
// runtime's netns teardown opens a path that is already gone.
//
// HISTORICALLY that was a crash. Envoy 1.38's error path for a vanished netns
// returned a nullptr connection which the caller dereferenced, so one racing
// dial took down the whole node proxy, and a stale path left in agent storage
// made the proxy unbootable (cold-start health checkers dial immediately) —
// #112, #245. That is fixed upstream and the fixes are in the pinned proxy
// snapshot (1.40.0-dev.20260904.13144fb):
//
//   - envoyproxy/envoy#45975 (merged 2026-07-13): the pool dial surfaces a
//     FailedToCreateConnection / LocalConnectionFailure — a clean UF for that
//     one request — instead of segfaulting.
//   - envoyproxy/envoy#46503 (merged 2026-08-11): the active TCP/HTTP/gRPC
//     health checkers, which create upstream connections directly and so still
//     crashed after #45975, record a NETWORK health-check failure instead.
//   - envoyproxy/envoy#45976 (opt-in netns validation at config load) is in the
//     snapshot too and stays OFF for us: it would turn a stale pod into an
//     LDS/CDS NACK, which is worse than a clean dial failure.
//
// The pin stays, because the window it covers is still real — just no longer
// fatal. CNI ADD bind-mounts the runtime netns to an aether-owned pin path and
// registers *that* path with the agent; the bind mount keeps the netns alive
// (and the path open-able) independent of the runtime's teardown, so a
// hot-restart successor can re-create the pod's listeners and deferred dials
// (health checkers and pool drains were measured 10-13s after config removal
// under roll churn) can still *succeed* rather than merely fail cleanly. CNI
// DEL therefore unpins on a delay — but, since #796, it no longer waits on the
// agent's ACK to do so: a DEL that cannot reach the agent unpins on the same
// delay, returns success, and leaves reconciliation to the agent's ghost sweep
// (agent/internal/cni/server/ghostsweep.go). Blocking instead wedged the node,
// which is now the worse failure of the two.

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
	return p.unpinTarget(conf.NetnsPinPath(containerID))
}

// unpinTarget removes the pin at an explicit path (the detached unpinner gets
// the resolved path on its argv rather than re-parsing CNI config).
func (p *AetherPlugin) unpinTarget(target string) error {
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

// delFailSuffix names the marker that records when the DEL for a container
// FIRST failed against a reachable agent. It sits next to that container's pin
// (same dir, same ID) so the two are created, swept and removed together.
const delFailSuffix = ".delfail"

// delFailPath returns the first-failure marker path for a container.
func delFailPath(conf config.AetherConf, containerID string) string {
	return conf.NetnsPinPath(containerID) + delFailSuffix
}

// noteDelFailure records the first time a DEL for containerID failed against a
// reachable agent, and reports whether that first failure is now older than
// conf.NetnsDelGiveUpAfter() — i.e. whether CmdDel should stop handing the
// error back to the runtime.
//
// containerd retries a failed DEL indefinitely, and a pod whose sandbox cannot
// be torn down keeps its CPU request, so an agent that answers but never
// succeeds wedges the node just as thoroughly as an absent one (#796). The
// bound has to live on disk because the plugin process exists for exactly one
// CNI call and has no memory of the previous attempt.
func (p *AetherPlugin) noteDelFailure(conf config.AetherConf, containerID string) bool {
	giveUp := conf.NetnsDelGiveUpAfter()
	if giveUp <= 0 {
		return true
	}
	path := delFailPath(conf, containerID)
	if first, err := readDelFailure(path); err == nil {
		return time.Since(first) >= giveUp
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.logger.Warn("failed to create netns pin dir for the CNI DEL failure marker; the retry loop stays unbounded",
			zap.String("path", path), zap.Error(err))
		return false
	}
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		p.logger.Warn("failed to record the CNI DEL failure marker; the retry loop stays unbounded",
			zap.String("path", path), zap.Error(err))
	}
	return false
}

// clearDelFailure drops the marker once the DEL stops needing it (the agent
// ACKed, or the plugin gave up). A missing marker is not an error.
func (p *AetherPlugin) clearDelFailure(conf config.AetherConf, containerID string) {
	if err := os.Remove(delFailPath(conf, containerID)); err != nil && !os.IsNotExist(err) {
		p.logger.Warn("failed to remove the CNI DEL failure marker",
			zap.String("containerID", containerID), zap.Error(err))
	}
}

// readDelFailure reads a marker's timestamp. Any unreadable or malformed
// marker is reported as an error, so the caller rewrites it with "now" rather
// than giving up on a file it cannot interpret.
func readDelFailure(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
}

// NetnsUnpinSubcommand is the hidden argv[1] under which the CNI binary
// re-executes itself as a short-lived detached unpinner.
const NetnsUnpinSubcommand = "netns-unpin"

// spawnDetachedUnpinFn indirects the detached-unpin spawn so a test can assert
// that a CNI DEL scheduled the unpin (and with which target and delay) without
// forking a process.
var spawnDetachedUnpinFn = (*AetherPlugin).spawnDetachedUnpin

// spawnDetachedUnpin re-executes this binary as a detached (setsid) process
// that sleeps delay and then unpins target. CNI DEL must return promptly (a
// long in-process sleep delays pod teardown node-wide), but the pin must
// outlive Envoy's drain tail — health checkers and pool drains were observed
// dialing 10-13s after config removal under roll churn, and a dial through an
// already-unpinned path fails (cleanly, on the pinned snapshot — it used to be
// a nullptr segfault; see the package comment above).
func (p *AetherPlugin) spawnDetachedUnpin(target string, delay time.Duration) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving self: %w", err)
	}
	cmd := exec.Command(self, NetnsUnpinSubcommand, target, delay.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting detached unpinner: %w", err)
	}
	// Detach: the child outlives this CNI invocation; init reaps it.
	return cmd.Process.Release()
}

// RunDetachedUnpin is the detached-unpinner entrypoint: argv = [target, delay].
func (p *AetherPlugin) RunDetachedUnpin(args []string) {
	if len(args) != 2 {
		p.logger.Error("netns-unpin: expected <target> <delay>", zap.Strings("args", args))
		return
	}
	target := args[0]
	delay, err := time.ParseDuration(args[1])
	if err != nil {
		p.logger.Error("netns-unpin: bad delay", zap.String("delay", args[1]), zap.Error(err))
		return
	}
	time.Sleep(delay)
	if err := p.unpinTarget(target); err != nil {
		p.logger.Warn("netns-unpin: failed; orphan will be swept by GC", zap.String("target", target), zap.Error(err))
		return
	}
	p.logger.Info("netns-unpin: released", zap.String("target", target), zap.Duration("delay", delay))
}

// gcAttachments is the CNI GC payload subset listing still-valid attachments.
type gcAttachments struct {
	ValidAttachments []struct {
		ContainerID string `json:"containerID"`
	} `json:"cni.dev/valid-attachments"`
}

// sweepNetnsPins unpins every entry in the pin dir whose container ID is not in
// the valid set — orphans left by DELs that never completed, or whose detached
// unpinner never ran. Best-effort.
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
		p.sweepPinEntry(conf, dir, e.Name(), valid)
	}
}

// sweepPinEntry handles one directory entry of the pin dir: a DEL-failure
// marker, a pin still backing a valid attachment, or an orphan pin to release.
func (p *AetherPlugin) sweepPinEntry(conf config.AetherConf, dir, name string, valid map[string]struct{}) {
	// A DEL-failure marker belongs to its container's pin: it survives exactly
	// as long as the attachment does, and must never be mistaken for a pin
	// (unpinning it would silently reset the give-up bound of a DEL still being
	// retried).
	if cid, isMarker := strings.CutSuffix(name, delFailSuffix); isMarker {
		if _, ok := valid[cid]; ok {
			return
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			p.logger.Warn("netns pin sweep: failed to remove orphan DEL marker", zap.String("file", name), zap.Error(err))
		}
		return
	}
	if _, ok := valid[name]; ok {
		return
	}
	if err := p.unpinNetns(conf, name); err != nil {
		p.logger.Warn("netns pin sweep: failed to unpin orphan", zap.String("containerID", name), zap.Error(err))
	} else {
		p.logger.Info("netns pin sweep: unpinned orphan", zap.String("containerID", name))
	}
	p.clearDelFailure(conf, name)
}
