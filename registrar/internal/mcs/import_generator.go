package mcs

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	commonlog "github.com/bpalermo/aether/common/log"
	"github.com/bpalermo/aether/registry"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// ImportGenerator reconciles the registry's clusterset-wide export view into, in
// THIS cluster: a ServiceImport (type ClusterSetIP) per exported service, and a
// LOCAL selectorless clusterset VIP Service — mirroring the registrar's
// mesh-Service generator. No EndpointSlice import: endpoints stay in the registry,
// and the proxy resolves cross-cluster endpoints via registry EDS at dial time
// (a later phase). Each clusterset Service is annotated with the mesh service +
// mesh port so the agent maps the captured ClusterIP back to the registry-backed
// EDS cluster, exactly like a per-cluster mesh VIP.
//
// The clusterset VIP Service has a DISTINCT name (<svc>-clusterset) so it
// coexists with the same-named per-cluster mesh Service (<svc>): the mesh VIP
// backs <svc>.<ns>.svc.cluster.local, the clusterset VIP its own ClusterIP. The
// generator copies the K8s-allocated ClusterIP of the clusterset Service onto the
// ServiceImport's spec.IPs, which is what an MCS DNS controller publishes for
// <svc>.<ns>.svc.clusterset.local.
//
// It is a leader-elected manager Runnable: only the leader writes. It reads the
// export view on a ticker (the etcd registry's view changes when any cluster's
// ExportController writes a mark).
type ImportGenerator struct {
	client.Client
	// Exporter is the registry's cross-cluster export capability. Required.
	Exporter registry.ServiceExporter
	// MeshPort is the clusterset VIP Service port (the mesh capture port).
	MeshPort int32
	Interval time.Duration
	Log      *slog.Logger
}

// NeedLeaderElection makes the generator run only on the elected leader.
func (g *ImportGenerator) NeedLeaderElection() bool { return true }

// Start runs the reconcile loop until the context is cancelled.
func (g *ImportGenerator) Start(ctx context.Context) error {
	g.Log = commonlog.Named(g.Log, "mcs-import-generator")
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	g.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			g.reconcile(ctx)
		}
	}
}

// desiredImport is one materialized clusterset service: a ServiceImport + a
// selectorless clusterset VIP Service, keyed by (namespace, service).
type desiredImport struct {
	service   string
	namespace string
	clusters  []string
}

// reconcile makes the managed ServiceImports + clusterset Services equal the
// registry's clusterset-wide export view: create missing, update drifted, prune
// stale (only objects this generator owns, by label).
func (g *ImportGenerator) reconcile(ctx context.Context) {
	exports, err := g.Exporter.ListExports(ctx)
	if err != nil {
		g.Log.ErrorContext(ctx, "list registry exports failed", "error", err)
		return
	}

	desired := map[client.ObjectKey]*desiredImport{}
	for _, e := range exports {
		if e.Service == "" || e.Namespace == "" {
			continue
		}
		key := client.ObjectKey{Namespace: e.Namespace, Name: e.Service}
		d := desired[key]
		if d == nil {
			d = &desiredImport{service: e.Service, namespace: e.Namespace}
			desired[key] = d
		}
		if e.Cluster != "" {
			d.clusters = append(d.clusters, e.Cluster)
		}
	}
	for _, d := range desired {
		sort.Strings(d.clusters)
		d.clusters = dedupe(d.clusters)
	}

	g.pruneServiceImports(ctx, desired)
	g.pruneClustersetServices(ctx, desired)
	for _, d := range desired {
		// Apply the clusterset VIP Service FIRST so K8s allocates its ClusterIP,
		// then stamp that IP onto the ServiceImport (the MCS ClusterSetIP VIP).
		vip := g.applyClustersetService(ctx, d)
		g.applyServiceImport(ctx, d, vip)
	}
}

// clustersetServiceName is the distinct name of the generated clusterset VIP
// Service for a mesh service: <svc>-clusterset. A separate object (its own
// ClusterIP) from the same-named per-cluster mesh Service, so the two coexist.
func clustersetServiceName(service string) string {
	return service + aetherlabels.ClustersetServiceNameSuffix
}

