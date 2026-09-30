// Package udscsi is the csi.aether.io node plugin (proposal 039 Phase 1): the
// CSI driver that gives each UDS-served workload a mesh-owned socket directory.
//
// For every pod that declares an inline `csi: {driver: csi.aether.io}` volume
// the kubelet calls NodePublishVolume, and the plugin:
//
//  1. mounts a FRESH tmpfs at <root>/<pod-uid> (default /run/aether/uds/<uid>)
//     with nosuid,nodev,noexec,nosymfollow, a size cap, and
//     `mode=2770,uid=0,gid=<fsGroup>` — so only the pod's fsGroup (and root, i.e.
//     the node proxy) can reach it, and files created in it inherit that group;
//  2. bind-mounts it onto the kubelet's target_path, which is what the pod's
//     containers see.
//
// The per-pod directory is a pure function of the pod UID, which the kubelet
// supplies (podInfoOnMount). Nothing the workload controls — not the volume
// name, not a volume attribute — ever reaches a host path. The plugin is
// stateless: everything it needs to undo a publish is in the unpublish request's
// target_path and in the host's mount table.
//
// Identity + Node services only, ephemeral inline volumes only; there is no
// Controller service and no staging.
package udscsi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// DriverName is the CSI driver name: the CSIDriver object's name, the
	// `driver:` a pod's inline volume names, and what the plugin registers with
	// the kubelet as.
	DriverName = "csi.aether.io"
	// DefaultRoot is where the per-pod tmpfs mounts live on the host. /run is
	// tmpfs on every supported distro, so a reboot starts it empty and the
	// kubelet's republish rebuilds it.
	DefaultRoot = "/run/aether/uds"
	// DefaultKubeletRoot is the kubelet's --root-dir default (Talos too).
	DefaultKubeletRoot = "/var/lib/kubelet"

	// The volume_context keys the kubelet adds for a CSIDriver with
	// podInfoOnMount: true.
	ctxPrefix    = "csi.storage.k8s.io/"
	ctxPodUID    = ctxPrefix + "pod.uid"
	ctxEphemeral = ctxPrefix + "ephemeral"

	// tmpfsMode is the mode of every per-pod tmpfs root: rwx for root (the node
	// proxy) and the pod's fsGroup, nothing for anyone else, and setgid so a
	// socket the app binds inherits the fsGroup.
	tmpfsMode = "2770"
	// fsGroupHint is the fix for the most likely FailedMount this driver emits.
	fsGroupHint = "set pod.spec.securityContext.fsGroup"
)

