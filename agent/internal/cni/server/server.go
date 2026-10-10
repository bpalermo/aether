package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aethermesh.dev/agent/internal/cniconflist"
	"aethermesh.dev/agent/internal/spire"
	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/grpcserver"
	commonlog "aethermesh.dev/common/log"
	"aethermesh.dev/common/telemetry"
	"aethermesh.dev/registry"
	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNIServer is a gRPC server that implements the CNI plugin interface.
// It handles pod registration and deregistration requests, stores pod data locally,
// and registers service endpoints in the registry. The server also queries Kubernetes
// node metadata (region and zone) for topology-aware routing.
//
// CNIServer embeds grpcserver.Server and implements the ServerCallback interface to query
// node metadata before accepting client connections.
type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer
	grpcserver.Server

	log *slog.Logger

	clusterName string
	nodeName    string
	trustDomain string
	nodeRegion  string
	nodeZone    string
	nodeIP      string

	storage  storage.Storage[*cniv1.CNIPod]
	registry registry.Registry

	// lifecycleMu serializes pod removal (unregister + storage delete) against the
	// liveness loop's health re-registrations. Without it, the 5s liveness tick can
	// observe a pod after RemovePod unregistered its endpoint but before the pod
	// left storage, and re-register it — a permanent ghost endpoint in the registry
	// that nothing unregisters again.
	lifecycleMu sync.Mutex

	// livenessForget collects container IDs whose cached liveness state must be
	// dropped (the reconciler re-registered their endpoint at the mode-default
	// health, so the next observation must be treated as a transition).
	// Consumed at the start of each liveness tick.
	livenessForgetMu sync.Mutex
	livenessForget   map[string]struct{}

	snapshotCache podSnapshots
	spireBridge   *spire.Bridge
	identityWatch IdentityWatch
	// ackTracker confirms (and diagnoses) Envoy's delta-xDS ACK/NACK of pod
	// listener updates; healthClient probes the proxy's health gateway
	// listener for the liveness loop. The agent makes no Envoy admin calls.
	ackTracker   *ack.Tracker
	healthClient *healthGatewayClient
	metrics      *cniMetrics

	k8sClient client.Client
	// informers is the manager's (node-scoped) informer cache, used by the
	// termination watch to observe pod deletionTimestamp transitions. Nil
	// disables the watch.
	informers ctrlcache.Informers

	// drainPoolCloseDelay separates the two drain phases (see
	// schedulePoolClose); overridable in tests.
	drainPoolCloseDelay time.Duration

	// netnsFailStreaks counts, per container ID, consecutive ghost-sweep passes
	// whose netns-liveness check failed. A stored pod is classified a
	// stale-netns ghost only after ghostNetnsFailThreshold consecutive failures
	// (hysteresis), so a transient fleet-wide stat failure — the 2026-07-19
	// power-blip signature — cannot mass-prune live pods (#566). Reset the moment
	// a netns check passes or the pod leaves storage. Accessed only under
	// lifecycleMu (the sweep holds it across the prune).
	netnsFailStreaks map[string]int

	// missingStorageStreaks counts, per namespace/name, consecutive ghost-sweep
	// passes a live mesh pod was found missing from local storage (a lost CNI
	// ADD). After lostAddEvictThreshold passes the agent evicts the pod to force
	// sandbox recreation and a fresh CNI ADD (#567). Reset when the pod registers
	// or disappears. Accessed only under lifecycleMu.
	missingStorageStreaks map[string]int

	// staleRunningStreaks counts, per container ID, consecutive ghost-sweep
	// passes a stored entry's netns was gone while the API reported its pod
	// Running with an IP and no fresher entry covered the pod — the node-reboot
	// CNI boot race (#640): the pod is up on the base CNI with no mesh
	// interception. After staleRunningEvictThreshold passes the agent evicts the
	// pod to force a fresh CNI ADD. Reset on any state change. Accessed only
	// under lifecycleMu.
	staleRunningStreaks map[string]int

	// pruneBreakerEngaged and pruneBreakerTrippedPasses track the mass-delete
	// circuit breaker across sweep passes so its ERROR line can be loud on the
	// transition into (and out of) the tripped state and rate-limited while it
	// stands (#670: it logged every pass on every node for weeks unnoticed).
	// Accessed only under lifecycleMu.
	pruneBreakerEngaged       bool
	pruneBreakerTrippedPasses int

	// evictPod evicts a pod via the Kubernetes Eviction API (policy/v1,
	// PDB-respecting). Overridable in tests (the fake client has no eviction
	// subresource). Nil disables self-heal eviction.
	evictPod func(ctx context.Context, namespace, name string) error

	// chain reports whether aether is chained in the node's active CNI conflist,
	// which is what decides whether self-heal eviction can heal anything at all
	// (see unchained). Set via SetChainState; nil disables the interlock.
	chain cniconflist.ChainState

	// ownership, when set, holds the socket bind and every node-writing loop
	// (liveness, termination drain, ghost sweep) until this agent owns its
	// node (proposal 041). Set via SetOwnership; nil means owned from the
	// start.
	ownership Ownership
}

