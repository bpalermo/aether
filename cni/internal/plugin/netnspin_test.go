package plugin

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bpalermo/aether/cni/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func pinTestConf(t *testing.T) config.AetherConf {
	t.Helper()
	return config.AetherConf{NetnsPinDir: t.TempDir()}
}

func TestNetnsPinPathDefaults(t *testing.T) {
	assert.Equal(t, "/run/aether/netns/abc", config.AetherConf{}.NetnsPinPath("abc"))
	assert.Equal(t, "/custom/abc", config.AetherConf{NetnsPinDir: "/custom"}.NetnsPinPath("abc"))
}

func TestNetnsUnpinDelay(t *testing.T) {
	assert.Equal(t, 60*time.Second, config.AetherConf{}.NetnsUnpinDelay())
	assert.Equal(t, time.Duration(0), config.AetherConf{NetnsUnpinDelaySeconds: -1}.NetnsUnpinDelay())
	assert.Equal(t, 3*time.Second, config.AetherConf{NetnsUnpinDelaySeconds: 3}.NetnsUnpinDelay())
}

// Unpinning a container that was never pinned (pin failed at ADD, or DEL
// retried after cleanup) must succeed silently.
func TestUnpinNetnsMissingIsNoop(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	require.NoError(t, p.unpinNetns(pinTestConf(t), "never-pinned"))
}

// A pin target that exists but is not a mount point (pin creation failed
// mid-way) is removed without error.
func TestUnpinNetnsPlainFile(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	conf := pinTestConf(t)
	target := conf.NetnsPinPath("half-pinned")
	require.NoError(t, os.WriteFile(target, nil, 0o600))

	require.NoError(t, p.unpinNetns(conf, "half-pinned"))
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err))
}

// Without CAP_SYS_ADMIN the bind mount fails; pinNetns must surface the error
// (CmdAdd then falls back to the runtime path) and leave no half-created pin.
func TestPinNetnsWithoutPrivilegeFailsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; mount would succeed")
	}
	p := NewAetherPlugin(zap.NewNop())
	conf := pinTestConf(t)
	src := filepath.Join(t.TempDir(), "netns")
	require.NoError(t, os.WriteFile(src, nil, 0o600))

	_, err := p.pinNetns(conf, src, "cid-1")
	require.Error(t, err)
	_, statErr := os.Stat(conf.NetnsPinPath("cid-1"))
	assert.True(t, os.IsNotExist(statErr), "failed pin must not leave a mount point behind")
}

// Root-only: full pin -> unpin round trip with a real bind mount.
func TestPinUnpinNetnsRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for bind mounts")
	}
	p := NewAetherPlugin(zap.NewNop())
	conf := pinTestConf(t)
	src := filepath.Join(t.TempDir(), "netns")
	require.NoError(t, os.WriteFile(src, []byte("ns"), 0o600))

	pinned, err := p.pinNetns(conf, src, "cid-rt")
	require.NoError(t, err)
	assert.Equal(t, conf.NetnsPinPath("cid-rt"), pinned)
	b, err := os.ReadFile(pinned)
	require.NoError(t, err)
	assert.Equal(t, "ns", string(b))

	// Re-ADD replaces the existing pin.
	_, err = p.pinNetns(conf, src, "cid-rt")
	require.NoError(t, err)

	require.NoError(t, p.unpinNetns(conf, "cid-rt"))
	_, statErr := os.Stat(pinned)
	assert.True(t, os.IsNotExist(statErr))
}

// The GC sweep unpins entries absent from the valid-attachments set and keeps
// the rest.
func TestSweepNetnsPins(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	conf := pinTestConf(t)
	require.NoError(t, os.WriteFile(conf.NetnsPinPath("keep"), nil, 0o600))
	require.NoError(t, os.WriteFile(conf.NetnsPinPath("orphan"), nil, 0o600))

	p.sweepNetnsPins(conf, []byte(`{"cni.dev/valid-attachments":[{"containerID":"keep"}]}`))

	_, err := os.Stat(conf.NetnsPinPath("keep"))
	assert.NoError(t, err)
	_, err = os.Stat(conf.NetnsPinPath("orphan"))
	assert.True(t, os.IsNotExist(err))
}

// A GC payload without attachments (or an unreadable pin dir) must not panic.
func TestSweepNetnsPinsEmpty(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	p.sweepNetnsPins(config.AetherConf{NetnsPinDir: filepath.Join(t.TempDir(), "missing")}, []byte(`{}`))
}

// TestUnpinDelayDefault pins the 60s drain-tail margin (observed late dials up
// to ~13s post-removal; a dial through a released pin segfaults Envoy).
func TestUnpinDelayDefault(t *testing.T) {
	assert.Equal(t, 60*time.Second, config.AetherConf{}.NetnsUnpinDelay())
}

// TestRunDetachedUnpin: the detached entrypoint waits and releases the target;
// bad argv is rejected without panicking.
func TestRunDetachedUnpin(t *testing.T) {
	p := NewAetherPlugin(zap.NewNop())
	conf := pinTestConf(t)
	target := conf.NetnsPinPath("det-1")
	require.NoError(t, os.WriteFile(target, nil, 0o600))

	p.RunDetachedUnpin([]string{target, "1ms"})
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err))

	p.RunDetachedUnpin([]string{"only-one-arg"})
	p.RunDetachedUnpin([]string{target, "not-a-duration"})
}
