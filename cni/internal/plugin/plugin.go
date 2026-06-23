// Package plugin implements the Aether CNI plugin, which is invoked by the container runtime
// (e.g., containerd) during pod lifecycle transitions.
//
// The plugin implements the Container Networking Interface (CNI) specification (v1.0.0)
// and is chained after other CNI plugins. It delegates actual networking setup to the Aether
// agent (running as a DaemonSet) via a gRPC client, enabling transparent traffic interception
// and service mesh integration.
//
// The plugin supports these CNI operations:
//   - Add: Called when a pod is created. Collects pod metadata, resolves the container PID,
//     and sends the pod info to the agent for registration.
//   - Del: Called when a pod is deleted. Sends the pod removal request to the agent.
//   - Check: Called to verify the plugin is functional (currently a no-op).
//   - GC and Status: Garbage collection and status reporting (currently no-ops).
//
// The plugin extracts pod information from CNI arguments and the previous result from
// chained plugins, including pod IPs, network namespace, and Kubernetes metadata
// (pod name, namespace, container ID).
package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/cni/config"
	"github.com/bpalermo/aether/cni/internal/cri"
	"github.com/bpalermo/aether/cni/internal/telemetry"
	"github.com/bpalermo/aether/common/constants"
	commontelemetry "github.com/bpalermo/aether/common/telemetry"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// AetherPlugin implements the CNI plugin interface for Aether service mesh integration.
// It is invoked by the container runtime during pod lifecycle transitions.
type AetherPlugin struct {
	logger *zap.Logger
}

// NewAetherPlugin creates a new AetherPlugin instance with the given logger.
func NewAetherPlugin(logger *zap.Logger) *AetherPlugin {
	return &AetherPlugin{
		logger,
	}
}

// CmdAdd handles the CNI Add operation, called when a pod is created.
// It parses the CNI configuration and Kubernetes arguments, extracts pod networking info,
// resolves the container PID, and sends the pod registration request to the agent.
// Returns the previous plugin's CNI result on success.
func (p *AetherPlugin) CmdAdd(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI add command")

	netConf, prevResult, k8sArgs, err := p.parseAddArgs(args)
	if err != nil {
		return err
	}
	telemetry.Init(context.Background(), p.logger, netConf.OTLPEndpoint)

	if ignorableNamespace(string(k8sArgs.K8S_POD_NAMESPACE)) {
		p.logger.Info("skipping CNI add for system pod",
			zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
			zap.String("pod", string(k8sArgs.K8S_POD_NAME)))
		return types.PrintResult(prevResult, netConf.CNIVersion)
	}

	podIPs := extractPodIPs(prevResult)

	p.logger.Info("processing CNI add request",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)),
		zap.String("container_id", args.ContainerID),
		zap.String("netns", args.Netns),
		zap.String("interface", args.IfName),
		zap.Strings("pod_ips", podIPs),
		zap.String("cni_version", netConf.CNIVersion))

	pidValue := p.resolvePID(args.Netns, netConf.CRISocket, args.ContainerID)
	cniPod := newPodFromArgs(args, k8sArgs, podIPs, pidValue)

	// Pin the netns to an aether-owned path and register that path instead of
	// the runtime's, so Envoy's per-pod dials never race the runtime's netns
	// teardown (see netnspin.go). Pin failure falls back to the runtime path:
	// a working mesh with the old crash window beats a failed pod start.
	if !netConf.NetnsPinDisabled {
		if pinned, err := p.pinNetns(netConf, args.Netns, args.ContainerID); err != nil {
			p.logger.Warn("failed to pin netns; falling back to runtime netns path",
				zap.String("netns", args.Netns), zap.Error(err))
		} else {
			cniPod.NetworkNamespace = pinned
		}
	}

	// Transparent capture (proposal 018, Phase 3a): redirect outbound ClusterIP:18081
	// to the pod-local capture listener. Best-effort — a failure leaves the explicit
	// fast-lane working, so it must not fail the pod's networking. Uses the runtime
	// netns path (the rule lives in the kernel netns, not the bind-mount pin).
	if netConf.TransparentCaptureEnabled {
		if err := installCaptureRedirect(args.Netns, p.logger); err != nil {
			p.logger.Warn("failed to install transparent-capture redirect; continuing without capture",
				zap.String("netns", args.Netns), zap.Error(err))
		}
	}

	return p.sendAddPod(context.Background(), netConf, cniPod, prevResult)
}

