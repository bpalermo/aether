package install

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"
)

// otlpLookupTimeout bounds the one resolution cni-install does for the OTLP
// endpoint. The init container gates the agent (and with it the node's CNI), so
// a slow or broken cluster DNS must cost seconds, never the install.
const otlpLookupTimeout = 5 * time.Second

// hostLookup resolves a host name to its addresses (net.Resolver.LookupHost).
type hostLookup func(ctx context.Context, host string) ([]string, error)

// pinOTLPEndpoint rewrites an OTLP "host:port" endpoint whose host is a name
// into "<address>:port", resolving the name here — inside the cni-install init
// container, which runs in the agent pod (dnsPolicy ClusterFirstWithHostNet)
// and therefore resolves cluster Service names.
//
// Why (issue #950): the CNI plugin binary that consumes the endpoint is exec'd
// by the container runtime on the HOST, under the host's resolver. On Talos (and
// kind, and any node whose /etc/resolv.conf does not point at cluster DNS) the
// collector's "<svc>.<ns>.svc.cluster.local" never resolves, every flush fails
// with "name resolver error: produced zero addresses", and no aether_cni_*
// series ever reaches the collector. A ClusterIP is reachable from the host
// network namespace (kube-proxy programs it there) and is stable for the
// Service's lifetime; a Service that is deleted and recreated gets a new one,
// which the next agent start (cni-install re-renders the entry) picks up.
//
// The transport is unchanged: the plugin already dials the endpoint with
// insecure (plaintext) gRPC, so there is no TLS server name to preserve, and the
// :authority becomes the address, which the collector's OTLP receiver ignores.
//
// Anything that is not a resolvable name is returned unchanged: an empty
// endpoint (telemetry off), an address, a value without a port, and a name that
// fails to resolve or resolves to nothing — the last being exactly the pre-#950
// behaviour, and correct on nodes whose host resolver does know the name.
//
// When the name resolves to several addresses (dual-stack, or a headless
// Service), the lowest is chosen, IPv4 before IPv6, so the choice never depends
// on answer order and repeated installs chain byte-identical entries.
func pinOTLPEndpoint(ctx context.Context, logger *slog.Logger, endpoint string, lookup hostLookup) string {
	if endpoint == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		logger.WarnContext(ctx, "OTLP endpoint is not host:port; writing it into the CNI config unchanged", "endpoint", endpoint, "error", err)
		return endpoint
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return endpoint
	}

	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	lookupCtx, cancel := context.WithTimeout(ctx, otlpLookupTimeout)
	defer cancel()
	answers, err := lookup(lookupCtx, host)
	if err != nil {
		logger.WarnContext(ctx, "could not resolve the OTLP endpoint; writing the name into the CNI config, which the plugin can only use if the HOST resolver knows it",
			"endpoint", endpoint, "error", err)
		return endpoint
	}

	addrs := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		if addr, err := netip.ParseAddr(a); err == nil {
			addrs = append(addrs, addr.Unmap())
		}
	}
	if len(addrs) == 0 {
		logger.WarnContext(ctx, "the OTLP endpoint resolved to no addresses; writing the name into the CNI config", "endpoint", endpoint)
		return endpoint
	}
	// netip.Addr.Compare orders by address family first (IPv4 < IPv6).
	slices.SortFunc(addrs, netip.Addr.Compare)
	pinned := net.JoinHostPort(addrs[0].String(), port)

	if len(addrs) > 1 {
		logger.WarnContext(ctx, "the OTLP endpoint resolved to several addresses; pinning the lowest (a headless Service's pod IPs are NOT stable — point the CNI at a ClusterIP Service)",
			"endpoint", endpoint, "addresses", answers, "pinned", pinned)
	} else {
		logger.InfoContext(ctx, "pinned the OTLP endpoint to its resolved address so the CNI plugin can dial it without cluster DNS",
			"endpoint", endpoint, "pinned", pinned)
	}
	return pinned
}