// podUIDPattern accepts a Kubernetes pod UID: a lower-case RFC 4122 UUID for an
// API pod, or the 32-hex-digit config hash a static pod carries. Anything else
// — in particular anything with a '/' or a '.' — never becomes a path segment.
var podUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$`)

// Config is the node plugin's static configuration.
type Config struct {
	// NodeID is reported by NodeGetInfo: the node's name.
	NodeID string
	// KubeletRoot is the kubelet's --root-dir. Every target_path must lie under
	// <KubeletRoot>/pods/<pod-uid>/.
	KubeletRoot string
	// Root is the host directory holding the per-pod tmpfs mounts.
	Root string
	// SizeBytes caps each per-pod tmpfs.
	SizeBytes int64
	// Version is reported by GetPluginInfo.
	Version string
}

// Driver implements the CSI Identity and Node services.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	cfg     Config
	mounter Mounter
	log     *slog.Logger

	// mu serialises publish/unpublish. Every operation is a handful of
	// syscalls, and the kubelet retries on its own backoff, so there is no
	// throughput to win by per-UID locking — and one lock makes the
	// check-then-mount sequences trivially race-free.
	mu sync.Mutex
}

// NewDriver validates cfg and returns a Driver that mutates the host through m.
func NewDriver(cfg Config, m Mounter, log *slog.Logger) (*Driver, error) {
	switch {
	case cfg.NodeID == "":
		return nil, errors.New("node ID is required (set NODE_NAME or --node-id)")
	case !filepath.IsAbs(cfg.KubeletRoot):
		return nil, fmt.Errorf("kubelet root %q must be an absolute path", cfg.KubeletRoot)
	case !filepath.IsAbs(cfg.Root):
		return nil, fmt.Errorf("root %q must be an absolute path", cfg.Root)
	case cfg.SizeBytes <= 0:
		return nil, fmt.Errorf("size must be positive, got %d", cfg.SizeBytes)
	}
	cfg.KubeletRoot = filepath.Clean(cfg.KubeletRoot)
	cfg.Root = filepath.Clean(cfg.Root)
	return &Driver{cfg: cfg, mounter: m, log: log}, nil
}

// --- Identity ----------------------------------------------------------------

// GetPluginInfo reports the driver name and version.
func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: d.cfg.Version}, nil
}

// GetPluginCapabilities reports none: in particular no CONTROLLER_SERVICE, so
// nothing ever calls a controller RPC on this plugin.
func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

// Probe reports ready: the plugin has no dependency that can be unready once
// it is serving.
func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

// --- Node --------------------------------------------------------------------

// NodeGetInfo reports the node ID. No topology, no volume limit.
func (d *Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: d.cfg.NodeID}, nil
}

// NodeGetCapabilities advertises VOLUME_MOUNT_GROUP only. With the CSIDriver's
// fsGroupPolicy: File that is what makes the kubelet hand the pod's fsGroup to
// NodePublishVolume as volume_mount_group — instead of recursively chowning the
// volume itself after the mount, which on this driver would race the app.
func (d *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{Capabilities: []*csi.NodeServiceCapability{{
		Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{
			Type: csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
		}},
	}}}, nil
}

// publishRequest is a validated NodePublishVolume.
type publishRequest struct {
	target string // cleaned target_path
	podUID string
	gid    uint32
}

// NodePublishVolume mounts the pod's per-pod tmpfs and binds it onto
// target_path. Every rejection is InvalidArgument, which the kubelet surfaces
// on the pod as a FailedMount event carrying the message.
func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	pr, err := d.validatePublish(req)
	if err != nil {
		d.log.Warn("rejecting NodePublishVolume", "volume_id", req.GetVolumeId(),
			"target_path", req.GetTargetPath(), "error", err)
		return nil, err
	}
	log := d.log.With("volume_id", req.GetVolumeId(), "pod_uid", pr.podUID, "target_path", pr.target)

	d.mu.Lock()
	defer d.mu.Unlock()

	// Idempotency: the kubelet republishes after its own restart (volume
	// reconstruction). If our bind is already there, there is nothing to do.
	if fstype, mounted, err := d.mounter.MountedFS(pr.target); err != nil {
		return nil, status.Errorf(codes.Internal, "inspect %s: %v", pr.target, err)
	} else if mounted {
		if fstype != "tmpfs" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"target %s is already a %s mount, not a %s volume", pr.target, fstype, DriverName)
		}
		log.Debug("already published")
		return &csi.NodePublishVolumeResponse{}, nil
	}

	podDir := filepath.Join(d.cfg.Root, pr.podUID)
	if err := d.ensurePodTmpfs(log, podDir, pr.gid); err != nil {
		return nil, err
	}

	// The CSI spec makes the SP responsible for creating target_path (the CO
	// only guarantees its parent).
	if err := d.mounter.MkdirAll(pr.target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create target %s: %v", pr.target, err)
	}
	// The container runtime re-binds target_path into the pod's containers
	// with the per-mount flags IT chooses, so nosuid/nodev/noexec/nosymfollow
	// are not what the app sees there (seen on kind: the pod's /s is plain
	// `rw`). They do not need to be: the symlink confused deputy is the node
	// proxy following an app-planted link, and the proxy reaches the socket
	// through the HOST's copy of the per-pod tmpfs, which carries them. The
	// superblock options (size, mode 2770, gid) hold in every view.
	if err := d.mounter.Mount(podDir, pr.target, "", bindFlags, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "bind %s onto %s: %v", podDir, pr.target, err)
	}

	log.Info("published", "pod_dir", podDir, "gid", pr.gid)
	return &csi.NodePublishVolumeResponse{}, nil
}

// ensurePodTmpfs mounts the per-pod tmpfs at podDir unless it already is one.
func (d *Driver) ensurePodTmpfs(log *slog.Logger, podDir string, gid uint32) error {
	fstype, mounted, err := d.mounter.MountedFS(podDir)
	if err != nil {
		return status.Errorf(codes.Internal, "inspect %s: %v", podDir, err)
	}
	if mounted {
		if fstype != "tmpfs" {
			return status.Errorf(codes.Internal, "%s is a %s mount, not the per-pod tmpfs", podDir, fstype)
		}
		// A second volume of the same pod, or a republish after the target
		// was unmounted by hand: reuse the pod's tmpfs.
		return nil
	}
	if err := d.mounter.MkdirAll(podDir, 0o700); err != nil {
		return status.Errorf(codes.Internal, "create %s: %v", podDir, err)
	}
	data := TmpfsData(gid, d.cfg.SizeBytes)
	err = d.mounter.Mount("tmpfs", podDir, "tmpfs", tmpfsFlags|noSymfollowFlag, data)
	if errors.Is(err, syscall.EINVAL) {
		// A kernel older than 5.10 rejects MS_NOSYMFOLLOW. Not fatal, but
		// loud: it is the flag that closes the symlink confused-deputy class
		// (proposal 039), and refusing to mount would turn an old kernel into
		// a FailedMount for every UDS pod on the node.
		log.Warn("the kernel refused MS_NOSYMFOLLOW; mounting the per-pod tmpfs nosuid,nodev,noexec WITHOUT it — symlinks on it will be followed",
			"pod_dir", podDir, "error", err)
		err = d.mounter.Mount("tmpfs", podDir, "tmpfs", tmpfsFlags, data)
	}
	if err != nil {
		return status.Errorf(codes.Internal, "mount per-pod tmpfs at %s: %v", podDir, err)
	}
	return nil
}

// TmpfsData is the tmpfs mount data string for a pod whose fsGroup is gid.
func TmpfsData(gid uint32, sizeBytes int64) string {
	return fmt.Sprintf("mode=%s,uid=0,gid=%d,size=%d", tmpfsMode, gid, sizeBytes)
}

func (d *Driver) validatePublish(req *csi.NodePublishVolumeRequest) (*publishRequest, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	vc := req.GetVolumeContext()
	if vc[ctxEphemeral] != "true" {
		return nil, status.Errorf(codes.InvalidArgument,
			"%s only serves inline ephemeral volumes (volume_context %s=%q); declare it as `volumes: [{csi: {driver: %s}}]` in the pod spec, not through a PersistentVolume",
			DriverName, ctxEphemeral, vc[ctxEphemeral], DriverName)
	}
	for k := range vc {
		if !strings.HasPrefix(k, ctxPrefix) {
			return nil, status.Errorf(codes.InvalidArgument,
				"%s takes no volumeAttributes, got %q", DriverName, k)
		}
	}
	podUID := vc[ctxPodUID]
	if !podUIDPattern.MatchString(podUID) {
		return nil, status.Errorf(codes.InvalidArgument,
			"volume_context %s=%q is not a pod UID (is podInfoOnMount enabled on the CSIDriver?)", ctxPodUID, podUID)
	}
	if req.GetReadonly() {
		return nil, status.Errorf(codes.InvalidArgument,
			"%s volumes cannot be read-only: the app must be able to bind(2) its socket there", DriverName)
	}
	vcap := req.GetVolumeCapability()
	if vcap == nil || vcap.GetMount() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s only serves filesystem (mount) volumes", DriverName)
	}
	target := filepath.Clean(req.GetTargetPath())
	if uid, ok := d.podUIDFromTarget(target); !ok || uid != podUID {
		return nil, status.Errorf(codes.InvalidArgument,
			"target_path %s is not under %s/pods/%s/ (is --kubelet-root right?)", target, d.cfg.KubeletRoot, podUID)
	}
	group := vcap.GetMount().GetVolumeMountGroup()
	if group == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"%s needs the pod's fsGroup to own the socket directory, and this pod has none: %s", DriverName, fsGroupHint)
	}
	gid, err := strconv.ParseUint(group, 10, 32)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "volume_mount_group %q is not a numeric group ID", group)
	}
	return &publishRequest{target: target, podUID: podUID, gid: uint32(gid)}, nil
}

// podUIDFromTarget extracts <uid> from <KubeletRoot>/pods/<uid>/<rest>. The
// target path is the kubelet's, never the workload's.
func (d *Driver) podUIDFromTarget(target string) (string, bool) {
	prefix := d.cfg.KubeletRoot + "/pods/"
	if !strings.HasPrefix(target, prefix) {
		return "", false
	}
	uid, rest, ok := strings.Cut(strings.TrimPrefix(target, prefix), "/")
	if !ok || rest == "" || !podUIDPattern.MatchString(uid) {
		return "", false
	}
	return uid, true
}

// NodeUnpublishVolume unmounts the bind at target_path, then the pod's tmpfs,
// and removes both directories. Every step tolerates "already done", so a
// retry after a partial failure — or an unpublish of something never
// published — converges to OK.
func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	target := filepath.Clean(req.GetTargetPath())
	podUID, ok := d.podUIDFromTarget(target)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument,
			"target_path %s is not under %s/pods/<pod-uid>/", target, d.cfg.KubeletRoot)
	}
	log := d.log.With("volume_id", req.GetVolumeId(), "pod_uid", podUID, "target_path", target)

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.unmountIfMounted(target, 0); err != nil {
		return nil, err
	}
	if err := d.mounter.Remove(target); err != nil {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", target, err)
	}
	// MNT_DETACH: the pod's containers are gone by now, but a straggling fd
	// (the node proxy mid-connect, say) must not fail the unpublish and leave
	// the pod stuck Terminating.
	podDir := filepath.Join(d.cfg.Root, podUID)
	if err := d.unmountIfMounted(podDir, detachFlag); err != nil {
		return nil, err
	}
	if err := d.mounter.Remove(podDir); err != nil {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", podDir, err)
	}
	log.Info("unpublished", "pod_dir", podDir)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) unmountIfMounted(path string, flags int) error {
	_, mounted, err := d.mounter.MountedFS(path)
	if err != nil {
		return status.Errorf(codes.Internal, "inspect %s: %v", path, err)
	}
	if !mounted {
		return nil
	}
	if err := d.mounter.Unmount(path, flags); err != nil {
		return status.Errorf(codes.Internal, "unmount %s: %v", path, err)
	}
	return nil
}
