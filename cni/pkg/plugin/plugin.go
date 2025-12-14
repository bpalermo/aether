package plugin

import (
	"fmt"

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
	p.logger.Info("processing CNI add request",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)),
		zap.String("container_id", args.ContainerID),
		zap.String("netns", args.Netns),
		zap.String("interface", args.IfName),
		zap.String("cni_version", netConf.CNIVersion))

	err = p.createRegistryEntry(args.Netns, k8sArgs, netConf)
	if err != nil {
		return fmt.Errorf("failed to ensure registry directory: %v", err)
	}

	// Return the previous result to maintain the chain
	return types.PrintResult(prevResult, netConf.CNIVersion)
}

func (p *AetherPlugin) CmdCheck(_ *skel.CmdArgs) error {
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

	p.logger.Info("deleting network for pod",
		zap.String("namespace", string(k8sArgs.K8S_POD_NAMESPACE)),
		zap.String("pod", string(k8sArgs.K8S_POD_NAME)))

	if err = p.removeRegistryEntry(k8sArgs, conf); err != nil {
		return err
	}

	return nil
}

func (p *AetherPlugin) CmdGC(_ *skel.CmdArgs) error {
	return nil
}

func (p *AetherPlugin) CmdStatus(_ *skel.CmdArgs) error {
	return nil
}
