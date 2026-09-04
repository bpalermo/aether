// Package cniconflist keeps aether chained in the node's active CNI conflist.
//
// Aether installs itself as a chained plugin INSIDE another CNI's conflist, and
// the cni-install init container appends that entry exactly once per agent pod
// start. Any competing writer that rewrites the file from its own template wins
// permanently: on Talos, kube-flannel's init container does an unconditional
// `cp -f /etc/kube-flannel/cni-conf.json /etc/cni/net.d/10-flannel.conflist` on
// every flannel pod recreation — which Talos triggers on every bootstrap-manifest
// re-sync (boot, `talosctl apply-config`, Kubernetes upgrades). The aether entry
// is silently stripped, no CNI ADD ever reaches the agent again, and every
// subsequently-started pod on that node runs outside the mesh (incident
// 2026-08-29, issue #645). Workload rolls cannot fix it; only re-running
// cni-install can.
//
// The Reasserter closes that window to seconds: it watches the CNI config
// directory (fsnotify) plus a slow periodic re-check, and re-appends the entry
// whenever it goes missing — using the same mutation the installer uses
// (aethermesh.dev/cni/conflist), never a fork of it.
package cniconflist

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"aethermesh.dev/cni/conflist"
	"aethermesh.dev/common/file"
	commonlog "aethermesh.dev/common/log"
	"github.com/fsnotify/fsnotify"
	"go.opentelemetry.io/otel"
)

const (
	// DefaultCheckInterval is the belt-and-suspenders re-check period, for the
	// events fsnotify never delivers (watch dropped, mount replaced, an event
	// lost while the agent was restarting).
	DefaultCheckInterval = 60 * time.Second

	// DefaultSettleDelay is how long the watcher waits after the last filesystem
	// event before inspecting the file. A writer doing an in-place `cp -f` walks
	// the file through a truncated/partial state, which would parse as garbage;
	// a short settle also collapses the event burst of a single rewrite into one
	// check.
	DefaultSettleDelay = 750 * time.Millisecond

	// confMode is the mode the conflist is written back with, matching the
	// installer's.
	confMode = os.FileMode(0o644)
)

// Reasserter is the controller-runtime Runnable that keeps aether's entry in the
// node's active CNI conflist. It never CREATES a config: it only re-appends the
// entry to an existing, valid, base-CNI-carrying conflist. Boot ordering (no
// conflist yet) stays owned by cni-install.
type Reasserter struct {
	// Dir is the CNI network-config directory as mounted into the agent
	// (read-write). Required.
	Dir string
	// Interval overrides DefaultCheckInterval when positive (tests).
	Interval time.Duration
	// SettleDelay overrides DefaultSettleDelay when positive (tests).
	SettleDelay time.Duration
	// Log is the agent logger; a "cni-conflist" child is derived from it.
	Log *slog.Logger

	log     *slog.Logger
	metrics *reassertMetrics

	// entry is the last aether plugin entry OBSERVED in the conflist, and the one
	// re-appended when it goes missing. Priming it from the file rather than
	// re-rendering it here is deliberate: the netconf carries installer-time
	// parameters (OTLP endpoint, redirect-all default, mesh-DNS DNAT target) that
	// the agent has no business re-deriving, and cni-install — the agent pod's own
	// init container — re-renders and re-appends it at every agent start, so the
	// cache is primed on the first check of every agent lifetime.
	entry []byte

	// chainStatePublisher is the lock-free publication of the chaining state to
	// the rest of the agent (the node taint gate, the readiness check, the ghost
	// sweep's eviction interlock). Embedded rather than a field so *Reasserter
	// satisfies ChainState directly; its zero value is a usable "not observed
	// yet", so a Reasserter that was never Start()ed reports unknown.
	chainStatePublisher
}

// Start runs the watch + periodic re-check loop until the context is cancelled.
// It never returns an error: a broken watch degrades a safety net, it must never
// take the agent (and with it the CNI socket) down.
func (r *Reasserter) Start(ctx context.Context) error {
	r.log = commonlog.Named(r.Log, "cni-conflist")

	metrics, err := newMetrics(otel.Meter(meterName))
	if err != nil {
		r.log.Error("failed to create CNI conflist metrics; continuing without instrumentation", "error", err)
	}
	r.metrics = metrics

	events, errs, closeWatcher := r.watch(ctx)
	defer closeWatcher()

	interval := r.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	settleDelay := r.SettleDelay
	if settleDelay <= 0 {
		settleDelay = DefaultSettleDelay
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Created already-fired-and-drained: nothing is pending until an event lands.
	settle := time.NewTimer(settleDelay)
	settle.Stop()
	defer settle.Stop()

	r.log.InfoContext(ctx, "CNI conflist re-assert loop started", "dir", r.Dir, "interval", interval, "settleDelay", settleDelay)
	r.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.check(ctx)
		case <-settle.C:
			r.check(ctx)
		case event, ok := <-events:
			if !ok {
				// The watcher died (its channels are closed). Drop it — a nil
				// channel never fires again — and carry on with the periodic
				// re-check rather than spinning on a closed channel.
				r.log.WarnContext(ctx, "CNI config directory watch closed; falling back to periodic re-checks only", "dir", r.Dir)
				events, errs = nil, nil
				continue
			}
			if !isConfigEvent(event) {
				continue
			}
			// Go >= 1.23 timer semantics: Stop+Reset cannot leave a stale value
			// on the channel, so no drain is needed.
			settle.Stop()
			settle.Reset(settleDelay)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			r.log.WarnContext(ctx, "CNI config directory watch error", "dir", r.Dir, "error", err)
		}
	}
}

