package gatewayapi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/edge/secret"
	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A real self-signed ECDSA keypair (CN=test) for the listener-TLS tests: the
// kubernetes secret provider validates the keypair with tls.X509KeyPair, so a
// "valid cert" fixture must hold parseable material.
const (
	validListenerTLSCert = `-----BEGIN CERTIFICATE-----
MIIBHzCBxqADAgECAgEBMAoGCCqGSM49BAMCMA8xDTALBgNVBAMTBHRlc3QwHhcN
MjYwNjI2MTU1MzUwWhcNMjcwNjI2MTY1MzUwWjAPMQ0wCwYDVQQDEwR0ZXN0MFkw
EwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEn4alJ26H8ozmsdz28/d8BUxOcmTuHYiT
IdRPGfJR6P6CHstygFvJD1N7mvblUBSGyaJq8jI9p0eIFI+km0F5k6MTMBEwDwYD
VR0RBAgwBoIEdGVzdDAKBggqhkjOPQQDAgNIADBFAiEA8t876pZRDOkFPAoZV0gg
36yLul1OYTpOylOe9NsAyHwCIE7SBys13i6g5re4YmKgv1GBo6wW3VhxZWO6qm/Y
KTqF
-----END CERTIFICATE-----
`
	validListenerTLSKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIIsvYanqaRRY782l1DBVdbGmnaBSmptup19WIFXx2vdjoAoGCCqGSM49
AwEHoUQDQgAEn4alJ26H8ozmsdz28/d8BUxOcmTuHYiTIdRPGfJR6P6CHstygFvJ
D1N7mvblUBSGyaJq8jI9p0eIFI+km0F5kw==
-----END EC PRIVATE KEY-----
`
)

// listenerTLSGateway builds an HTTPS Gateway of our class whose single listener
// references the named Secret as its certificateRef.
func listenerTLSGateway(name, secretName string) *gatewayv1.Gateway {
	secretKind := gatewayv1.Kind("Secret")
	emptyGroup := gatewayv1.Group("")
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
				TLS: &gatewayv1.ListenerTLSConfig{
					CertificateRefs: []gatewayv1.SecretObjectReference{{
						Group: &emptyGroup, Kind: &secretKind, Name: gatewayv1.ObjectName(secretName),
					}},
				},
			}},
		},
	}
}

// tlsTypeSecret builds a kubernetes.io/tls Secret with the given cert/key bytes
// (used to construct missing-key, malformed, and valid fixtures).
func tlsTypeSecret(name, cert, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte(cert), corev1.TLSPrivateKeyKey: []byte(key)},
	}
}

// secretsRegistryFor builds a real Secrets Registry backed by the given fake
// client so the reconciler resolves (and validates) certificateRefs end-to-end.
func secretsRegistryFor(c client.Client) *secret.Registry {
	return secret.NewRegistry(secret.NewKubernetesProvider(c, "ns"))
}

func ourClass() *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
}

// TestGatewayClassObservedGeneration: the Accepted condition on the GatewayClass
// carries the class's own metadata.generation as observedGeneration (not 0), and a
// GatewayClass that bears our controllerName but a DIFFERENT name than the
// configured one is still reconciled — covering GatewayClassObservedGenerationBump.
func TestGatewayClassObservedGeneration(t *testing.T) {
	// Configured class name is "aether", but the conformance bump test creates a
	// differently-named class that still carries our controllerName.
	other := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gatewayclass-observed-generation-bump", Generation: 7},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), other).
		WithStatusSubresource(&gatewayv1.GatewayClass{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "aether-ingress", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.GatewayClass{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "gatewayclass-observed-generation-bump"}, got))
	acc := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayClassConditionStatusAccepted))
	require.NotNil(t, acc, "differently-named class with our controllerName must be reconciled")
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	assert.Equal(t, int64(7), acc.ObservedGeneration, "observedGeneration must equal the class generation, not 0")
}

// TestListenerInvalidRouteKinds: a listener whose allowedRoutes.kinds names an
// unsupported kind must publish ResolvedRefs=False/InvalidRouteKinds and an
// appropriate supportedKinds list (empty when ALL named kinds are unsupported;
// the supported subset otherwise) — covers GatewayInvalidRouteKind.
func TestListenerInvalidRouteKinds(t *testing.T) {
	gwOnlyInvalid := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-only-invalid", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{Kind: "InvalidRoute"}}},
			}},
		},
	}
	gwSupportedAndInvalid := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-supported-and-invalid", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{{Kind: "InvalidRoute"}, {Kind: "HTTPRoute"}}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), gwOnlyInvalid, gwSupportedAndInvalid).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	// Only-invalid: ResolvedRefs=False/InvalidRouteKinds, supportedKinds empty.
	got := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gw-only-invalid"}, got))
	require.Len(t, got.Status.Listeners, 1)
	rr := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	require.NotNil(t, rr)
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidRouteKinds), rr.Reason)
	assert.Empty(t, got.Status.Listeners[0].SupportedKinds, "all-invalid kinds → empty supportedKinds")

	// Supported-and-invalid: ResolvedRefs=False/InvalidRouteKinds, supportedKinds=[HTTPRoute].
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gw-supported-and-invalid"}, got))
	require.Len(t, got.Status.Listeners, 1)
	rr = meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	require.NotNil(t, rr)
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidRouteKinds), rr.Reason)
	require.Len(t, got.Status.Listeners[0].SupportedKinds, 1)
	assert.Equal(t, gatewayv1.Kind("HTTPRoute"), got.Status.Listeners[0].SupportedKinds[0].Kind)
}

// TestListenerInvalidTLS: an HTTPS listener whose certificateRef doesn't resolve
// (no Secrets provider configured) gets ResolvedRefs=False/InvalidCertificateRef
// and a NON-Programmed listener + Gateway — covers GatewayInvalidTLSConfiguration.
func TestListenerInvalidTLS(t *testing.T) {
	secretKind := gatewayv1.Kind("Secret")
	emptyGroup := gatewayv1.Group("")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-bad-tls", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
				TLS: &gatewayv1.ListenerTLSConfig{
					CertificateRefs: []gatewayv1.SecretObjectReference{{
						Group: &emptyGroup, Kind: &secretKind, Name: "nonexistent-certificate",
					}},
				},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), gw).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
		Build()
	// Secrets nil → TLS cannot resolve → InvalidCertificateRef.
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gw-bad-tls"}, got))
	require.Len(t, got.Status.Listeners, 1)
	rr := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
	require.NotNil(t, rr)
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), rr.Reason)
	prog := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionFalse, prog.Status, "listener with invalid TLS must not be Programmed")
	// supportedKinds still HTTPRoute (the kind is fine; the cert is not).
	require.Len(t, got.Status.Listeners[0].SupportedKinds, 1)
	assert.Equal(t, gatewayv1.Kind("HTTPRoute"), got.Status.Listeners[0].SupportedKinds[0].Kind)
	// Top-level Gateway Programmed must be False while a listener is broken.
	gwProg := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, gwProg)
	assert.Equal(t, metav1.ConditionFalse, gwProg.Status)

	// observedGeneration must be stamped to the Gateway's current generation on BOTH
	// the Accepted AND Programmed conditions even on the invalid-TLS path — the
	// controller must not early-return before stamping it (rev13 regression: the
	// cert-error Gateways reported observedGeneration 0 while generation was 1, so
	// GatewayInvalidTLSConfiguration failed waiting for observedGeneration==1). The
	// conformance suite waits for status.conditions[].observedGeneration ==
	// metadata.generation before asserting the condition values.
	gwAcc := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, gwAcc)
	assert.Equal(t, metav1.ConditionTrue, gwAcc.Status, "Accepted stays True even with an invalid-TLS listener")
	assert.Equal(t, got.Generation, gwAcc.ObservedGeneration, "Accepted observedGeneration must track metadata.generation on the invalid-TLS path")
	assert.Equal(t, got.Generation, gwProg.ObservedGeneration, "Programmed observedGeneration must track metadata.generation on the invalid-TLS path")
}

// TestListenerInvalidTLSWithProvider exercises the real Secrets provider on the
// listener-TLS resolution path (the GatewayInvalidTLSConfiguration conformance
// cases): a certificateRef to a missing Secret, a Secret missing tls.key, and a
// Secret of the right type whose cert/key bytes don't parse as a keypair all yield
// ResolvedRefs=False/InvalidCertificateRef + Programmed=False (observedGeneration
// stamped). A VALID keypair stays ResolvedRefs=True/Programmed=True (regression
// guard — api.palermo.dev must stay green).
func TestListenerInvalidTLSWithProvider(t *testing.T) {
	cases := []struct {
		name        string
		gwName      string
		secretName  string
		secret      *corev1.Secret // nil → no Secret created (nonexistent ref)
		wantInvalid bool
	}{
		{
			name:        "nonexistent secret",
			gwName:      "gw-tls-missing",
			secretName:  "nonexistent-certificate",
			secret:      nil,
			wantInvalid: true,
		},
		{
			name:        "secret missing tls.key",
			gwName:      "gw-tls-no-key",
			secretName:  "no-key",
			secret:      tlsTypeSecret("no-key", validListenerTLSCert, ""),
			wantInvalid: true,
		},
		{
			name:        "malformed cert/key bytes",
			gwName:      "gw-tls-malformed",
			secretName:  "malformed-certificate",
			secret:      tlsTypeSecret("malformed-certificate", "Hello world\n", "Hello world\n"),
			wantInvalid: true,
		},
		{
			name:        "valid keypair (regression)",
			gwName:      "gw-tls-valid",
			secretName:  "valid-certificate",
			secret:      tlsTypeSecret("valid-certificate", validListenerTLSCert, validListenerTLSKey),
			wantInvalid: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := listenerTLSGateway(tc.gwName, tc.secretName)
			b := fake.NewClientBuilder().WithScheme(statusScheme(t)).
				WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{})
			objs := []client.Object{ourClass(), gw}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			c := b.WithObjects(objs...).Build()
			r := &Reconciler{
				Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns",
				GatewayClassName: "aether", MeshDomain: "mesh",
				Secrets: secretsRegistryFor(c), Log: slog.Default(),
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)

			got := &gatewayv1.Gateway{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: tc.gwName}, got))
			require.Len(t, got.Status.Listeners, 1)
			rr := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionResolvedRefs))
			require.NotNil(t, rr)
			prog := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, string(gatewayv1.ListenerConditionProgrammed))
			require.NotNil(t, prog)
			gwProg := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			require.NotNil(t, gwProg)

			if tc.wantInvalid {
				assert.Equal(t, metav1.ConditionFalse, rr.Status, "invalid cert → ResolvedRefs=False")
				assert.Equal(t, string(gatewayv1.ListenerReasonInvalidCertificateRef), rr.Reason)
				assert.Equal(t, metav1.ConditionFalse, prog.Status, "invalid cert → listener not Programmed")
				assert.Equal(t, string(gatewayv1.ListenerReasonInvalid), prog.Reason)
				assert.Equal(t, metav1.ConditionFalse, gwProg.Status, "invalid cert → Gateway not Programmed")
				// observedGeneration must still be stamped on the invalid path (#361 guard).
				assert.Equal(t, got.Generation, rr.ObservedGeneration)
				assert.Equal(t, got.Generation, prog.ObservedGeneration)
			} else {
				assert.Equal(t, metav1.ConditionTrue, rr.Status, "valid cert → ResolvedRefs=True (regression)")
				assert.Equal(t, string(gatewayv1.ListenerReasonResolvedRefs), rr.Reason)
				assert.Equal(t, metav1.ConditionTrue, prog.Status, "valid cert → listener Programmed (regression)")
				assert.Equal(t, metav1.ConditionTrue, gwProg.Status, "valid cert → Gateway Programmed (regression)")
			}
		})
	}
}

// TestRouteNotMatchingSectionName: a route whose parentRef names a sectionName
// that no listener has gets Accepted=False/NoMatchingParent and is NOT counted in
// the listener attachedRoutes — covers HTTPRouteInvalidParentRefNotMatchingSectionName.
func TestRouteNotMatchingSectionName(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "same-namespace", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	badSection := gatewayv1.SectionName("http1") // no such listener
	port := gatewayv1.PortNumber(80)
	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Kind: ptr(gatewayv1.Kind("Gateway")), Name: "same-namespace",
				SectionName: &badSection, Port: &port,
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "ns", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	acc := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionFalse, acc.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingParent), acc.Reason)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "same-namespace"}, gotGW))
	require.Len(t, gotGW.Status.Listeners, 1)
	assert.Equal(t, int32(0), gotGW.Status.Listeners[0].AttachedRoutes, "non-matching sectionName must not attach")
}

// TestRouteNotAllowedByListeners: a cross-namespace route attaching to a Gateway
// whose listener allowedRoutes.namespaces is "Same" gets
// Accepted=False/NotAllowedByListeners and is not attached — covers
// HTTPRouteInvalidCrossNamespaceParentRef.
func TestRouteNotAllowedByListeners(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "same-namespace", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{
					From: ptr(gatewayv1.NamespacesFromSame),
				}},
			}},
		},
	}
	infraNs := gatewayv1.Namespace("infra")
	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "cross", Namespace: "web-backend", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Kind: ptr(gatewayv1.Kind("Gateway")), Name: "same-namespace", Namespace: &infraNs,
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "web-backend", Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), gw, hr).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "infra", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotHR := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "web-backend", Name: "cross"}, gotHR))
	require.Len(t, gotHR.Status.Parents, 1)
	acc := meta.FindStatusCondition(gotHR.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionFalse, acc.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNotAllowedByListeners), acc.Reason)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "infra", Name: "same-namespace"}, gotGW))
	require.Len(t, gotGW.Status.Listeners, 1)
	assert.Equal(t, int32(0), gotGW.Status.Listeners[0].AttachedRoutes)
}

// TestAttachedRoutesNon8080AndHostname: a non-8080 (port 80) listener with a
// hostname counts only the routes whose hostnames intersect the listener hostname
// (allowedRoutes.namespaces=Selector honored), and a mismatched-hostname route is
// Accepted=False/NoMatchingListenerHostname — covers the non-port-8080
// GatewayWithAttachedRoutes variant.
func TestAttachedRoutesNon8080AndHostname(t *testing.T) {
	infraNsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "infra",
		Labels: map[string]string{"kubernetes.io/metadata.name": "infra"},
	}}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw2", Namespace: "infra", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				Hostname: ptr(gatewayv1.Hostname("foo.example.com")),
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Kinds: []gatewayv1.RouteGroupKind{{Kind: "HTTPRoute"}},
					Namespaces: &gatewayv1.RouteNamespaces{
						From:     ptr(gatewayv1.NamespacesFromSelector),
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "infra"}},
					},
				},
			}},
		},
	}
	mkRoute := func(name string, hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra", Generation: 1},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
					Kind: ptr(gatewayv1.Kind("Gateway")), Name: "gw2",
				}}},
				Hostnames: hostnames,
				Rules: []gatewayv1.HTTPRouteRule{{
					BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{Name: "infra-backend-v1", Port: ptr(gatewayv1.PortNumber(8080))},
					}}},
				}},
			},
		}
	}
	r2 := mkRoute("http-route-2") // no hostname → intersects
	r3 := mkRoute("http-route-3") // no hostname → intersects
	rNA := mkRoute("http-route-not-accepted", "not-accepted.test.com")

	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(ourClass(), infraNsObj, gw, r2, r3, rNA).
		WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: statusFakeSink{}, Namespace: "infra", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default()}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	gotGW := &gatewayv1.Gateway{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "infra", Name: "gw2"}, gotGW))
	require.Len(t, gotGW.Status.Listeners, 1)
	assert.Equal(t, int32(2), gotGW.Status.Listeners[0].AttachedRoutes,
		"port-80 listener with hostname must count only the 2 intersecting routes")

	gotNA := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "infra", Name: "http-route-not-accepted"}, gotNA))
	require.Len(t, gotNA.Status.Parents, 1)
	acc := meta.FindStatusCondition(gotNA.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionFalse, acc.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingListenerHostname), acc.Reason)
}