// Ownership is this agent's claim on its node as the CNI server needs to see
// it (proposal 041). *ownership.Node implements it.
type Ownership interface {
	// WaitOwned blocks until this agent owns the node, or returns ctx's error.
	WaitOwned(ctx context.Context) error
}

// SetOwnership makes the server a standby until this agent owns its node: it
// binds cni.sock only then, and only then starts the loops that write the
// registry, local storage or the Kubernetes API. Before that the agent that
// owns the node serves every CNI ADD/DEL, and a standby answering one would
// wait for an ACK from a proxy that is not its client (envoyAckTimeout) and
// start the pod with no listener. What that agent did meanwhile is picked up
// by ReconcileStorage, a takeover step that runs before ownership is announced.
//
// Only the SPIRE resubscription of stored pods runs before ownership: client
// certificates are a first-serve gate (#1103), and two Broker subscriptions
// for one pod are two streams of the same SVID.
func (s *CNIServer) SetOwnership(o Ownership) {
	s.ownership = o
	if o != nil {
		s.SetBindGate(o.WaitOwned)
	}
}

// SetChainState wires the CNI conflist chaining state into the ghost sweep's
// eviction interlock (#667). A setter rather than a 13th positional constructor
// argument, deliberately: the re-asserter and the CNI server are built
// independently in root.go and this is an optional interlock, not a dependency
// the server needs to exist. Same shape as SnapshotCache.SetMeshDNSSnapshotPath.
func (s *CNIServer) SetChainState(cs cniconflist.ChainState) { s.chain = cs }

// IdentityWatch is this agent's own mesh identity as the CNI server needs to see
// it: whether it is here yet. commonspire.WaitingSource implements it.
type IdentityWatch interface {
	HasSVID() bool
}

// SetIdentityWatch lets the server tell a registry call that failed because this
// agent has no SVID yet — it cannot complete a handshake with the registrar, the
// designed #740 wait — from a real registry fault when it logs (#766). Log
// severity only; nil (the default) means "identity is never pending".
func (s *CNIServer) SetIdentityWatch(w IdentityWatch) { s.identityWatch = w }

// identityPending reports whether this agent is still waiting for its first SVID.
func (s *CNIServer) identityPending() bool {
	return s.identityWatch != nil && !s.identityWatch.HasSVID()
}

// unchained reports whether this node is known to be UNABLE to mesh a new pod
// because aether is not chained in its active CNI conflist.
//
// It fails OPEN on purpose — nil ChainState (the re-assert loop's kill switch)
// and not-yet-observed both return false, leaving eviction enabled. This is the
// opposite of the taint gate's choice, and for a good reason: holding a taint
// costs a requeue, whereas suppressing eviction here disables the #567/#640
// self-heal that recovers a node after a reboot. Withholding that on a guess
// would trade a known-good recovery for an unproven one.
func (s *CNIServer) unchained() bool {
	if s.chain == nil {
		return false
	}
	st := s.chain.ChainStatus()
	return st.Observed && !st.Chained
}

var _ grpcserver.ServerCallback = (*CNIServer)(nil)

