package install

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// Installer copies the CNI plugin binaries onto the host and chains the aether
// entry into the node's CNI conflist.
//
// Every line the installer emits goes through its own logger: the package used
// to log part of the install through controller-runtime's global logger, which
// nothing in cni/ ever binds, so those records were discarded (issue #696).
type Installer struct {
	cfg    *InstallerConfig
	logger *slog.Logger
}

// NewInstaller returns an instance of Installer with the given config
func NewInstaller(logger *slog.Logger, cfg *InstallerConfig) *Installer {
	return &Installer{
		cfg,
		logger,
	}
}

func (in *Installer) Run(ctx context.Context) error {
	in.logger.InfoContext(ctx, "running CNI installer")
	installedBins, err := in.installAll(ctx)
	if err != nil {
		return err
	}

	in.logger.InfoContext(ctx, "CNI binaries installed", "binaries", installedBins)
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

	// No kubeconfig is needed: the Aether CNI plugin delegates Kubernetes API
	// access to the node agent via gRPC over Unix domain socket.

	_, err = createCNIConfigFile(ctx, in.logger, in.cfg)
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
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
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
			in.logger.Error("failed to close source file", "error", err)
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
			in.logger.Error("failed to cleanup temp file", "error", err)
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
