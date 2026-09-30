package udscsi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testUID     = "0f6a3b7e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	testRoot    = "/run/aether/uds"
	testKubelet = "/var/lib/kubelet"
	testSize    = 1 << 20
)

var testTarget = testKubelet + "/pods/" + testUID + "/volumes/kubernetes.io~csi/s/mount"

// mountCall is one recorded Mount.
type mountCall struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// fakeMounter models the host mount table as a map of mount point -> fstype,
// and records every mutating call.
type fakeMounter struct {
	mounts    map[string]string
	dirs      map[string]os.FileMode
	mountLog  []mountCall
	unmounts  []string
	unmountFl []int
	removed   []string

	// oldKernel makes a mount carrying MS_NOSYMFOLLOW fail EINVAL, like a
	// Linux < 5.10 kernel.
	oldKernel bool
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounts: map[string]string{}, dirs: map[string]os.FileMode{}}
}

func (f *fakeMounter) MountedFS(p string) (string, bool, error) {
	t, ok := f.mounts[p]
	return t, ok, nil
}

func (f *fakeMounter) MkdirAll(p string, perm os.FileMode) error {
	f.dirs[p] = perm
	return nil
}

func (f *fakeMounter) Mount(source, target, fstype string, flags uintptr, data string) error {
	f.mountLog = append(f.mountLog, mountCall{source, target, fstype, flags, data})
	if f.oldKernel && noSymfollowFlag != 0 && flags&noSymfollowFlag != 0 {
		return fmt.Errorf("mount: %w", syscall.EINVAL)
	}
	if flags&bindFlags != 0 && bindFlags != 0 {
		f.mounts[target] = f.mounts[source]
	} else {
		f.mounts[target] = fstype
	}
	return nil
}

func (f *fakeMounter) Unmount(target string, flags int) error {
	f.unmounts = append(f.unmounts, target)
	f.unmountFl = append(f.unmountFl, flags)
	delete(f.mounts, target)
	return nil
}

func (f *fakeMounter) Remove(p string) error {
	f.removed = append(f.removed, p)
	delete(f.dirs, p)
	return nil
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestDriver(t *testing.T, m Mounter) *Driver {
	t.Helper()
	d, err := NewDriver(Config{
		NodeID: "node-a", KubeletRoot: testKubelet, Root: testRoot, SizeBytes: testSize, Version: "test",
	}, m, discard())
	require.NoError(t, err)
	return d
}

// publishReq is a well-formed kubelet request for an inline volume of a pod
// whose fsGroup is 65532; mutate customises it.
func publishReq(mutate ...func(*csi.NodePublishVolumeRequest)) *csi.NodePublishVolumeRequest {
	req := &csi.NodePublishVolumeRequest{
		// The kubelet's ephemeral volume handle: "csi-" + a hash. Deliberately
		// NOT the pod UID, so a test can tell which one reached a path.
		VolumeId:   "csi-4d1e9a7c0b2f",
		TargetPath: testTarget,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{VolumeMountGroup: "65532"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/pod.uid":             testUID,
			"csi.storage.k8s.io/pod.name":            "app-0",
			"csi.storage.k8s.io/pod.namespace":       "team",
			"csi.storage.k8s.io/serviceAccount.name": "app",
			"csi.storage.k8s.io/ephemeral":           "true",
		},
	}
	for _, m := range mutate {
		m(req)
	}
	return req
}

func requireCode(t *testing.T, err error, want codes.Code, msgContains string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "not a gRPC status: %v", err)
	assert.Equal(t, want, st.Code(), "status: %v", st)
	assert.Contains(t, st.Message(), msgContains)
}

