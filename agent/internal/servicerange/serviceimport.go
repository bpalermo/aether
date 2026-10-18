package servicerange

import (
	"context"
	"net/netip"
	"slices"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// The MCS API (multicluster.x-k8s.io) leaves the allocation of a ClusterSetIP
// to the implementation, so a ServiceImport's VIP can lie outside every
// ServiceCIDR block. Each such address joins the range as a /32, so that a
// connection to it is handled like one to a ClusterIP.

// ImportReconciler watches the cluster's MCS ServiceImports and hands the sink
// the range again whenever the set of their IPv4 ClusterSetIPs changes.
// Level-based like Reconciler: each reconcile re-lists, so a deleted import
// takes its /32 with it.
type ImportReconciler struct {
	client.Client

	Sink  Sink
	state *state
}

// Reconcile re-lists every ServiceImport and replaces the ClusterSetIP part of
// the range.
func (r *ImportReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	list := &mcsv1alpha1.ServiceImportList{}
	if err := r.List(ctx, list); err != nil {
		return reconcile.Result{}, err
	}
	r.state.setClusterSetIPs(ctx, r.Sink, ClusterSetIPs(list.Items))
	return reconcile.Result{}, nil
}

// ClusterSetIPs returns the IPv4 addresses in spec.ips of the ClusterSetIP
// ServiceImports among items, sorted and without duplicates; nil when there is
// none. Skipped: the imports aether's registrar materializes (their VIP is the
// ClusterIP of a mesh Service, already a mesh destination), Headless imports
// (they have no VIP), and an entry that is not an IPv4 address.
func ClusterSetIPs(items []mcsv1alpha1.ServiceImport) []netip.Addr {
	var out []netip.Addr
	for i := range items {
		si := &items[i]
		if si.Labels[aetherlabels.LabelManagedServiceImport] == "true" {
			continue
		}
		if si.Spec.Type != mcsv1alpha1.ClusterSetIP {
			continue
		}
		for _, s := range si.Spec.IPs {
			a, err := netip.ParseAddr(s)
			if err != nil || !a.Is4() {
				continue
			}
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

// Combine returns the range the sink is handed: cidrs plus a /32 for each of
// ips that no prefix of cidrs contains, canonical (Canonicalize).
func Combine(cidrs []netip.Prefix, ips []netip.Addr) []netip.Prefix {
	out := slices.Clone(cidrs)
	for _, a := range ips {
		if !slices.ContainsFunc(cidrs, func(p netip.Prefix) bool { return p.Contains(a) }) {
			out = append(out, netip.PrefixFrom(a, 32))
		}
	}
	return Canonicalize(out)
}
