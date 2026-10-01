// Command uds-csi is the csi.aether.io CSI node plugin (proposal 039 Phase 1):
// the mesh-owned carrier for the directory a UDS-served workload binds its
// socket in. See //agent/internal/udscsi for what a publish does.
//
// It serves two gRPC endpoints on Unix sockets under the kubelet root:
//
//   - the CSI Identity + Node services on <kubelet-root>/plugins/csi.aether.io/csi.sock;
//   - the kubelet plugin-registration service on
//     <kubelet-root>/plugins_registry/csi.aether.io-reg.sock — served here, not
//     by a csi-node-driver-registrar sidecar.
//
// `uds-csi --probe` is the DaemonSet's exec liveness probe: exit 0 iff the CSI
// socket exists. It is a flag on this binary rather than its own binary because
// the probe runs rarely (every 30s) and this binary is already small.
//
// Keep it small: //agent/cmd/uds-csi:deps_test fails the build if this binary
// ever links controller-runtime, client-go, apimachinery, go-control-plane or
// SPIRE, and scripts/check-uds-csi-deps.sh asserts the same on the build graph.
// It makes no Kubernetes API calls and needs no RBAC.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"aethermesh.dev/agent/internal/udscsi"
	"aethermesh.dev/common/log"
	"aethermesh.dev/common/signals"
)

// Version is set at build time via -ldflags (Bazel x_defs). It is what
// GetPluginInfo reports as vendor_version.
var Version = "dev"

type options struct {
	kubeletRoot        string
	csiSocket          string
	registrationSocket string
	nodeID             string
	root               string
	size               string
	inodes             int64
	debug              bool
	probe              bool
}

// newFlagSet binds every flag to o. //agent/cmd/uds-csi:uds-csi_test asserts
// that each flag the chart passes exists here.
func newFlagSet(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("uds-csi", flag.ContinueOnError)
	fs.StringVar(&o.kubeletRoot, "kubelet-root", udscsi.DefaultKubeletRoot,
		"The kubelet's --root-dir. Every target_path must lie under <kubelet-root>/pods/, and the socket defaults derive from it")
	fs.StringVar(&o.csiSocket, "csi-socket", "",
		"CSI endpoint (default <kubelet-root>/plugins/csi.aether.io/csi.sock). Must be the path the KUBELET sees")
	fs.StringVar(&o.registrationSocket, "registration-socket", "",
		"Kubelet plugin-registration endpoint (default <kubelet-root>/plugins_registry/csi.aether.io-reg.sock)")
	fs.StringVar(&o.nodeID, "node-id", os.Getenv("NODE_NAME"),
		"Node name reported by NodeGetInfo (default $NODE_NAME)")
	fs.StringVar(&o.root, "root", udscsi.DefaultRoot,
		"Host directory holding the per-pod tmpfs mounts (<root>/<pod-uid>)")
	fs.StringVar(&o.size, "size", "1Mi", "Size cap of each per-pod tmpfs (bytes, or Ki/Mi/Gi)")
	fs.Int64Var(&o.inodes, "inodes", 64,
		fmt.Sprintf("Inode cap (nr_inodes) of each per-pod tmpfs, its root directory included; at least %d", udscsi.MinInodes))
	fs.BoolVar(&o.debug, "debug", false, "Enable debug-level logging")
	fs.BoolVar(&o.probe, "probe", false,
		"Liveness probe mode: exit 0 iff the CSI socket exists, then exit (no server)")
	return fs
}

func parseFlags(args []string) (*options, error) {
	o := &options{}
	fs := newFlagSet(o)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if o.csiSocket == "" {
		o.csiSocket = udscsi.DefaultCSISocket(o.kubeletRoot)
	}
	if o.registrationSocket == "" {
		o.registrationSocket = udscsi.DefaultRegistrationSocket(o.kubeletRoot)
	}
	return o, nil
}

func run(ctx context.Context, args []string) error {
	o, err := parseFlags(args)
	if err != nil {
		return err
	}
	if o.probe {
		return udscsi.Probe(o.csiSocket)
	}

	logger := log.Named(log.NewLogger(o.debug), "uds-csi")
	size, err := udscsi.ParseSize(o.size)
	if err != nil {
		return err
	}
	d, err := udscsi.NewDriver(udscsi.Config{
		NodeID:      o.nodeID,
		KubeletRoot: o.kubeletRoot,
		Root:        o.root,
		SizeBytes:   size,
		Inodes:      o.inodes,
		Version:     Version,
	}, udscsi.NewMounter(), logger)
	if err != nil {
		return err
	}
	logger.Info("starting", "version", Version, "node_id", o.nodeID, "root", o.root,
		"size_bytes", size, "inodes", o.inodes, "kubelet_root", o.kubeletRoot)
	return udscsi.Serve(ctx, udscsi.ServeConfig{
		CSISocket:          o.csiSocket,
		RegistrationSocket: o.registrationSocket,
	}, d, logger)
}

func main() {
	// First SIGTERM: stop serving and remove both sockets (the registration
	// socket first, so the kubelet deregisters the driver). Second: exit 1.
	ctx, stop := signals.NotifyContext(context.Background())
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "uds-csi:", err)
		os.Exit(1)
	}
}