// CmdCheck handles the CNI Check operation for plugin health verification.
// It validates the network namespace, interface, and pod registration with the agent.
func (p *AetherPlugin) CmdCheck(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI check command")

	netConf, _, k8sArgs, err := p.parseAddArgs(args)
	if err != nil {
		return err
	}
	telemetry.Init(context.Background(), p.logger, netConf.OTLPEndpoint)

	if ignorableNamespace(string(k8sArgs.K8S_POD_NAMESPACE)) {
		return nil
	}

	// Verify the pod's network namespace still exists.
	if err := verifyNetns(args.Netns); err != nil {
		return fmt.Errorf("network namespace check failed: %w", err)
	}

	// Verify the expected network interface exists inside the namespace.
	if err := verifyInterface(args.Netns, args.IfName); err != nil {
		return fmt.Errorf("interface check failed: %w", err)
	}

	// Verify the pod is still registered with the agent.
	if err := p.verifyPodRegistration(args, k8sArgs, netConf); err != nil {
		return fmt.Errorf("pod registration check failed: %w", err)
	}

	p.logger.Info("CNI check passed",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)),
		zap.String("netns", args.Netns),
		zap.String("interface", args.IfName))

	return nil
}

// CmdDel handles the CNI Del operation, called when a pod is deleted.
// It parses the CNI configuration and Kubernetes arguments, then sends the pod
// removal request to the agent for cleanup.
func (p *AetherPlugin) CmdDel(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI del command")

	conf, k8sArgs, err := p.parseDelArgs(args)
	if err != nil {
		return err
	}
	telemetry.Init(context.Background(), p.logger, conf.OTLPEndpoint)

	namespace := string(k8sArgs.K8S_POD_NAMESPACE)
	if ignorableNamespace(namespace) {
		p.logger.Info("skipping CNI del for system pod",
			zap.String("namespace", namespace),
			zap.String("pod", string(k8sArgs.K8S_POD_NAME)))
		return nil
	}

	podName := string(k8sArgs.K8S_POD_NAME)
	containerID := string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID)

	p.logger.Info("deleting network for pod",
		zap.String("namespace", namespace),
		zap.String("pod", podName))

	if err := p.sendRemovePod(context.Background(), conf, podName, namespace, containerID); err != nil {
		// Keep the netns pin: the agent has not confirmed the pod's xDS
		// resources are gone, and unpinning now reintroduces the deleted-netns
		// Envoy crash. The runtime retries DEL; CmdGC sweeps true orphans.
		return err
	}

	// Confirm the proxy actually closed the pod's listener socket (connection
	// refused) before scheduling the unpin: the agent's removal-ACK wait is
	// best-effort and the unpin delay is a heuristic. Best-effort as well.
	if !conf.ReadinessProbeDisabled {
		if netns := p.delProbeNetns(conf, args); netns != "" {
			probeCtx, cancel := context.WithTimeout(context.Background(), readyProbeDelTimeout)
			if probeErr := newReadinessProber(netns).waitGone(probeCtx); probeErr != nil {
				p.logger.Warn("proxy listener removal not confirmed; continuing",
					zap.String("netns", netns), zap.Error(probeErr))
			}
			cancel()
		}
	}

	if !conf.NetnsPinDisabled {
		// The agent has deregistered the pod and waited for Envoy to ack the
		// listener removal; a detached unpinner holds the pin through Envoy's
		// drain tail (health checkers and pool drains were observed dialing
		// 10-13s after removal under churn) without delaying pod teardown.
		if err := p.spawnDetachedUnpin(conf.NetnsPinPath(args.ContainerID), conf.NetnsUnpinDelay()); err != nil {
			p.logger.Warn("failed to spawn netns unpinner; orphan will be swept by GC",
				zap.String("containerID", args.ContainerID), zap.Error(err))
		}
	}
	return nil
}