// NeedLeaderElection reports false: every node's conflist is that node's own, so
// this runs on every agent rather than a single leader.
func (r *Reasserter) NeedLeaderElection() bool { return false }

// watch starts an fsnotify watch on the config directory. A watch that cannot be
// established is logged and degraded to the periodic re-check (nil channels
// simply never fire in the select), never a startup failure.
func (r *Reasserter) watch(ctx context.Context) (<-chan fsnotify.Event, <-chan error, func()) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.log.WarnContext(ctx, "failed to create the CNI config watcher; falling back to periodic re-checks only", "error", err)
		return nil, nil, func() {}
	}
	if err := watcher.Add(r.Dir); err != nil {
		r.log.WarnContext(ctx, "failed to watch the CNI config directory; falling back to periodic re-checks only", "dir", r.Dir, "error", err)
		_ = watcher.Close()
		return nil, nil, func() {}
	}
	return watcher.Events, watcher.Errors, func() { _ = watcher.Close() }
}

// isConfigEvent reports whether an event concerns a CNI config file appearing,
// changing, or disappearing. Chmod-only events and the temp files atomic writers
// leave behind (their extension is not .conf/.conflist) are ignored.
func isConfigEvent(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	switch filepath.Ext(event.Name) {
	case ".conf", ".conflist":
		return true
	default:
		return false
	}
}

// check inspects the active conflist and re-appends aether's entry if a
// competing writer stripped it. It records the chain-present state on EVERY
// call, zero included, so the gauge is the alerting signal for "this node's
// pods are being created outside the mesh" — and, through the same record()
// call, so is the in-process ChainState the taint gate and readiness check read
// (#667).
func (r *Reasserter) check(ctx context.Context) {
	path, data, chain, err := r.readActive()
	if err != nil {
		// No usable conflist at all: the primary CNI has not written one yet (boot)
		// or it is corrupt. Either way aether is not chained and this loop must not
		// manufacture a config — cni-install and boot ordering own that.
		r.log.WarnContext(ctx, "no usable CNI conflist to inspect; leaving the directory untouched", "dir", r.Dir, "error", err)
		r.record(ctx, false)
		return
	}

	entry, present, err := chain.AetherEntry()
	if err != nil {
		r.log.WarnContext(ctx, "failed to read the chained aether entry", "path", path, "error", err)
		r.record(ctx, present)
		return
	}
	if present {
		// Steady state: refresh the last-known-good entry and record the gauge.
		r.entry = entry
		r.record(ctx, true)
		return
	}

	r.record(ctx, false)
	r.repair(ctx, path, data, chain)
}

// repair re-appends the last-known-good aether entry to a conflist it went
// missing from, under the guardrails: a real base CNI must still be there, and a
// known-good entry must have been observed at least once.
func (r *Reasserter) repair(ctx context.Context, path string, data []byte, chain *conflist.Chain) {
	if !chain.HasBasePlugin() {
		r.log.WarnContext(ctx, "active CNI conflist carries no primary CNI plugin; not chaining aether into it", "path", path)
		return
	}
	if len(r.entry) == 0 {
		r.log.WarnContext(ctx, "aether is not chained in the active CNI conflist and no known-good entry was ever observed; cni-install must run", "path", path)
		return
	}

	merged, err := conflist.Insert(r.entry, data)
	if err != nil {
		r.log.WarnContext(ctx, "failed to rebuild the CNI conflist with aether chained", "path", path, "error", err)
		return
	}
	if err := file.AtomicWrite(path, merged, confMode); err != nil {
		r.log.WarnContext(ctx, "failed to write the re-asserted CNI conflist", "path", path, "error", err)
		return
	}

	r.metrics.reasserted(ctx)
	r.record(ctx, true)
	r.log.InfoContext(ctx, "re-asserted the aether entry in the CNI conflist after a competing writer stripped it", "path", path)
}

// readActive resolves, reads and parses the conflist kubelet is actually using
// (the lexicographically first usable one in Dir).
func (r *Reasserter) readActive() (string, []byte, *conflist.Chain, error) {
	path, err := conflist.ActivePath(r.Dir)
	if err != nil {
		return "", nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, nil, nil, err
	}
	chain, err := conflist.Parse(data)
	if err != nil {
		return path, data, nil, err
	}
	return path, data, chain, nil
}
