package gatewayapi

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/gatewaystatus"
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

// infraGateway is a one-listener Gateway carrying the given spec.infrastructure
// labels and annotations (nil, nil = no spec.infrastructure at all).
func infraGateway(labels, annotations map[string]string) gatewayv1.Gateway {
	gw := gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "aether",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	if labels == nil && annotations == nil {
		return gw
	}
	inf := &gatewayv1.GatewayInfrastructure{}
	if labels != nil {
		inf.Labels = map[gatewayv1.LabelKey]gatewayv1.LabelValue{}
		for k, v := range labels {
			inf.Labels[gatewayv1.LabelKey(k)] = gatewayv1.LabelValue(v)
		}
	}
	if annotations != nil {
		inf.Annotations = map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{}
		for k, v := range annotations {
			inf.Annotations[gatewayv1.AnnotationKey(k)] = gatewayv1.AnnotationValue(v)
		}
	}
	gw.Spec.Infrastructure = inf
	return gw
}

// infraHarness reconciles the per-Gateway Service of one Gateway against a fake
// API server, the way the edge reconciler does on every pass.
type infraHarness struct {
	t   *testing.T
	c   client.Client
	r   *Reconciler
	log *bytes.Buffer
}

func newInfraHarness(t *testing.T) *infraHarness {
	t.Helper()
	buf := &bytes.Buffer{}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).Build()
	return &infraHarness{
		t: t, c: c, log: buf,
		r: &Reconciler{
			Client: c, Namespace: "aether-ingress", EdgeServiceName: "aether-edge",
			Log: slog.New(slog.NewTextHandler(buf, nil)),
		},
	}
}

func (h *infraHarness) key() types.NamespacedName {
	return types.NamespacedName{Namespace: "aether-ingress", Name: gatewayServiceName("aether-edge", "apps", "web")}
}

// reconcile runs one pass for gw and returns the resulting Service.
func (h *infraHarness) reconcile(gw gatewayv1.Gateway) *corev1.Service {
	h.t.Helper()
	gws := []gatewayv1.Gateway{gw}
	allocs, err := allocateGatewayListenerPorts(gws, nil)
	require.NoError(h.t, err)
	_, err = h.r.reconcileGatewayServices(context.Background(), gws, allocs, nil)
	require.NoError(h.t, err)
	return h.service()
}

func (h *infraHarness) service() *corev1.Service {
	h.t.Helper()
	got := &corev1.Service{}
	require.NoError(h.t, h.c.Get(context.Background(), h.key(), got))
	return got
}

// TestGatewayInfrastructure_NoInfrastructureUnchanged: a Gateway without
// spec.infrastructure gets exactly the Service it got before the feature
// existed: aether's own label, no annotation, and no bookkeeping annotation.
func TestGatewayInfrastructure_NoInfrastructureUnchanged(t *testing.T) {
	h := newInfraHarness(t)
	for range 2 { // create, then the update path
		svc := h.reconcile(infraGateway(nil, nil))
		assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web"}, svc.Labels)
		assert.Empty(t, svc.Annotations)
	}

	// An empty, but present, spec.infrastructure is the same thing.
	gw := infraGateway(nil, nil)
	gw.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{}
	svc := h.reconcile(gw)
	assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web"}, svc.Labels)
	assert.Empty(t, svc.Annotations)
}

// TestGatewayInfrastructure_AddUpdateRemove walks one Gateway through adding,
// changing and removing spec.infrastructure labels and annotations, on both the
// create and the update path of the Service.
func TestGatewayInfrastructure_AddUpdateRemove(t *testing.T) {
	h := newInfraHarness(t)

	// Add, on create.
	svc := h.reconcile(infraGateway(
		map[string]string{"team": "payments", "tier": "edge"},
		map[string]string{"lb.example.com/pool": "public", "note": "v1"},
	))
	assert.Equal(t, "payments", svc.Labels["team"])
	assert.Equal(t, "edge", svc.Labels["tier"])
	assert.Equal(t, "public", svc.Annotations["lb.example.com/pool"])
	assert.Equal(t, "v1", svc.Annotations["note"])
	assert.Equal(t, "apps.web", svc.Labels[LabelEdgeGateway], "aether's own label is still there")
	assert.JSONEq(t, `{"labels":["team","tier"],"annotations":["lb.example.com/pool","note"]}`,
		svc.Annotations[AnnotationInfrastructureKeys], "the keys the Gateway set are recorded, sorted")

	// Update a value, drop one label and one annotation, add a new annotation.
	svc = h.reconcile(infraGateway(
		map[string]string{"team": "checkout"},
		map[string]string{"lb.example.com/pool": "internal", "owner": "a"},
	))
	assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web", "team": "checkout"}, svc.Labels,
		"a label removed from the Gateway is removed from the Service")
	assert.Equal(t, "internal", svc.Annotations["lb.example.com/pool"])
	assert.Equal(t, "a", svc.Annotations["owner"])
	assert.NotContains(t, svc.Annotations, "note", "an annotation removed from the Gateway is removed from the Service")
	assert.JSONEq(t, `{"labels":["team"],"annotations":["lb.example.com/pool","owner"]}`,
		svc.Annotations[AnnotationInfrastructureKeys])

	// Remove spec.infrastructure altogether: back to the plain Service.
	svc = h.reconcile(infraGateway(nil, nil))
	assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web"}, svc.Labels)
	assert.Empty(t, svc.Annotations, "nothing of the Gateway's, and no bookkeeping, is left behind")
}

