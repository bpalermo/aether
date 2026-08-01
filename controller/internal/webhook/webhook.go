// Package webhook provides the aether-controller's single validating admission
// endpoint (/validate). The apiserver routes each CRD's CREATE/UPDATE to this one
// path (the webhook rules select the resources); the handler dispatches to a
// per-kind validator by the request Kind. One endpoint keeps the
// ValidatingWebhookConfiguration and its serving cert simple.
package webhook

import (
	"context"
	"log/slog"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Path is the single URL path every CRD validating rule points at.
const Path = "/validate"

// Handler dispatches an admission request to the validator registered for its
// Kind. An unknown Kind is admitted (fail-open) — it should never arrive, since
// the webhook rules scope the endpoint to known resources.
type Handler struct {
	log    *slog.Logger
	byKind map[string]admission.Handler
}

// NewHandler builds a dispatcher routing each Kind to its validator.
func NewHandler(log *slog.Logger, byKind map[string]admission.Handler) *Handler {
	return &Handler{log: log, byKind: byKind}
}

// Handle routes by the request's Kind.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v, ok := h.byKind[req.Kind.Kind]; ok {
		return v.Handle(ctx, req)
	}
	h.log.WarnContext(ctx, "no validator for admission kind; admitting", "kind", req.Kind.Kind)
	return admission.Allowed("")
}

// SetupWithManager registers the dispatcher at Path on the manager's webhook server.
func (h *Handler) SetupWithManager(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(Path, &admission.Webhook{Handler: h})
}
