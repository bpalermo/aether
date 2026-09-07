// Package identity holds the node agent's late-bound workload identity facts —
// the ones that used to be resolved synchronously on the startup path and are
// now folded in whenever SPIRE gets around to issuing an SVID (issue #740).
package identity

import "sync/atomic"

// TrustDomain is a concurrency-safe holder for the SPIFFE trust domain the
// agent programs into Envoy (SDS resource names, SPIFFE IDs, peer validation).
//
// It exists because the trust domain is no longer knowable at wiring time. It
// used to come from the first SVID, which the process blocked on before starting
// anything; now the process wires everything up first and the SVID arrives
// later. The holder is seeded with the mesh domain — the only value that can be
// right, since peer authorization is scoped to it unconditionally and aether
// treats addressing (<svc>.<mesh-domain>) and identity (spiffe://<mesh-domain>/…)
// as one domain by design — and is reconciled against the real SVID the moment
// one lands.
//
// The zero value is usable and reads as "".
type TrustDomain struct {
	v atomic.Pointer[string]
}

// NewTrustDomain returns a holder seeded with the given trust domain.
func NewTrustDomain(seed string) *TrustDomain {
	t := &TrustDomain{}
	t.v.Store(&seed)
	return t
}

// Get returns the current trust domain.
func (t *TrustDomain) Get() string {
	if v := t.v.Load(); v != nil {
		return *v
	}
	return ""
}

// Set stores td and reports whether it DIFFERS from what was held before.
//
// The boolean is the point: a changed trust domain means every resource named
// from the seeded value is wrong and has to be rebuilt, which is a WARN-worthy
// misconfiguration (SPIRE issuing into a trust domain other than the mesh
// domain), while the overwhelmingly common "SPIRE confirmed the seed" case must
// stay silent and do no work.
func (t *TrustDomain) Set(td string) bool {
	prev := t.v.Swap(&td)
	return prev == nil || *prev != td
}