func TestNodePublishVolume_MountsPerPodTmpfsAndBindsIt(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err)

	podDir := testRoot + "/" + testUID
	require.Len(t, m.mountLog, 2, "exactly one tmpfs mount and one bind")
	assert.Equal(t, mountCall{
		source: "tmpfs", target: podDir, fstype: "tmpfs",
		flags: tmpfsFlags | noSymfollowFlag,
		data:  "mode=2770,uid=0,gid=65532,size=1048576",
	}, m.mountLog[0], "the per-pod tmpfs: exact flags and data string")
	assert.Equal(t, mountCall{source: podDir, target: testTarget, flags: bindFlags}, m.mountLog[1],
		"the per-pod tmpfs is bind-mounted onto the kubelet's target_path")
	assert.Equal(t, os.FileMode(0o700), m.dirs[podDir], "per-pod mountpoint dir")
	assert.Contains(t, m.dirs, testTarget, "the SP creates target_path")
}

func TestTmpfsFlagsAreNosuidNodevNoexec(t *testing.T) {
	if tmpfsFlags == 0 {
		t.Skip("non-Linux build")
	}
	// MS_NOSUID|MS_NODEV|MS_NOEXEC in the Linux ABI.
	assert.Equal(t, uintptr(0x2|0x4|0x8), tmpfsFlags)
	assert.Equal(t, uintptr(0x100), noSymfollowFlag, "MS_NOSYMFOLLOW")
	assert.Equal(t, uintptr(0x1000), bindFlags, "MS_BIND")
}

func TestNodePublishVolume_NoFsGroupFailsWithTheFix(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq(func(r *csi.NodePublishVolumeRequest) {
		r.VolumeCapability.GetMount().VolumeMountGroup = ""
	}))
	requireCode(t, err, codes.InvalidArgument, "set pod.spec.securityContext.fsGroup")
	assert.Empty(t, m.mountLog, "nothing is mounted for a rejected request")
}

func TestNodePublishVolume_Rejections(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*csi.NodePublishVolumeRequest)
		msg    string
	}{
		"not ephemeral (a PV)": {
			func(r *csi.NodePublishVolumeRequest) { delete(r.VolumeContext, "csi.storage.k8s.io/ephemeral") },
			"only serves inline ephemeral volumes",
		},
		"ephemeral false": {
			func(r *csi.NodePublishVolumeRequest) { r.VolumeContext["csi.storage.k8s.io/ephemeral"] = "false" },
			"only serves inline ephemeral volumes",
		},
		"no pod uid (podInfoOnMount off)": {
			func(r *csi.NodePublishVolumeRequest) { delete(r.VolumeContext, "csi.storage.k8s.io/pod.uid") },
			"podInfoOnMount",
		},
		"pod uid with a path traversal": {
			func(r *csi.NodePublishVolumeRequest) { r.VolumeContext["csi.storage.k8s.io/pod.uid"] = "../../etc" },
			"is not a pod UID",
		},
		"volumeAttributes set by the pod author": {
			func(r *csi.NodePublishVolumeRequest) { r.VolumeContext["dir"] = "/etc" },
			"takes no volumeAttributes",
		},
		"read-only": {
			func(r *csi.NodePublishVolumeRequest) { r.Readonly = true },
			"cannot be read-only",
		},
		"block volume": {
			func(r *csi.NodePublishVolumeRequest) {
				r.VolumeCapability.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
			},
			"filesystem (mount) volumes",
		},
		"target outside the kubelet root": {
			func(r *csi.NodePublishVolumeRequest) { r.TargetPath = "/etc/cron.d/x" },
			"is not under /var/lib/kubelet/pods/",
		},
		"target of a different pod": {
			func(r *csi.NodePublishVolumeRequest) {
				r.TargetPath = testKubelet + "/pods/11111111-2222-3333-4444-555555555555/volumes/kubernetes.io~csi/s/mount"
			},
			"is not under /var/lib/kubelet/pods/" + testUID,
		},
		"non-numeric fsGroup": {
			func(r *csi.NodePublishVolumeRequest) { r.VolumeCapability.GetMount().VolumeMountGroup = "staff" },
			"not a numeric group ID",
		},
		"no volume id": {
			func(r *csi.NodePublishVolumeRequest) { r.VolumeId = "" },
			"volume_id is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newFakeMounter()
			d := newTestDriver(t, m)
			_, err := d.NodePublishVolume(context.Background(), publishReq(tc.mutate))
			requireCode(t, err, codes.InvalidArgument, tc.msg)
			assert.Empty(t, m.mountLog, "nothing is mounted for a rejected request")
		})
	}
}

