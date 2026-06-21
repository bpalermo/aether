package virtualhost

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func vhSpec(hosts []string, withRoute bool) *configv1.VirtualHostSpec {
	spec := &configv1.VirtualHostSpec{}
	if len(hosts) > 0 {
		spec.SetHosts(hosts)
	}
	if withRoute {
		m := &configv1.RouteMatch{}
		m.SetPrefix("/")
		b := &configv1.RouteBackend{}
		b.SetService("svc-1")
		hr := &configv1.HTTPRoute{}
		hr.SetMatch(m)
		hr.SetBackend(b)
		spec.SetRoutes([]*configv1.HTTPRoute{hr})
	}
	return spec
}

func existingVH(name, ns string, hosts ...string) *crdv1.VirtualHost {
	vh := &crdv1.VirtualHost{Spec: vhSpec(hosts, true)}
	vh.Name = name
	vh.Namespace = ns
	return vh
}

func newValidator(objs ...client.Object) *Validator {
	scheme := runtime.NewScheme()
	_ = crdv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Validator{Reader: cl, Log: slog.New(slog.DiscardHandler)}
}

func handle(t *testing.T, v *Validator, name, ns string, hosts []string, withRoute bool) admission.Response {
	vh := &crdv1.VirtualHost{Spec: vhSpec(hosts, withRoute)}
	vh.Name = name
	vh.Namespace = ns
	raw, err := json.Marshal(vh)
	require.NoError(t, err)
	return v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	})
}

func TestAllowsUniqueHost(t *testing.T) {
	v := newValidator(existingVH("other", "aether-edge", "b.example.com"))
	resp := handle(t, v, "api", "aether-edge", []string{"a.example.com"}, true)
	assert.True(t, resp.Allowed, "a host claimed by nobody else is admitted")
}

func TestRejectsDuplicateHost(t *testing.T) {
	v := newValidator(existingVH("owner", "team-a", "api.example.com"))
	resp := handle(t, v, "intruder", "team-b", []string{"api.example.com"}, true)
	require.False(t, resp.Allowed, "a host claimed by another VirtualHost is rejected")
	assert.Contains(t, resp.Result.Message, "team-a/owner")
}

func TestAllowsSelfUpdate(t *testing.T) {
	// The same object (name+namespace) re-claiming its own host is an update, not
	// a collision.
	v := newValidator(existingVH("api", "aether-edge", "api.example.com"))
	resp := handle(t, v, "api", "aether-edge", []string{"api.example.com"}, true)
	assert.True(t, resp.Allowed, "a VirtualHost may keep its own host on update")
}

func TestAllowsWildcardAndSpecific(t *testing.T) {
	// *.example.com and api.example.com are NOT a conflict — Envoy resolves
	// most-specific-first; only exact-string duplicates are rejected.
	v := newValidator(existingVH("wild", "aether-edge", "*.example.com"))
	resp := handle(t, v, "api", "aether-edge", []string{"api.example.com"}, true)
	assert.True(t, resp.Allowed, "a specific host may coexist with a wildcard host")
}

func TestRejectsInvalidSpec(t *testing.T) {
	// No routes -> protovalidate (routes min_items=1) fails before the dup check.
	v := newValidator()
	resp := handle(t, v, "api", "aether-edge", []string{"api.example.com"}, false)
	assert.False(t, resp.Allowed, "a spec failing protovalidate is rejected")
}
