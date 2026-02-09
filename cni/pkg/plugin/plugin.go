package plugin

import (
	"context"
	"fmt"
	"strings"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"go.uber.org/zap"
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
	netConf, err := NewConf(args.StdinData)
	if err != nil {
		return err
	}

	if netConf.PrevResult == nil {
		msg := "must be called as chained plugin"
		p.logger.Error(msg)
		return fmt.Errorf(msg)
	}

	// Parse the previous result properly
	prevResult, err := current.GetResult(netConf.PrevResult)
	if err != nil {
		p.logger.Error("failed to parse previous result", zap.Error(err))
		return fmt.Errorf("failed to parse previous result: %v", err)
	}

	k8sArgs := K8sArgs{}
	if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
		msg := "failed to load args"
		p.logger.Error(msg, zap.Error(err))
		return fmt.Errorf("%s: %v", msg, err)
	}

	namespace := string(k8sArgs.K8S_POD_NAMESPACE)
	if ignorableNamespace(namespace) {
		p.logger.Info("skipping CNI add for system pod",
			zap.String("namespace", namespace),
			zap.String("pod", string(k8sArgs.K8S_POD_NAME)))
		return types.PrintResult(prevResult, netConf.CNIVersion)
	}

	// Extract the allocated IP addresses
	var podIPs []string
	for _, ipConfig := range prevResult.IPs {
		if ipConfig.Address.IP != nil {
			podIPs = append(podIPs, ipConfig.Address.IP.String())
		}
	}

	p.logger.Info("processing CNI add request",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)),
		zap.String("container_id", args.ContainerID),
		zap.String("netns", args.Netns),
		zap.String("interface", args.IfName),
		zap.Strings("pod_ips", podIPs),
		zap.String("cni_version", netConf.CNIVersion))

	cniPod := newPodFromArgs(args, k8sArgs, podIPs)

	client, err := NewCNIClient(p.logger, netConf.AgentCNIPath)
	if err != nil {
		p.logger.Error("failed to create CNI client", zap.Error(err))
		return err
	}
	defer func(client *CNIClient) {
		if err = client.Close(); err != nil {
			p.logger.Error("failed to close CNI client", zap.Error(err))
		}
	}(client)

	if err != nil {
		return err
	}

	res, err := client.AddPod(context.Background(), cniPod)
	if err != nil {
		p.logger.Error("failed to add pod to agent", zap.Error(err))
		return fmt.Errorf("failed to add pod to agent: %v", err)
	}

	if res.Result != cniv1.AddPodResponse_SUCCESS {
		p.logger.Warn("adding pod from agent was not successful", zap.String("result", res.Result.String()))
		return fmt.Errorf("adding pod to agent was not successful: %v", res.Result)
	}

	// Return the previous result to maintain the chain
	return types.PrintResult(prevResult, netConf.CNIVersion)
}

func (p *AetherPlugin) CmdCheck(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI check command")
	return nil
}

func (p *AetherPlugin) CmdDel(args *skel.CmdArgs) error {
	p.logger.Debug("running CNI del command")
	conf, err := NewConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	// For chained plugins, we typically don't need to do cleanup
	// as the main plugin handles network teardown
	// Just log the deletion for debugging
	k8sArgs := K8sArgs{}
	if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
		p.logger.Error("failed to load args during delete", zap.Error(err))
		return fmt.Errorf("failed to load args during delete: %v", err)
	}

	namespace := string(k8sArgs.K8S_POD_NAMESPACE)
	if ignorableNamespace(namespace) {
		p.logger.Info("skipping CNI add for system pod",
			zap.String("namespace", namespace),
			zap.String("pod", string(k8sArgs.K8S_POD_NAME)))
		return nil
	}

	p.logger.Info("deleting network for pod",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)))

	client, err := NewCNIClient(p.logger, conf.AgentCNIPath)
	if err != nil {
		p.logger.Error("failed to create CNI client", zap.Error(err))
		return err
	}
	defer func(client *CNIClient) {
		if err = client.Close(); err != nil {
			p.logger.Error("failed to close CNI client", zap.Error(err))
		}
	}(client)

	cniPod := newPodFromArgs(args, k8sArgs, nil)

	res, err := client.RemovePod(context.Background(), cniPod)
	if err != nil {
		p.logger.Error("failed to remove pod from agent", zap.Error(err))
		return fmt.Errorf("failed to remove pod from agent: %v", err)
	}

	if res.Result != cniv1.RemovePodResponse_SUCCESS {
		p.logger.Warn("removing pod from agent was not successful", zap.String("result", res.Result.String()))
		return fmt.Errorf("adding pod to agent was not successful: %v", res.Result)
	}

	return nil
}

func (p *AetherPlugin) CmdGC(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI GC command")
	return nil
}

func (p *AetherPlugin) CmdStatus(_ *skel.CmdArgs) error {
	p.logger.Debug("running CNI status command")
	return nil
}

func newPodFromArgs(args *skel.CmdArgs, k8sArgs K8sArgs, podIPs []string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		ContainerId:      args.ContainerID,
		Name:             string(k8sArgs.K8S_POD_NAME),
		Namespace:        string(k8sArgs.K8S_POD_NAMESPACE),
		NetworkNamespace: args.Netns,
		Ips:              podIPs,
	}
}

func ignorableNamespace(namespace string) bool {
	return strings.EqualFold(namespace, "kube-system") || strings.EqualFold(namespace, "aether-system")
}