func TestNodePublishVolume_Idempotent(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err)
	_, err = d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err, "a republish of a published volume is OK")
	assert.Len(t, m.mountLog, 2, "the republish mounted nothing")
}

func TestNodePublishVolume_ReusesAnExistingPodTmpfs(t *testing.T) {
	m := newFakeMounter()
	m.mounts[testRoot+"/"+testUID] = "tmpfs"
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err)
	require.Len(t, m.mountLog, 1, "only the bind")
	assert.Equal(t, bindFlags, m.mountLog[0].flags)
}

func TestNodePublishVolume_TargetMountedByAnotherFSIsNotOurs(t *testing.T) {
	m := newFakeMounter()
	m.mounts[testTarget] = "ext4"
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq())
	requireCode(t, err, codes.FailedPrecondition, "already a ext4 mount")
	assert.Empty(t, m.mountLog)
}

func TestNodePublishVolume_NoSymfollowUnsupportedIsNotFatal(t *testing.T) {
	if noSymfollowFlag == 0 {
		t.Skip("non-Linux build")
	}
	m := newFakeMounter()
	m.oldKernel = true
	d := newTestDriver(t, m)

	_, err := d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err, "an old kernel still gets a nosuid,nodev,noexec, size-capped mount")
	require.Len(t, m.mountLog, 3, "tmpfs with MS_NOSYMFOLLOW (EINVAL), tmpfs without it, bind")
	assert.Equal(t, tmpfsFlags|noSymfollowFlag, m.mountLog[0].flags)
	assert.Equal(t, tmpfsFlags, m.mountLog[1].flags, "the retry drops only MS_NOSYMFOLLOW")
	assert.Equal(t, m.mountLog[0].data, m.mountLog[1].data)
}

// The per-pod directory is <root>/<pod uid> and NOTHING else: not the volume
// id, not the pod name/namespace/service account, not any other context value.
func TestNodePublishVolume_PodDirIsDerivedFromThePodUIDOnly(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)

	req := publishReq(func(r *csi.NodePublishVolumeRequest) {
		r.VolumeId = "csi-../../../etc"
		r.VolumeContext["csi.storage.k8s.io/pod.name"] = "../../../etc"
		r.VolumeContext["csi.storage.k8s.io/pod.namespace"] = "/etc"
		r.VolumeContext["csi.storage.k8s.io/serviceAccount.name"] = "evil"
	})
	_, err := d.NodePublishVolume(context.Background(), req)
	require.NoError(t, err)

	want := filepath.Join(testRoot, testUID)
	require.NotEmpty(t, m.mountLog)
	assert.Equal(t, want, m.mountLog[0].target)
	for _, c := range m.mountLog {
		for _, p := range []string{c.source, c.target} {
			assert.False(t, strings.Contains(p, "etc") || strings.Contains(p, "evil") || strings.Contains(p, "csi-"),
				"a path was derived from something other than the pod uid: %q", p)
		}
	}
}

func TestNodeUnpublishVolume_TearsDownBothMounts(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)
	_, err := d.NodePublishVolume(context.Background(), publishReq())
	require.NoError(t, err)

	_, err = d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "csi-4d1e9a7c0b2f", TargetPath: testTarget,
	})
	require.NoError(t, err)

	podDir := testRoot + "/" + testUID
	assert.Equal(t, []string{testTarget, podDir}, m.unmounts, "the bind first, then the tmpfs")
	assert.Equal(t, []int{0, detachFlag}, m.unmountFl, "the tmpfs is lazily detached")
	assert.Equal(t, []string{testTarget, podDir}, m.removed)
	assert.Empty(t, m.mounts, "nothing left mounted")
}

