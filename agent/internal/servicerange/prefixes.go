// Package servicerange gives the node agent the cluster's Service range: the
// address blocks Kubernetes allocates ClusterIPs from (proposal 044, Phase 0).
//
// The range comes from the cluster's networking.k8s.io/v1 ServiceCIDR objects,
// watched by a reconciler, or from the --service-cidrs override, which wins
// when set (the reconciler is then not started). Only IPv4 blocks are kept for
// now. Where the multicluster.x-k8s.io ServiceImport CRD is installed, a /32
// for each IPv4 ClusterSetIP outside those blocks joins the range too, under
// the override as well (ImportReconciler). When no ServiceCIDR range is known
// the agent logs one WARN and reports aether.agent.service_range.known = 0, and
// the later phases build no Service-range branch for ClusterIPs: every non-mesh
// destination outside the ClusterSetIP /32s stays on the passthrough.
package servicerange

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Canonicalize returns the IPv4 prefixes of in, masked to their network
// address, sorted and without duplicates. IPv6 prefixes are dropped. The
// result is nil when nothing is left, so two canonical ranges compare with
// slices.Equal.
func Canonicalize(in []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range in {
		if !p.IsValid() || !p.Addr().Is4() {
			continue
		}
		out = append(out, p.Masked())
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Bits(), b.Bits())
	})
	return slices.Compact(out)
}

// ParseCIDRs parses CIDR strings, the --service-cidrs override or a
// ServiceCIDR's spec.cidrs. Any entry that is not a CIDR is an error. It
// returns the canonical IPv4 range and how many IPv6 entries were dropped.
func ParseCIDRs(cidrs []string) (ranges []netip.Prefix, droppedIPv6 int, err error) {
	parsed := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		p, perr := netip.ParsePrefix(strings.TrimSpace(s))
		if perr != nil {
			return nil, 0, fmt.Errorf("%q is not a CIDR: %w", s, perr)
		}
		if !p.Addr().Is4() {
			droppedIPv6++
			continue
		}
		parsed = append(parsed, p)
	}
	return Canonicalize(parsed), droppedIPv6, nil
}
