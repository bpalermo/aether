package hotrestart

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// The "initializing" flag of Envoy's hot-restart shared memory, and the one
// case in which the supervisor clears it (issue #1680).
//
// Every Envoy sets the flag when it attaches the base id's shared memory and
// clears it when its workers start. An Envoy started at an epoch above 0 that
// finds it set exits at once with "previous envoy process is still
// initializing": it must not hot-restart from a parent that is not ready.
// Nothing clears the flag for an Envoy that DIES while it initializes. From
// then on no hot restart from the healthy Envoy can succeed, and the node keeps
// its proxy only until something needs to replace it: the next roll's pod
// retried for minutes and crash-looped, until the old pod was deleted by hand.
//
// Two things leave the flag behind. An Envoy started at epoch 0 beside a live
// one whose epoch is not a multiple of 3 (unheard.go; a supervisor older than
// that fix still does it), which also replaces the shared memory itself. And a
// hot-restart child killed during its initialization (the OOM killer, a node
// under pressure, the handoff watchdog's SIGKILL).
//
// The flag is stale exactly when no Envoy of the base id is initializing, and
// an Envoy that is alive holds its hot-restart sockets from before it touches
// the shared memory until it exits. So before it forks an Envoy at epoch N > 0
// the supervisor clears a set flag when both hold:
//
//   - the node's admin reports LIVE at epoch N-1 (the parent has finished
//     initializing: LIVE is the state it reaches when it does), and
//   - the only hot-restart socket id held is (N-1) mod 3: the parent is the
//     one Envoy of the base id that exists. A draining grandparent, another
//     pod's child, a stray start that has not exited yet: any of them makes the
//     supervisor leave the flag alone, fork as before, and try again on the
//     retry that follows the child's exit.
//
// The clear is a write into another program's memory layout, so it is held to
// what was measured on the pinned Envoy: the segment's size field must equal
// the file's size, the version field must be the pinned one, and the flags are
// the segment's last 8 bytes. A layout the supervisor does not recognise is
// left untouched and logged; //agent/test/hotrestartrevert fails on a pin bump
// that changes it.
//
// The healthy parent keeps serving through all of this: after a stray epoch 0
// it maps the OLD segment (unlinked, still its own), and the segment holds
// nothing but the flag and two log mutexes.

const (
	// envoyShmDir is where shm_open puts Envoy's segment; the proxy pods share
	// it through a hostPath.
	envoyShmDir = "/dev/shm"
	// envoyHotRestartVersion is HOT_RESTART_VERSION of the pinned Envoy
	// (source/server/hot_restart_impl.cc), the second field of the segment.
	// Envoy bumps it when the segment's layout changes.
	envoyHotRestartVersion = 11
	// envoyShmFlagInitializing is SHMEM_FLAGS_INITIALIZING.
	envoyShmFlagInitializing = 0x1
	// The segment is two uint64 (size, version), two process-shared mutexes and
	// the flags: 104 bytes on amd64 (measured), larger where pthread_mutex_t
	// is. Sizes outside this range are not a layout the supervisor knows.
	envoyShmMinSize = 64
	envoyShmMaxSize = 256

	// triggerInitializingFlagCleared is the restart_triggers value counted
	// when the supervisor clears a stale flag before a hot restart.
	triggerInitializingFlagCleared = "stale_initializing_flag_cleared"
)

// errUnknownShmLayout: the segment is not laid out as the pinned Envoy's.
var errUnknownShmLayout = errors.New("unrecognised envoy shared-memory layout")

// shmPath is the base id's shared-memory file. Envoy names it by base id * 10.
func (s *Supervisor) shmPath() string {
	return filepath.Join(s.shmDir, "envoy_shared_memory_"+strconv.FormatUint(uint64(s.cfg.BaseID)*10, 10))
}