// CmdGC handles the CNI GC (garbage collection) operation: it unpins netns
// pins whose container is no longer a valid attachment (orphans of failed
// DELs). Registry/xDS garbage collection is handled by the agent.
func (p *AetherPlugin) CmdGC(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI GC command")
	conf, err := config.NewConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}
	if !conf.NetnsPinDisabled {
		p.sweepNetnsPins(conf, args.StdinData)
	}
	return nil
}

// CmdStatus handles the CNI Status operation for plugin status reporting.
// It checks agent gRPC endpoint reachability.
func (p *AetherPlugin) CmdStatus(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI status command")

	conf, err := config.NewConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// Check that the agent gRPC endpoint is reachable.
	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("plugin not ready: failed to create agent client: %w", err)
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			p.logger.Warn("failed to close CNI client", zap.Error(cerr))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	if err := client.CheckAgentConnection(ctx); err != nil {
		return fmt.Errorf("plugin not ready: agent is unreachable: %w", err)
	}

	p.logger.Info("CNI status check passed")
	return nil
}

// parseAddArgs parses and validates CmdAdd inputs: config, previous result, and K8s args.
func (p *AetherPlugin) parseAddArgs(args *skel.CmdArgs) (config.AetherConf, *current.Result, config.K8sArgs, error) {
	netConf, err := config.NewConf(args.StdinData)
	if err != nil {
		return netConf, nil, config.K8sArgs{}, err
	}

	if netConf.PrevResult == nil {
		return netConf, nil, config.K8sArgs{}, fmt.Errorf("must be called as chained plugin")
	}

	prevResult, err := current.GetResult(netConf.PrevResult)
	if err != nil {
		return netConf, nil, config.K8sArgs{}, fmt.Errorf("failed to parse previous result: %w", err)
	}

	var k8sArgs config.K8sArgs
	if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
		return netConf, nil, config.K8sArgs{}, fmt.Errorf("failed to load args: %w", err)
	}

	return netConf, prevResult, k8sArgs, nil
}

// parseDelArgs parses CmdDel inputs: config and K8s args.
func (p *AetherPlugin) parseDelArgs(args *skel.CmdArgs) (config.AetherConf, config.K8sArgs, error) {
	conf, err := config.NewConf(args.StdinData)
	if err != nil {
		return conf, config.K8sArgs{}, fmt.Errorf("failed to parse config: %w", err)
	}

	var k8sArgs config.K8sArgs
	if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
		return conf, config.K8sArgs{}, fmt.Errorf("failed to load args during delete: %w", err)
	}

	return conf, k8sArgs, nil
}

// extractPodIPs collects non-nil IP addresses from a CNI result.
func extractPodIPs(result *current.Result) []string {
	var ips []string
	for _, ipConfig := range result.IPs {
		if ipConfig.Address.IP != nil {
			ips = append(ips, ipConfig.Address.IP.String())
		}
	}
	return ips
}

// resolvePID attempts to determine the container PID. It first parses the
// netns path, then falls back to a CRI query. Returns nil if both fail.
func (p *AetherPlugin) resolvePID(netns, criSocket, containerID string) *wrapperspb.UInt32Value {
	if pid, err := pidFromNetns(netns); err == nil {
		return wrapperspb.UInt32(pid)
	}

	p.logger.Debug("could not extract PID from netns, falling back to CRI")
	pid, err := cri.GetContainerPID(context.Background(), criSocket, containerID)
	if err != nil {
		p.logger.Warn("failed to get container PID, continuing without it", zap.Error(err))
		return nil
	}
	return wrapperspb.UInt32(pid)
}