// TestGatewayInfrastructure_ForeignMetadataPreserved: labels and annotations
// another controller (or a person) put on the Service are never removed, neither
// while the Gateway sets its own nor when it stops setting them.
func TestGatewayInfrastructure_ForeignMetadataPreserved(t *testing.T) {
	h := newInfraHarness(t)
	h.reconcile(infraGateway(map[string]string{"team": "payments"}, map[string]string{"note": "v1"}))

	// A load-balancer controller annotates and labels the Service.
	svc := h.service()
	svc.Annotations["lb.example.com/allocated-from"] = "pool-a"
	svc.Labels["lb.example.com/managed"] = "true"
	require.NoError(t, h.c.Update(context.Background(), svc))

	svc = h.reconcile(infraGateway(map[string]string{"team": "payments"}, map[string]string{"note": "v2"}))
	assert.Equal(t, "pool-a", svc.Annotations["lb.example.com/allocated-from"])
	assert.Equal(t, "true", svc.Labels["lb.example.com/managed"])
	assert.Equal(t, "v2", svc.Annotations["note"])
	assert.JSONEq(t, `{"labels":["team"],"annotations":["note"]}`, svc.Annotations[AnnotationInfrastructureKeys],
		"a foreign key is not recorded as the Gateway's")

	svc = h.reconcile(infraGateway(nil, nil))
	assert.Equal(t, map[string]string{"lb.example.com/allocated-from": "pool-a"}, svc.Annotations)
	assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web", "lb.example.com/managed": "true"}, svc.Labels)
}

// TestGatewayInfrastructure_ReservedKeysRefused: a key aether, the Gateway API or
// the address pin owns is not copied, whatever the Gateway says, and everything
// else of the same Gateway still is.
func TestGatewayInfrastructure_ReservedKeysRefused(t *testing.T) {
	h := newInfraHarness(t)
	gw := infraGateway(
		map[string]string{
			LabelEdgeGateway:                         "someone.else",
			"gateway.networking.k8s.io/gateway-name": "web",
			"team":                                   "payments",
		},
		map[string]string{
			AnnotationInfrastructureKeys:     `{"labels":["aether.io/edge-gateway"]}`,
			AnnotationMetalLBLoadBalancerIPs: "198.51.100.7",
			"edge.aether.io/anything":        "x",
			"note":                           "v1",
		},
	)
	ipType := gatewayv1.IPAddressType
	gw.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Type: &ipType, Value: "203.0.113.10"}}

	svc := h.reconcile(gw)
	assert.Equal(t, map[string]string{LabelEdgeGateway: "apps.web", "team": "payments"}, svc.Labels,
		"aether's GC label keeps aether's value; the Gateway API label is not forged")
	assert.Equal(t, "203.0.113.10", svc.Annotations[AnnotationMetalLBLoadBalancerIPs],
		"the address pin comes from spec.addresses only")
	assert.NotContains(t, svc.Annotations, "edge.aether.io/anything")
	assert.Equal(t, "v1", svc.Annotations["note"])
	assert.JSONEq(t, `{"labels":["team"],"annotations":["note"]}`, svc.Annotations[AnnotationInfrastructureKeys])

	// Without spec.addresses the pin stays absent: the annotation is not a second
	// way to set it.
	gw.Spec.Addresses = nil
	svc = h.reconcile(gw)
	assert.NotContains(t, svc.Annotations, AnnotationMetalLBLoadBalancerIPs)

	// The refusal is logged once per change, not on every pass.
	logged := h.log.String()
	assert.Equal(t, 1, strings.Count(logged, "spec.infrastructure keys ignored"), logged)
	for _, k := range []string{LabelEdgeGateway, "gateway.networking.k8s.io/gateway-name", AnnotationMetalLBLoadBalancerIPs, "edge.aether.io/anything"} {
		assert.Contains(t, logged, k)
	}
	h.reconcile(infraGateway(map[string]string{"team": "payments"}, nil))
	h.reconcile(gw)
	assert.Equal(t, 2, strings.Count(h.log.String(), "spec.infrastructure keys ignored"),
		"logged again after the Gateway went through a state without refused keys")
}

