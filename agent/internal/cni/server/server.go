package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/ack"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	aetherannotations "github.com/bpalermo/aether/common/constants/annotations"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/common/xds"
	"github.com/bpalermo/aether/registry"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CNIServer is a gRPC server that implements the CNI plugin interface.
// It handles pod registration and deregistration requests, stores pod data locally,
// and registers service endpoints in the registry. The server also queries Kubernetes
// node metadata (region and zone) for topology-aware routing.
//
// CNIServer embeds xds.Server and implements the ServerCallback interface to query
// node metadata before accepting client connections.
type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer
	xds.Server

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

	snapshotCache *cache.SnapshotCache
	spireBridge   *spire.Bridge
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
}

var _ xds.ServerCallback = (*CNIServer)(nil)

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
		Server:              xds.NewServer(xds.NewServerConfig(xds.WithUDS(cfg.SocketPath)), log, xds.WithGRPCServer(grpcServer)),
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

	// Delegated liveness: reflect each local pod's app health (from the proxy's
	// active health check) into the registry so it is marked unhealthy in every
	// client's EDS while the app is not serving.
	go s.runLivenessLoop(ctx)

	// Restore SVID subscriptions for pods loaded from storage: subscriptions are
	// otherwise created only on CNI ADD, so an agent restart would leave existing
	// pods' workload SVIDs unsubscribed and their mTLS broken until recreation.
	go s.runResubscribeStoredPods(ctx)

	// Early drain: deregister endpoints the moment pod deletion is requested
	// (deletionTimestamp), instead of waiting for CNI DEL after the containers
	// are already dead.
	go s.runTerminationWatch(ctx, s.informers)

	// Ghost reconciliation: deregister this node's registry endpoints that no
	// live local pod accounts for (lost CNI DELs, node churn). Prerequisite for
	// EDS health-check mode, where a HEALTHY ghost would receive traffic forever.
	go s.runGhostSweepLoop(ctx)
	return nil
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
