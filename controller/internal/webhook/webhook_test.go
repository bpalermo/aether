package webhook

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type stub struct{ resp admission.Response }

func (s stub) Handle(context.Context, admission.Request) admission.Response { return s.resp }

func req(kind string) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Kind: metav1.GroupVersionKind{Group: "config.aether.io", Version: "v1", Kind: kind},
	}}
}

func TestDispatchByKind(t *testing.T) {
	h := NewHandler(slog.New(slog.DiscardHandler), map[string]admission.Handler{
		"MeshConfig":  stub{admission.Denied("mc")},
		"VirtualHost": stub{admission.Denied("vh")},
	})
	assert.Equal(t, "mc", h.Handle(context.Background(), req("MeshConfig")).Result.Message)
	assert.Equal(t, "vh", h.Handle(context.Background(), req("VirtualHost")).Result.Message)
}

// TestUnknownKindAdmitted verifies an unrouted Kind is admitted (fail-open) — it
// should never arrive, since the webhook rules scope the endpoint.
func TestUnknownKindAdmitted(t *testing.T) {
	h := NewHandler(slog.New(slog.DiscardHandler), map[string]admission.Handler{})
	assert.True(t, h.Handle(context.Background(), req("Other")).Allowed)
}