func TestNodeUnpublishVolume_Idempotent(t *testing.T) {
	m := newFakeMounter()
	d := newTestDriver(t, m)

	for i := range 2 {
		_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
			VolumeId: "csi-4d1e9a7c0b2f", TargetPath: testTarget,
		})
		require.NoError(t, err, "unpublish #%d of a volume that is not mounted is OK", i+1)
	}
	assert.Empty(t, m.unmounts, "nothing was mounted, so nothing is unmounted")
}

func TestNodeUnpublishVolume_RejectsATargetOutsideTheKubeletRoot(t *testing.T) {
	d := newTestDriver(t, newFakeMounter())
	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "v", TargetPath: "/etc",
	})
	requireCode(t, err, codes.InvalidArgument, "is not under /var/lib/kubelet/pods/")
}

func TestNodeGetCapabilities_VolumeMountGroup(t *testing.T) {
	d := newTestDriver(t, newFakeMounter())
	resp, err := d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetCapabilities(), 1)
	assert.Equal(t, csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP, resp.GetCapabilities()[0].GetRpc().GetType())
}

func TestIdentity(t *testing.T) {
	d := newTestDriver(t, newFakeMounter())
	info, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "csi.aether.io", info.GetName())
	assert.Equal(t, "test", info.GetVendorVersion())

	caps, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Empty(t, caps.GetCapabilities(), "no CONTROLLER_SERVICE")

	probe, err := d.Probe(context.Background(), &csi.ProbeRequest{})
	require.NoError(t, err)
	assert.True(t, probe.GetReady().GetValue())

	node, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "node-a", node.GetNodeId())
}

func TestNewDriver_Validation(t *testing.T) {
	_, err := NewDriver(Config{KubeletRoot: testKubelet, Root: testRoot, SizeBytes: 1}, newFakeMounter(), discard())
	require.ErrorContains(t, err, "node ID is required")
	_, err = NewDriver(Config{NodeID: "n", KubeletRoot: "var/lib/kubelet", Root: testRoot, SizeBytes: 1}, newFakeMounter(), discard())
	require.ErrorContains(t, err, "absolute")
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1Mi": 1 << 20, "512Ki": 512 << 10, "4096": 4096, "1Gi": 1 << 30} {
		got, err := ParseSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "0", "-1Mi", "2Gi", "1MB", "lots"} {
		_, err := ParseSize(in)
		assert.Error(t, err, in)
	}
}

func TestMountedFS(t *testing.T) {
	info := strings.Join([]string{
		`22 1 0:21 / / rw,relatime shared:1 - overlay overlay rw`,
		`36 22 0:40 / /run/aether/uds/` + testUID + ` rw,nosuid,nodev,noexec,nosymfollow shared:9 - tmpfs tmpfs rw,size=1024k,mode=2770,gid=65532`,
		`37 22 0:40 / /var/lib/kubelet/pods/x/volumes/kubernetes.io~csi/with\040space/mount rw shared:9 - tmpfs tmpfs rw`,
		`38 22 8:1 /data /mnt/stacked rw - ext4 /dev/sda1 rw`,
		`39 38 0:41 / /mnt/stacked rw - tmpfs tmpfs rw`,
	}, "\n")

	fstype, ok, err := mountedFS(strings.NewReader(info), "/run/aether/uds/"+testUID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "tmpfs", fstype)

	_, ok, err = mountedFS(strings.NewReader(info), "/var/lib/kubelet/pods/x/volumes/kubernetes.io~csi/with space/mount")
	require.NoError(t, err)
	assert.True(t, ok, "octal-escaped mount points are unescaped")

	fstype, ok, err = mountedFS(strings.NewReader(info), "/mnt/stacked")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "tmpfs", fstype, "the topmost mount wins")

	_, ok, err = mountedFS(strings.NewReader(info), "/run/aether/uds")
	require.NoError(t, err)
	assert.False(t, ok, "a parent of a mount point is not a mount point")
}
