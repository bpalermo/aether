package server

import (
	"context"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// registerUnderAll writes endpoint under every registry key the pod holds
// (proposal 037) and returns the last error, or nil if all writes succeeded.
//
// A pod's key set is one entry per distinct L4 class across its advertised
// ports. Every pod written before proposal 037 holds exactly one key — an
// unsuffixed port inherits the pod-level endpoint.aether.io/protocol — so this
// loops once and is the pre-037 path unchanged.
//
// Two properties the callers depend on:
//
//   - It does NOT stop at the first error. A failure writing one key must not
//     skip the others: the remaining listings can still be brought up to date,
//     and the next retry has less to do.
//   - It reports a PARTIAL failure as a failure. This is proposal 037 Risk 5.
//     A registry does not reject a half-written dual-protocol pod, so nothing
//     but this return value distinguishes "both keys updated" from "one key
//     updated and the other silently stale". A caller that banks a partial
//     success as done — the liveness promotion path records the transition and
//     then sees prev == want forever after — leaves the other listing's
//     endpoint wrong permanently, with no error raised anywhere and a cluster
//     that merely looks empty.
//
// The caller supplies ctx, and it must be ONE deadline covering all writes:
// several of these run with lifecycleMu held (S20, #772), where a per-key
// timeout would let a dual-protocol pod multiply the worst-case lock hold that
// the bound exists to cap.
func (s *CNIServer) registerUnderAll(
	ctx context.Context,
	serviceName string,
	protocols []registryv1.Service_Protocol,
	endpoint *registryv1.ServiceEndpoint,
) error {
	var err error
	for _, protocol := range protocols {
		if rErr := s.registry.RegisterEndpoint(ctx, serviceName, protocol, endpoint); rErr != nil {
			err = rErr
		}
	}
	return err
}