// shmFlagsOffset validates the segment in f against the pinned layout and
// returns where its flags are.
func shmFlagsOffset(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size < envoyShmMinSize || size > envoyShmMaxSize || size%8 != 0 {
		return 0, fmt.Errorf("%w: file size %d", errUnknownShmLayout, size)
	}
	var head [16]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return 0, err
	}
	// Native byte order: the segment is another process's memory.
	if got := binary.NativeEndian.Uint64(head[0:8]); got != uint64(size) {
		return 0, fmt.Errorf("%w: size field %d, file size %d", errUnknownShmLayout, got, size)
	}
	if got := binary.NativeEndian.Uint64(head[8:16]); got != envoyHotRestartVersion {
		return 0, fmt.Errorf("%w: hot-restart version %d, the supervisor knows %d", errUnknownShmLayout, got, envoyHotRestartVersion)
	}
	return size - 8, nil
}

// shmInitializing reports whether the base id's shared memory has the
// "initializing" flag set. A missing segment is "not set".
func (s *Supervisor) shmInitializing() (set bool, err error) {
	f, err := os.Open(s.shmPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = f.Close() }()
	off, err := shmFlagsOffset(f)
	if err != nil {
		return false, err
	}
	var flags [8]byte
	if _, err := f.ReadAt(flags[:], off); err != nil {
		return false, err
	}
	return binary.NativeEndian.Uint64(flags[:])&envoyShmFlagInitializing != 0, nil
}

// clearShmInitializing clears the flag. The caller has established that no
// Envoy of the base id is initializing, so nothing else writes the flags
// between the read and the write.
func (s *Supervisor) clearShmInitializing() error {
	f, err := os.OpenFile(s.shmPath(), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	off, err := shmFlagsOffset(f)
	if err != nil {
		return err
	}
	var flags [8]byte
	if _, err := f.ReadAt(flags[:], off); err != nil {
		return err
	}
	v := binary.NativeEndian.Uint64(flags[:]) &^ envoyShmFlagInitializing
	binary.NativeEndian.PutUint64(flags[:], v)
	_, err = f.WriteAt(flags[:], off)
	return err
}

// clearStaleInitializing runs before an Envoy is forked at epoch (> 0, a hot
// restart from epoch-1): it clears the shared memory's "initializing" flag
// when it is set and stale (see the top of this file), so the fork is not
// refused for an Envoy that died long ago. Whatever it finds, the fork goes
// ahead: a flag it may not clear is Envoy's to refuse on.
func (s *Supervisor) clearStaleInitializing(ctx context.Context, epoch int) {
	if epoch <= 0 || s.cfg.AdminAddress == "" {
		return
	}
	set, err := s.shmInitializing()
	if err != nil {
		s.log.WarnContext(ctx, "cannot read the hot-restart shared memory's initializing flag; if the next envoy exits with "+
			"\"previous envoy process is still initializing\", delete the proxy pod that holds the node's envoy",
			"path", s.shmPath(), "error", err)
		return
	}
	if !set {
		return
	}
	parent := epoch - 1
	held := s.heldHotRestartIDs()
	live, liveOK := s.adminLiveEpoch(context.WithoutCancel(ctx))
	if !liveOK || live != parent || !slices.Equal(held, []int{parent % envoyMaxConcurrentEpochs}) {
		s.log.InfoContext(ctx, "the hot-restart shared memory is marked initializing and another envoy of this base id "+
			"may be; leaving the flag (the next envoy exits at once if it is set, and is retried)",
			"epoch", epoch, "parentEpoch", parent, "adminLive", liveOK, "adminLiveEpoch", live, "heldSocketIDs", held)
		return
	}
	if err := s.clearShmInitializing(); err != nil {
		s.log.ErrorContext(ctx, "the hot-restart shared memory is marked initializing by an envoy that no longer exists, "+
			"and the flag could not be cleared: every hot restart from the node's envoy will exit with "+
			"\"previous envoy process is still initializing\" until the proxy pod that holds it is deleted",
			"path", s.shmPath(), "epoch", epoch, "parentEpoch", parent, "error", err)
		return
	}
	s.metrics.restartTriggered(triggerInitializingFlagCleared)
	s.log.WarnContext(ctx, "cleared a stale initializing flag in the hot-restart shared memory: an envoy of this base id "+
		"died while it initialized (or was started at epoch 0 beside the live one), and no hot restart could have "+
		"succeeded with the flag set",
		"path", s.shmPath(), "epoch", epoch, "parentEpoch", parent, "heldSocketIDs", held)
}
