package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"aethermesh.dev/common/file"
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

		// Copy the file atomically (//common/file)
		if err := in.copyFileAtomic(srcPath, targetPath); err != nil {
			return nil, fmt.Errorf("failed to copy %s to %s: %w", srcPath, targetPath, err)
		}

		copiedFiles = append(copiedFiles, targetPath)
	}

	return copiedFiles, nil
}

// binOwner is the uid:gid every installed binary is given (root). A variable so
// the unprivileged unit tests can install as themselves.
var binOwner = struct{ uid, gid int }{0, 0}

// copyFileAtomic copies a file from src to dst using atomic writes. A dst that
// already holds exactly src's bytes, mode and owner is left alone (issue
// #1123): every agent pod start runs this init container, and on a restart the
// plugin on the host is almost always the one being installed, so the copy, the
// fsync and the directory sync are pure delay on the path to the new agent
// serving its node's proxy.
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

	if installedIdentical(srcFile, srcInfo, dst) {
		in.logger.Info("CNI binary already installed and unchanged; not rewriting it", "filepath", dst)
		return nil
	}
	if _, err := srcFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to rewind source file: %w", err)
	}

	// One writer for every file this installer puts on the host (the conflist
	// goes through the same package): a temp file in dst's own directory, given
	// src's mode and the install owner, written, fsynced, renamed over dst, and
	// the directory fsynced after the rename. The directory fsync is what makes
	// the new entry durable: without it a power cut can leave the host CNI bin
	// dir without the plugin it was just told it had (#645, issue #772).
	//
	// KeepInPageCache: the container runtime execs this file on the next pod
	// ADD, so its pages are wanted, not evicted.
	if err := file.AtomicWriteReader(dst, srcFile, srcInfo.Mode(),
		file.WithOwner(binOwner.uid, binOwner.gid),
		file.KeepInPageCache(),
	); err != nil {
		return fmt.Errorf("failed to atomically replace file: %w", err)
	}

	return nil
}

// installedIdentical reports whether dst is a regular file with src's size,
// permission bits, the install owner and byte-for-byte src's content. Any doubt
// (a stat or read error, a non-regular file) answers false, so the caller
// falls back to the atomic copy it always made.
func installedIdentical(src *os.File, srcInfo os.FileInfo, dst string) bool {
	dstInfo, err := os.Stat(dst)
	if err != nil || !sameInstallMetadata(srcInfo, dstInfo) {
		return false
	}
	dstFile, err := os.Open(dst)
	if err != nil {
		return false
	}
	defer func() { _ = dstFile.Close() }()
	return sameContent(src, dstFile)
}

// sameInstallMetadata reports whether dst is a regular file with src's size and
// mode, owned by the install owner.
func sameInstallMetadata(srcInfo, dstInfo os.FileInfo) bool {
	if !dstInfo.Mode().IsRegular() || dstInfo.Size() != srcInfo.Size() || dstInfo.Mode() != srcInfo.Mode() {
		return false
	}
	st, ok := dstInfo.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == binOwner.uid && int(st.Gid) == binOwner.gid
}

// sameContent compares two readers byte for byte, a chunk at a time.
func sameContent(a, b io.Reader) bool {
	const chunk = 1 << 20
	bufA, bufB := make([]byte, chunk), make([]byte, chunk)
	for {
		na, errA := io.ReadFull(a, bufA)
		nb, errB := io.ReadFull(b, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false
		}
		if errA != nil || errB != nil {
			return isEOF(errA) && isEOF(errB)
		}
	}
}

// isEOF reports whether a ReadFull error means the reader simply ended.
func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