// sendAddPod sends the pod to the agent and returns the previous result on success.
func (p *AetherPlugin) sendAddPod(ctx context.Context, conf config.AetherConf, pod *cniv1.CNIPod, prevResult *current.Result) (retErr error) {
	ctx, span := startPodSpan(ctx, "cni.add_pod", pod.GetName(), pod.GetNamespace(), pod.GetContainerId())
	defer func() { commontelemetry.EndSpan(span, retErr) }()

	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("failed to create CNI client: %w", err)
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			p.logger.Warn("failed to close CNI client", zap.Error(cerr))
		}
	}()

	res, err := client.AddPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("failed to add pod to agent: %w", err)
	}

	if res.Result != cniv1.AddPodResponse_RESULT_SUCCESS {
		return fmt.Errorf("adding pod to agent was not successful: %v", res.Result)
	}

	// Data-plane proof, before pod start completes: probe the outbound capture
	// listener from inside the pod's netns until the proxy's health_check
	// filter answers 200. The agent's ACK wait above only confirmed config
	// acceptance. Best-effort: a timeout is logged, never fails the ADD.
	if !conf.ReadinessProbeDisabled {
		probeCtx, probeSpan := startPodSpan(ctx, "cni.readiness_probe", pod.GetName(), pod.GetNamespace(), pod.GetContainerId())
		probeCtx, cancel := context.WithTimeout(probeCtx, readyProbeAddTimeout)
		probeErr := newReadinessProber(pod.GetNetworkNamespace()).waitServing(probeCtx)
		cancel()
		commontelemetry.EndSpan(probeSpan, probeErr)
		if probeErr != nil {
			p.logger.Warn("data plane not confirmed serving; continuing",
				zap.String("netns", pod.GetNetworkNamespace()), zap.Error(probeErr))
		}
	}

	return types.PrintResult(prevResult, conf.CNIVersion)
}

// delProbeNetns resolves the netns path to probe during DEL: the pinned path
// when pinning is enabled and the pin exists (the runtime's path may already
// be torn down), otherwise the runtime-provided path. Empty = nothing to probe.
func (p *AetherPlugin) delProbeNetns(conf config.AetherConf, args *skel.CmdArgs) string {
	if !conf.NetnsPinDisabled {
		if pinned := conf.NetnsPinPath(args.ContainerID); fileExists(pinned) {
			return pinned
		}
	}
	return args.Netns
}

// sendRemovePod sends the pod removal request to the agent.
func (p *AetherPlugin) sendRemovePod(ctx context.Context, conf config.AetherConf, podName, namespace, containerID string) (retErr error) {
	ctx, span := startPodSpan(ctx, "cni.remove_pod", podName, namespace, containerID)
	defer func() { commontelemetry.EndSpan(span, retErr) }()

	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("failed to create CNI client: %w", err)
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			p.logger.Warn("failed to close CNI client", zap.Error(cerr))
		}
	}()

	res, err := client.RemovePod(ctx, podName, namespace, containerID)
	if err != nil {
		return fmt.Errorf("failed to remove pod from agent: %w", err)
	}

	if res.Result != cniv1.RemovePodResponse_RESULT_SUCCESS {
		return fmt.Errorf("removing pod from agent was not successful: %v", res.Result)
	}

	return nil
}

func newPodFromArgs(args *skel.CmdArgs, k8sArgs config.K8sArgs, podIPs []string, pid *wrapperspb.UInt32Value) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		ContainerId:      args.ContainerID,
		Name:             string(k8sArgs.K8S_POD_NAME),
		Namespace:        string(k8sArgs.K8S_POD_NAMESPACE),
		NetworkNamespace: args.Netns,
		Ips:              podIPs,
		Pid:              pid,
	}
}

// pidFromNetns extracts the PID associated with a network namespace path.
// It first tries to parse the PID directly from a /proc/<pid>/ns/net path,
// then falls back to matching the file's inode against /proc/*/ns/net entries.
func pidFromNetns(netns string) (uint32, error) {
	if pid, err := pidFromNetnsPath(netns); err == nil {
		return pid, nil
	}
	return pidFromNetnsInode(netns)
}

// pidFromNetnsPath extracts the PID from a /proc/<pid>/ns/net path string.
func pidFromNetnsPath(netns string) (uint32, error) {
	const prefix = "/proc/"
	const suffix = "/ns/net"

	if !strings.HasPrefix(netns, prefix) || !strings.HasSuffix(netns, suffix) {
		return 0, fmt.Errorf("netns path %q does not match /proc/<pid>/ns/net format", netns)
	}

	pidStr := netns[len(prefix) : len(netns)-len(suffix)]
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse PID from netns path %q: %w", netns, err)
	}

	return uint32(pid), nil
}

