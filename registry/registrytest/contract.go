// Package registrytest provides shared assertions every Registry backend must
// satisfy, so the implementations (kubernetes, etcd, dynamodb, registrar) stay
// consistent. The keying format is centralized in common/serviceref and the write
// path qualifies once in registry/cni.go; this contract guards against a backend
// (especially a read-only one that DERIVES keys, like the kubernetes backend)
// silently diverging to a bare or cross-namespace key.
package registrytest

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/serviceref"
)

// RequireNamespaceQualifiedKeys asserts the contract that ListAllEndpoints output
// must satisfy regardless of backend:
//
//  1. every service key is a namespace-qualified "<ns>/<sa>" (serviceref.ParseKey
//     accepts it) — a bare ServiceAccount key is rejected by every downstream
//     consumer (the VIP generator, the agent dependency set), so it is a bug;
//  2. every endpoint listed under a key whose KubernetesMetadata carries a
//     namespace agrees with the key's namespace — catches the collision where
//     same-named ServiceAccounts in different namespaces merge under one key.
//
// Call it from each backend's ListAllEndpoints test (the kubernetes unit test and
// the etcd/dynamodb integration tests) so the keying can't drift.
func RequireNamespaceQualifiedKeys(t testing.TB, services map[string][]*registryv1.ServiceEndpoint) {
	t.Helper()
	for key, eps := range services {
		ref, ok := serviceref.ParseKey(key)
		if !ok {
			t.Errorf("service key %q is not namespace-qualified <ns>/<sa> (a backend must key like serviceref/cni.go)", key)
			continue
		}
		for _, ep := range eps {
			epNS := ep.GetKubernetesMetadata().GetNamespace()
			if epNS != "" && epNS != ref.Namespace {
				t.Errorf("service key %q has an endpoint in namespace %q — same-named ServiceAccounts must not merge across namespaces", key, epNS)
			}
		}
	}
}