// NewCNIServer creates a new CNI gRPC server.
// The server listens on a Unix domain socket and registers the CNI service with
// protovalidate middleware for request validation.
func NewCNIServer(clusterName string, nodeName string, trustDomain string, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry, snapshotCache *cache.SnapshotCache, ackTracker *ack.Tracker, spireBridge *spire.Bridge, log *slog.Logger, k8sClient client.Client, informers ctrlcache.Informers, cfg *CNIServerConfig) (*CNIServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
		// The TracerProvider is always installed, so this records real RPC spans (whose
		// trace_id flows to logs); spans export only with --trace-export.
		grpc.StatsHandler(telemetry.ServerStatsHandler()),
	)

	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the server.
	metrics, err := newCNIMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create CNI server metrics; continuing without instrumentation", "error", err)
	}

	cniSrv := &CNIServer{
		drainPoolCloseDelay: drainPoolCloseDelay,
		Server:              grpcserver.NewServer(grpcserver.NewServerConfig(grpcserver.WithUDS(cfg.SocketPath)), log, grpcserver.WithGRPCServer(grpcServer)),
		log:                 commonlog.Named(log, "cni"),
		metrics:             metrics,
		clusterName:         clusterName,
		nodeName:            nodeName,
		trustDomain:         trustDomain,
		storage:             localStorage,
		registry:            registry,
		k8sClient:           k8sClient,
		informers:           informers,
		snapshotCache:       snapshotCache,
		ackTracker:          ackTracker,
		spireBridge:         spireBridge,
		healthClient:        newHealthGatewayClient(cfg.ProxyHealthSocketPath),
	}
	// Self-heal lost-CNI-ADD pods via the Eviction API (#567): PDB-respecting, so
	// a real deploy's disruption budget still applies. Nil client => no eviction.
	if k8sClient != nil {
		cniSrv.evictPod = func(ctx context.Context, namespace, name string) error {
			return k8sClient.SubResource("eviction").Create(ctx,
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}},
				&policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
		}
	}

	cniSrv.AddCallback(cniSrv)

	cniv1.RegisterCNIServiceServer(grpcServer, cniSrv)

	return cniSrv, nil
}

// PreListen queries Kubernetes node metadata before the server starts accepting connections.
// It retrieves the region and zone labels from the node object.
func (s *CNIServer) PreListen(ctx context.Context) error {
	s.log.DebugContext(ctx, "querying node metadata")
	region, zone, nodeIP, err := queryNodeMetadata(ctx, s.nodeName, s.k8sClient)
	if err != nil {
		return err
	}

	s.nodeRegion = region
	s.nodeZone = zone
	s.nodeIP = nodeIP

	// Locality-aware failover: the xDS cache assigns EDS priorities relative
	// to this node's locality (signals a scoped reload — the initial
	// snapshot may predate this).
	s.snapshotCache.SetNodeLocality(region, zone)

	s.log.DebugContext(ctx, "node metadata queried successfully", "region", region, "zone", zone, "nodeIP", nodeIP)

	// Restore SVID subscriptions for pods loaded from storage: subscriptions are
	// otherwise created only on CNI ADD, so an agent restart would leave existing
	// pods' workload SVIDs unsubscribed and their mTLS broken until recreation.
	// Also on a standby: the certificates are a first-serve gate (#1103).
	go s.runResubscribeStoredPods(ctx)

	go s.startNodeWriters(ctx)
	return nil
}

// startNodeWriters starts the loops that write the registry, local storage
// and the Kubernetes API on this node's behalf, once this agent owns the node
// (immediately when no ownership is wired).
func (s *CNIServer) startNodeWriters(ctx context.Context) {
	if s.ownership != nil {
		if err := s.ownership.WaitOwned(ctx); err != nil {
			return
		}
	}

	// Delegated liveness: reflect each local pod's app health (from the proxy's
	// active health check) into the registry so it is marked unhealthy in every
	// client's EDS while the app is not serving.
	go s.runLivenessLoop(ctx)

	// Early drain: deregister endpoints the moment pod deletion is requested
	// (deletionTimestamp), instead of waiting for CNI DEL after the containers
	// are already dead.
	go s.runTerminationWatch(ctx, s.informers)

	// Ghost reconciliation: deregister this node's registry endpoints that no
	// live local pod accounts for (lost CNI DELs, node churn). Prerequisite for
	// EDS health-check mode, where a HEALTHY ghost would receive traffic forever.
	go s.runGhostSweepLoop(ctx)
}

// queryNodeMetadata retrieves the topology.kubernetes.io/region and
// topology.kubernetes.io/zone labels plus the node's InternalIP from a
// Kubernetes node (each empty if absent). The InternalIP is the routable dial
// target advertised on this node's endpoints for cross-cluster consumers whose
// pod network is not routable (proposal 019 per-node east/west waypoint).
func queryNodeMetadata(ctx context.Context, nodeName string, client client.Client) (region, zone, nodeIP string, err error) {
	node := &corev1.Node{}
	if err := client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return "", "", "", fmt.Errorf("failed to get node: %w", err)
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}

	return node.Labels[aetherannotations.AnnotationKubernetesNodeTopologyRegion],
		node.Labels[aetherannotations.AnnotationKubernetesNodeTopologyZone], nodeIP, nil
}