// pruneServiceImports deletes managed ServiceImports whose service no longer has
// any export in the clusterset.
func (g *ImportGenerator) pruneServiceImports(ctx context.Context, desired map[client.ObjectKey]*desiredImport) {
	var managed mcsv1alpha1.ServiceImportList
	if err := g.List(ctx, &managed, client.MatchingLabels{aetherlabels.LabelManagedServiceImport: "true"}); err != nil {
		g.Log.ErrorContext(ctx, "list managed ServiceImports failed", "error", err)
		return
	}
	for i := range managed.Items {
		s := &managed.Items[i]
		key := client.ObjectKeyFromObject(s)
		if _, ok := desired[key]; ok {
			continue
		}
		if err := g.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			g.Log.ErrorContext(ctx, "prune ServiceImport failed", "import", key.String(), "error", err)
		} else {
			g.Log.InfoContext(ctx, "pruned ServiceImport (export gone clusterset-wide)", "import", key.String())
		}
	}
}

// pruneClustersetServices deletes managed clusterset VIP Services with no export.
func (g *ImportGenerator) pruneClustersetServices(ctx context.Context, desired map[client.ObjectKey]*desiredImport) {
	var managed corev1.ServiceList
	if err := g.List(ctx, &managed, client.MatchingLabels{aetherlabels.LabelClustersetService: "true"}); err != nil {
		g.Log.ErrorContext(ctx, "list managed clusterset Services failed", "error", err)
		return
	}
	for i := range managed.Items {
		s := &managed.Items[i]
		key := client.ObjectKeyFromObject(s)
		// The clusterset Service name is <svc>-clusterset; the import key is keyed
		// by the mesh service name, recorded on the Service's clusterset-import
		// annotation (fall back to trimming the suffix off the name).
		importName := s.Annotations[aetherlabels.AnnotationClustersetImport]
		if importName == "" {
			importName = strings.TrimSuffix(s.Name, aetherlabels.ClustersetServiceNameSuffix)
		}
		importKey := client.ObjectKey{Namespace: s.Namespace, Name: importName}
		if _, ok := desired[importKey]; ok {
			continue
		}
		if err := g.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			g.Log.ErrorContext(ctx, "prune clusterset Service failed", "service", key.String(), "error", err)
		} else {
			g.Log.InfoContext(ctx, "pruned clusterset Service (export gone clusterset-wide)", "service", key.String())
		}
	}
}

// applyServiceImport creates or updates the ServiceImport (ClusterSetIP) for one
// exported service. vip is the K8s-allocated ClusterIP of the clusterset VIP
// Service (the MCS ClusterSetIP), set on spec.IPs; empty if not yet allocated.
// It never touches a ServiceImport it does not own.
func (g *ImportGenerator) applyServiceImport(ctx context.Context, d *desiredImport, vip string) {
	key := client.ObjectKey{Namespace: d.namespace, Name: d.service}
	clusters := make([]mcsv1alpha1.ClusterStatus, 0, len(d.clusters))
	for _, c := range d.clusters {
		clusters = append(clusters, mcsv1alpha1.ClusterStatus{Cluster: c})
	}
	ips := clustersetIPs(vip)

	existing := &mcsv1alpha1.ServiceImport{}
	if err := g.Get(ctx, key, existing); err == nil {
		if existing.Labels[aetherlabels.LabelManagedServiceImport] != "true" {
			g.Log.WarnContext(ctx, "a non-aether ServiceImport owns this name; skipping", "import", key.String())
			return
		}
		existing.Spec.Type = mcsv1alpha1.ClusterSetIP
		existing.Spec.Ports = g.importPorts()
		existing.Spec.IPs = ips
		if err := g.Update(ctx, existing); err != nil {
			g.Log.ErrorContext(ctx, "update ServiceImport failed", "import", key.String(), "error", err)
			return
		}
		g.setImportClusters(ctx, existing, clusters)
		return
	} else if !apierrors.IsNotFound(err) {
		g.Log.ErrorContext(ctx, "get ServiceImport failed", "import", key.String(), "error", err)
		return
	}

	si := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.service,
			Namespace: d.namespace,
			Labels:    map[string]string{aetherlabels.LabelManagedServiceImport: "true"},
		},
		Spec: mcsv1alpha1.ServiceImportSpec{
			Type:  mcsv1alpha1.ClusterSetIP,
			Ports: g.importPorts(),
			IPs:   ips,
		},
	}
	if err := g.Create(ctx, si); err != nil && !apierrors.IsAlreadyExists(err) {
		g.Log.ErrorContext(ctx, "create ServiceImport failed", "import", key.String(), "error", err)
		return
	}
	g.Log.InfoContext(ctx, "created ServiceImport (ClusterSetIP)", "import", key.String(), "clusters", d.clusters, "vip", vip)
	g.setImportClusters(ctx, si, clusters)
}