// TestGatewayInfrastructure_InvalidLabelRefused: the API server rejects a whole
// Service over one invalid label, which would cost the Gateway its address. The
// Gateway CRD validates these, so this only matters with validation off or an
// older CRD; the pair is dropped and the rest applied.
func TestGatewayInfrastructure_InvalidLabelRefused(t *testing.T) {
	m := gatewayInfrastructureMetadata(new(infraGateway(
		map[string]string{"good": "yes", "bad-value": "not a label value!", "bad key": "v"},
		map[string]string{"bad key": "v", "fine": "any value at all, annotations take free text"},
	)))
	assert.Equal(t, map[string]string{"good": "yes"}, m.labels)
	assert.Equal(t, map[string]string{"fine": "any value at all, annotations take free text"}, m.annotations)
	assert.Len(t, m.refused, 3)
}

// TestGatewayInfrastructure_TamperedRecord: the bookkeeping annotation is data on
// an object others can write. A record that is not valid, or that names keys
// aether owns, must not make the edge remove its own label or the address pin.
func TestGatewayInfrastructure_TamperedRecord(t *testing.T) {
	for name, record := range map[string]string{
		"not JSON":        "team,tier",
		"names own label": `{"labels":["aether.io/edge-gateway","team"],"annotations":["metallb.universe.tf/loadBalancerIPs","aether.io/infrastructure-keys","aether.io/future"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newInfraHarness(t)
			gw := infraGateway(map[string]string{"team": "payments"}, nil)
			ipType := gatewayv1.IPAddressType
			gw.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Type: &ipType, Value: "203.0.113.10"}}
			h.reconcile(gw)

			svc := h.service()
			svc.Annotations[AnnotationInfrastructureKeys] = record
			// An annotation under aether's prefix that this version does not
			// write itself: the record cannot have it removed either.
			svc.Annotations["aether.io/future"] = "keep"
			require.NoError(t, h.c.Update(context.Background(), svc))

			svc = h.reconcile(gw)
			assert.Equal(t, "apps.web", svc.Labels[LabelEdgeGateway])
			assert.Equal(t, "payments", svc.Labels["team"])
			assert.Equal(t, "203.0.113.10", svc.Annotations[AnnotationMetalLBLoadBalancerIPs])
			assert.Equal(t, "keep", svc.Annotations["aether.io/future"])
			assert.JSONEq(t, `{"labels":["team"]}`, svc.Annotations[AnnotationInfrastructureKeys],
				"the record is rewritten from what was applied")
		})
	}
}

// TestReconcile_GatewayInfrastructureStatus: spec.infrastructure metadata never
// costs a Gateway its Accepted condition. Refused keys are named in the
// condition's message, which is where the Gateway's owner can read them.
func TestReconcile_GatewayInfrastructureStatus(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		wantMessage string
		wantInMsg   []string
	}{
		{name: "no infrastructure", wantMessage: "Gateway accepted by the aether edge controller"},
		{name: "plain keys", labels: map[string]string{"team": "payments"}, wantMessage: "Gateway accepted by the aether edge controller"},
		{
			name:      "reserved key",
			labels:    map[string]string{"team": "payments", LabelEdgeGateway: "x"},
			wantInMsg: []string{"spec.infrastructure", "label " + LabelEdgeGateway},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
			}
			c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
				WithObjects(gc, new(infraGateway(tc.labels, nil))).
				WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
				Build()
			r := &Reconciler{
				Client: c, APIReader: c, Sink: statusFakeSink{},
				Namespace: "aether-ingress", EdgeServiceName: "aether-edge",
				GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler),
			}
			_, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)

			got := &gatewayv1.Gateway{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "web"}, got))
			accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionTrue, accepted.Status)
			assert.Equal(t, string(gatewayv1.GatewayReasonAccepted), accepted.Reason)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, accepted.Message)
			}
			for _, s := range tc.wantInMsg {
				assert.Contains(t, accepted.Message, s)
			}
			assert.NotContains(t, accepted.Message, "label team", "an applied key is not reported")

			svc := &corev1.Service{}
			require.NoError(t, c.Get(context.Background(),
				types.NamespacedName{Namespace: "aether-ingress", Name: gatewayServiceName("aether-edge", "apps", "web")}, svc))
			if tc.labels != nil {
				assert.Equal(t, "payments", svc.Labels["team"], "the whole reconcile carries the label to the Service")
			}
		})
	}
}
