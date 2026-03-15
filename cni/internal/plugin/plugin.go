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
	"github.com/bpalermo/aether/cni/internal/cri"
	"github.com/bpalermo/aether/cni/pkg/config"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type AetherPlugin struct {
	logger *zap.Logger
}

func NewAetherPlugin(logger *zap.Logger) *AetherPlugin {
	return &AetherPlugin{
		logger,
	}
}

func (p *AetherPlugin) CmdAdd(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI add command")

	netConf, prevResult, k8sArgs, err := p.parseAddArgs(args)
	if err != nil {
		return err
	}

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

	return p.sendAddPod(context.Background(), netConf, cniPod, prevResult)
}

func (p *AetherPlugin) CmdCheck(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI check command")
	return nil
}

func (p *AetherPlugin) CmdDel(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI del command")

	conf, k8sArgs, err := p.parseDelArgs(args)
	if err != nil {
		return err
	}

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

	return p.sendRemovePod(context.Background(), conf, podName, namespace, containerID)
}

func (p *AetherPlugin) CmdGC(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI GC command")
	return nil
}

func (p *AetherPlugin) CmdStatus(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI status command")
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
func (p *AetherPlugin) sendAddPod(ctx context.Context, conf config.AetherConf, pod *cniv1.CNIPod, prevResult *current.Result) error {
	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("failed to create CNI client: %w", err)
	}
	defer client.Close() //nolint:errcheck

	res, err := client.AddPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("failed to add pod to agent: %w", err)
	}

	if res.Result != cniv1.AddPodResponse_SUCCESS {
		return fmt.Errorf("adding pod to agent was not successful: %v", res.Result)
	}

	return types.PrintResult(prevResult, conf.CNIVersion)
}

// sendRemovePod sends the pod removal request to the agent.
func (p *AetherPlugin) sendRemovePod(ctx context.Context, conf config.AetherConf, podName, namespace, containerID string) error {
	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		return fmt.Errorf("failed to create CNI client: %w", err)
	}
	defer client.Close() //nolint:errcheck

	res, err := client.RemovePod(ctx, podName, namespace, containerID)
	if err != nil {
		return fmt.Errorf("failed to remove pod from agent: %w", err)
	}

	if res.Result != cniv1.RemovePodResponse_SUCCESS {
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

func ignorableNamespace(namespace string) bool {
	return strings.EqualFold(namespace, "kube-system") || strings.EqualFold(namespace, "aether-system")
}