// clustersetIPs is the ServiceImport spec.IPs for a clusterset VIP. The MCS API
// caps IPs at 2 entries; a single ClusterIP is the common case. A headless/none
// ClusterIP yields no spec.IPs (the import is published but addressless).
func clustersetIPs(vip string) []string {
	if vip == "" || vip == corev1.ClusterIPNone {
		return nil
	}
	return []string{vip}
}

// setImportClusters records the exporting clusters on the ServiceImport status.
// Best-effort: a write failure is logged, not surfaced.
func (g *ImportGenerator) setImportClusters(ctx context.Context, si *mcsv1alpha1.ServiceImport, clusters []mcsv1alpha1.ClusterStatus) {
	si.Status.Clusters = clusters
	if err := g.Status().Update(ctx, si); err != nil {
		g.Log.WarnContext(ctx, "update ServiceImport status failed", "import", client.ObjectKeyFromObject(si).String(), "error", err)
	}
}

// importPorts is the ServiceImport's advertised port set: the mesh capture port.
// Phase 1 advertises the single mesh port; per-app-port import is a later phase
// (the registry already carries per-port endpoints, proposal 005).
func (g *ImportGenerator) importPorts() []mcsv1alpha1.ServicePort {
	return []mcsv1alpha1.ServicePort{{
		Name:     "mesh",
		Protocol: corev1.ProtocolTCP,
		Port:     g.MeshPort,
	}}
}

// applyClustersetService creates or updates the selectorless clusterset VIP
// Service for one exported service and returns its K8s-allocated ClusterIP
// (empty if unallocated). It has a DISTINCT name (<svc>-clusterset) so it
// coexists with the same-named per-cluster mesh Service — a separate ClusterIP
// backing the ServiceImport's <svc>.<ns>.svc.clusterset.local address. Mirrors
// the mesh-Service generator: a pure ClusterIP + name handle, endpoints stay in
// the registry. It never touches a Service it does not own.
func (g *ImportGenerator) applyClustersetService(ctx context.Context, d *desiredImport) string {
	name := clustersetServiceName(d.service)
	key := client.ObjectKey{Namespace: d.namespace, Name: name}

	existing := &corev1.Service{}
	if err := g.Get(ctx, key, existing); err == nil {
		if existing.Labels[aetherlabels.LabelClustersetService] != "true" {
			g.Log.WarnContext(ctx, "a non-aether Service owns this name; skipping clusterset VIP", "service", key.String())
			return ""
		}
		return clusterIP(existing) // converged (selectorless VIP is static)
	} else if !apierrors.IsNotFound(err) {
		g.Log.ErrorContext(ctx, "get clusterset Service failed", "service", key.String(), "error", err)
		return ""
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.namespace,
			Labels: map[string]string{
				aetherlabels.LabelClustersetService: "true",
				aetherlabels.LabelMeshService:       "true",
			},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:      d.service,
				aetherlabels.AnnotationMeshPort:         strconv.Itoa(int(g.MeshPort)),
				aetherlabels.AnnotationClustersetImport: d.service,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			// Selectorless ClusterSet VIP: its own ClusterIP + name handle, distinct
			// from the per-cluster mesh VIP. Endpoints stay in the aether registry;
			// the agent maps this ClusterIP to the registry-backed EDS cluster, which
			// resolves cross-cluster endpoints at dial time.
			Ports: []corev1.ServicePort{{
				Name:       "mesh",
				Port:       g.MeshPort,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(g.MeshPort),
			}},
		},
	}
	if err := g.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		g.Log.ErrorContext(ctx, "create clusterset Service failed", "service", key.String(), "error", err)
		return ""
	}
	// Re-read so we observe the ClusterIP the apiserver allocated on create.
	if err := g.Get(ctx, key, svc); err != nil {
		g.Log.WarnContext(ctx, "get clusterset Service after create failed", "service", key.String(), "error", err)
		return ""
	}
	vip := clusterIP(svc)
	g.Log.InfoContext(ctx, "created clusterset Service (ClusterSet VIP)", "service", key.String(), "vip", vip)
	return vip
}

// clusterIP returns the Service's allocated ClusterIP, or "" if none/unallocated
// ("None" headless or empty pending allocation).
func clusterIP(svc *corev1.Service) string {
	if svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return ""
	}
	return svc.Spec.ClusterIP
}

// dedupe returns xs with consecutive duplicates removed; xs must be sorted.
func dedupe(xs []string) []string {
	if len(xs) < 2 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