// pidFromNetnsInode finds the PID that owns the given network namespace by
// comparing file inodes. It stats the target netns file, then scans
// /proc/[0-9]*/ns/net for a matching device and inode number.
func pidFromNetnsInode(netns string) (uint32, error) {
	targetStat, err := os.Stat(netns)
	if err != nil {
		return 0, fmt.Errorf("stat netns %q: %w", netns, err)
	}

	targetSys, ok := targetStat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("failed to get syscall.Stat_t for %q", netns)
	}

	matches, err := filepath.Glob("/proc/[0-9]*/ns/net")
	if err != nil {
		return 0, fmt.Errorf("glob /proc/*/ns/net: %w", err)
	}

	for _, candidate := range matches {
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		candidateSys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if candidateSys.Dev == targetSys.Dev && candidateSys.Ino == targetSys.Ino {
			pid, parseErr := pidFromNetnsPath(candidate)
			if parseErr != nil {
				continue
			}
			return pid, nil
		}
	}

	return 0, fmt.Errorf("no process found with netns inode matching %q", netns)
}

// verifyNetns checks that the network namespace path exists and is accessible.
func verifyNetns(netnsPath string) error {
	if netnsPath == "" {
		return fmt.Errorf("network namespace path is empty")
	}

	_, err := os.Stat(netnsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("network namespace %q no longer exists", netnsPath)
		}
		return fmt.Errorf("cannot access network namespace %q: %w", netnsPath, err)
	}

	return nil
}

// verifyInterface checks that the expected network interface exists in the
// network namespace. It reads the interface list from the sysfs path derived
// from the netns. For /proc-based namespaces, it checks /proc/<pid>/net/dev.
func verifyInterface(netnsPath, ifName string) error {
	if ifName == "" {
		return fmt.Errorf("interface name is empty")
	}

	// For /proc/<pid>/ns/net paths, check /proc/<pid>/net/dev for the interface.
	pid, err := pidFromNetnsPath(netnsPath)
	if err != nil {
		// If we can't extract a PID, we skip the interface check rather
		// than failing — the netns check already confirmed the namespace exists.
		return nil
	}

	devPath := fmt.Sprintf("/proc/%d/net/dev", pid)
	data, err := os.ReadFile(devPath)
	if err != nil {
		// If we can't read the dev file, skip the check gracefully.
		return nil
	}

	// /proc/<pid>/net/dev has a header (2 lines) then "iface: ..." lines.
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.TrimSpace(line)
		if colonIdx := strings.Index(fields, ":"); colonIdx > 0 {
			name := strings.TrimSpace(fields[:colonIdx])
			if name == ifName {
				return nil
			}
		}
	}

	return fmt.Errorf("interface %q not found in network namespace (pid %d)", ifName, pid)
}

// verifyPodRegistration checks that the pod is still registered with the agent
// by making a gRPC call.
func (p *AetherPlugin) verifyPodRegistration(args *skel.CmdArgs, k8sArgs config.K8sArgs, conf config.AetherConf) error {
	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("failed to create CNI client: %w", err)
	}
	defer func() {
		if cerr := client.Close(); cerr != nil {
			p.logger.Warn("failed to close CNI client", zap.Error(cerr))
		}
	}()

	// The verification re-sends AddPod (idempotent by overwrite), so it must
	// carry the same netns path ADD registered — the pinned one when present —
	// or the check would silently replace the pinned path with the runtime's.
	netns := args.Netns
	if !conf.NetnsPinDisabled {
		if pinned := conf.NetnsPinPath(args.ContainerID); fileExists(pinned) {
			netns = pinned
		}
	}

	pod := &cniv1.CNIPod{
		ContainerId:      args.ContainerID,
		Name:             string(k8sArgs.K8S_POD_NAME),
		Namespace:        string(k8sArgs.K8S_POD_NAMESPACE),
		NetworkNamespace: netns,
	}

	return client.VerifyPodRegistered(context.Background(), pod)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func ignorableNamespace(namespace string) bool {
	return constants.IsIgnoredNamespace(namespace)
}
