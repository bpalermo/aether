package capture

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/common/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type fakeSink struct {
	got     map[string]string
	records map[string]string
}

func (f *fakeSink) SetCaptureAuthorities(a map[string]string) { f.got = a }
func (f *fakeSink) SetMeshDNSRecords(r map[string]string)     { f.records = r }

func TestReconcile_ProjectsAuthoritiesAndDNSRecords(t *testing.T) {
	mesh := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-1", Namespace: "aether-test",
			Labels:      map[string]string{constants.LabelMeshService: "true"},
			Annotations: map[string]string{constants.AnnotationMeshService: "svc-1", constants.AnnotationMeshPort: "8080"},
		},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.42"},
	}
	// A non-mesh Service of the same shape must be ignored (label-scoped List).
	user := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"}}

	c := fake.NewClientBuilder().WithObjects(mesh, user).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, Log: slog.New(slog.DiscardHandler)}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"svc-1": "svc-1.aether-test.svc.cluster.local"}, sink.got)
	assert.Equal(t, map[string]string{"svc-1": "10.96.0.42"}, sink.records, "ClusterIP -> the <svc>.<meshDomain> A record")
}
