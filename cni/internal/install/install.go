package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/google/renameio/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

var installLog = ctrl.Log.WithName("installer")

type Installer struct {
	cfg    *InstallerConfig
	logger logr.Logger
}

func init() {

}

// NewInstaller returns an instance of Installer with the given config
func NewInstaller(logger logr.Logger, cfg *InstallerConfig) *Installer {
	return &Installer{
		cfg,
		logger,
	}
}

func (in *Installer) Run(ctx context.Context) error {
	in.logger.Info("running CNI installer")
	installedBins, err := in.installAll(ctx)
	if err != nil {
		return err
	}

	in.logger.Info("CNI binaries installed", "binaries", installedBins)
	return nil
}

func (in *Installer) installAll(ctx context.Context) ([]string, error) {
	// Install binaries
	// Currently we _always_ do this, since the binaries do not live in a shared location
	// and there's no harm in doing do.
	copiedFiles, err := in.copyBinaries(in.cfg.CNIBinSourceDir, in.cfg.CNIBinTargetDir)
	if err != nil {
		return nil, err
	}

	// TODO: write kubeconfig file

	_, err = createCNIConfigFile(ctx, in.cfg)
	if err != nil {
		return copiedFiles, fmt.Errorf("create CNI config file: %v", err)
	}

	return copiedFiles, nil
}

// copyBinaries copies/mirrors any files present in a single source dir to N number of target dirs
// and returns a set of the filenames copied.
func (in *Installer) copyBinaries(srcDir string, targetDir string) ([]string, error) {
	// Read all files from the source the directory
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read source directory: %w", err)
	}

	var copiedFiles []string

	for _, entry := range entries {
		// Skip directories
		if entry.IsDir() {
			continue
		}

		srcPath := filepath.Join(srcDir, entry.Name())

		// Ensure target directory exists
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create target directory %s: %w", targetDir, err)
		}

		targetPath := filepath.Join(targetDir, entry.Name())

		// Copy file using renameio for atomic writes
		if err := in.copyFileAtomic(srcPath, targetPath); err != nil {
			return nil, fmt.Errorf("failed to copy %s to %s: %w", srcPath, targetPath, err)
		}

		copiedFiles = append(copiedFiles, targetPath)
	}

	return copiedFiles, nil
}

// copyFileAtomic copies a file from src to dst using atomic writes
func (in *Installer) copyFileAtomic(src, dst string) error {
	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer func(srcFile *os.File) {
		err := srcFile.Close()
		if err != nil {
			in.logger.Error(err, "failed to close source file")
		}
	}(srcFile)

	// Get source file info for permissions
	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat source file: %w", err)
	}

	// Create a temporary file with renameio
	t, err := renameio.TempFile("", dst)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func(t *renameio.PendingFile) {
		err := t.Cleanup()
		if err != nil {
			in.logger.Error(err, "failed to cleanup temp file")
		}
	}(t)

	// Copy content
	if _, err := io.Copy(t, srcFile); err != nil {
		return fmt.Errorf("failed to copy content: %w", err)
	}

	// Set permissions
	if err := t.Chmod(srcInfo.Mode()); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Set ownership to root (UID 0, GID 0)
	if err := t.Chown(0, 0); err != nil {
		return fmt.Errorf("failed to set ownership to root: %w", err)
	}

	// Atomic rename to final destination
	if err := t.CloseAtomicallyReplace(); err != nil {
		return fmt.Errorf("failed to atomically replace file: %w", err)
	}

	return nil
}

// checkValidCNIConfig returns an error if an invalid CNI configuration is detected
// - CNIConfName is the name of the primary CNI config file which may or may not contain the Istio CNI config
// depending on whether Istio owned CNI config is enabled
// - cniConfigFilepath is the path to the CNI config file that is currently being used. This may be different
// from the primary CNI config file if using an Istio owned CNI config is enabled. The value is unset on the
// first call of checkValidCNIConfig
func checkValidCNIConfig(ctx context.Context, cfg *InstallerConfig, cniConfigFilepath string) error {

	return nil
}
