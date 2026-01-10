package plugin

import (
	"fmt"
	"os"
	"path/filepath"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/google/renameio/v2"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
)

func (p *AetherPlugin) createRegistryEntry(netns string, args K8sArgs, conf AetherConf, ips []string) error {
	if err := p.checkRegistryPathOrCreate(conf.RegistryPath); err != nil {
		return fmt.Errorf("failed to ensure registry directory: %v", err)
	}

	if err := p.writeRegistryEntryFile(netns, args, conf, ips); err != nil {
		return fmt.Errorf("failed to write registry entry file: %w", err)
	}

	return nil
}

func (p *AetherPlugin) checkRegistryPathOrCreate(registryPath string) error {
	// Check if the directory exists
	if _, err := os.Stat(registryPath); os.IsNotExist(err) {
		// Create the directory with all parent directories
		if err := os.MkdirAll(registryPath, 0755); err != nil {
			p.logger.Error("failed to create registry directory",
				zap.String("path", registryPath),
				zap.Error(err))
			return err
		}
		p.logger.Info("created registry directory", zap.String("path", registryPath))
	} else if err != nil {
		// Some other error occurred while checking
		p.logger.Error("failed to check registry directory",
			zap.String("path", registryPath),
			zap.Error(err))
		return err
	}

	p.logger.Debug("registry directory ready", zap.String("path", registryPath))
	return nil
}

// writeRegistryEntryFile writes a registry entry file to the specified destination.
// Returns an error if the operation fails.
func (p *AetherPlugin) writeRegistryEntryFile(netns string, args K8sArgs, conf AetherConf, ips []string) error {
	var annotations map[string]string
	if conf.RuntimeConfig != nil && conf.RuntimeConfig.PodAnnotations != nil {
		annotations = *conf.RuntimeConfig.PodAnnotations
	}

	entry := &registryv1.RegistryEntry{
		PodName:     string(args.K8S_POD_NAME),
		PodNs:       string(args.K8S_POD_NAMESPACE),
		NetworkNs:   netns,
		ContainerId: string(args.K8S_POD_INFRA_CONTAINER_ID),
		Ips:         ips,
		Annotations: annotations,
	}

	// Marshal to JSON
	data, err := protojson.Marshal(entry)
	if err != nil {
		p.logger.Error("failed to marshal registry entry",
			zap.String("pod", string(args.K8S_POD_NAME)),
			zap.Error(err))
		return fmt.Errorf("failed to marshal registry entry: %w", err)
	}

	// Create filename using pod name
	filename := getEntryFileName(conf.RegistryPath, args)

	// Write the file with appropriate permissions
	if err := renameio.WriteFile(filename, data, 0644); err != nil {
		p.logger.Error("failed to write registry file",
			zap.String("file", filename),
			zap.Error(err))
		return fmt.Errorf("failed to write registry file: %w", err)
	}

	p.logger.Info("registry entry created",
		zap.String("file", filename),
		zap.String("pod", string(args.K8S_POD_NAME)))

	return nil
}

func (p *AetherPlugin) removeRegistryEntry(args K8sArgs, conf AetherConf) error {
	filename := getEntryFileName(conf.RegistryPath, args)

	// Check if the file exists
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		p.logger.Warn("registry entry not found",
			zap.String("file", filename),
			zap.String("pod", string(args.K8S_POD_NAME)))
		return nil // Not an error if the file doesn't exist
	}

	// Remove the file
	if err := os.Remove(filename); err != nil {
		p.logger.Error("failed to remove registry entry",
			zap.String("file", filename),
			zap.String("pod", string(args.K8S_POD_NAME)),
			zap.Error(err))
		return fmt.Errorf("failed to remove registry entry: %w", err)
	}

	p.logger.Info("registry entry removed",
		zap.String("file", filename),
		zap.String("pod", string(args.K8S_POD_NAME)))

	return nil
}

func getEntryFileName(dst string, k8sArgs K8sArgs) string {
	return filepath.Join(dst, fmt.Sprintf("%s.json", string(k8sArgs.K8S_POD_INFRA_CONTAINER_ID)))
}
