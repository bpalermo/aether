package meshconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testMutatingWebhookConfigName = "aether-mutating-webhook"

	validatingResource = "validatingwebhookconfigurations"
	mutatingResource   = "mutatingwebhookconfigurations"

	admissionAPIPrefix = "/apis/admissionregistration.k8s.io/v1/"
)

// fakeAPIServer is the slice of a kube-apiserver the caBundle injector talks
// to: get, list, watch and update on the two cluster-scoped webhook
// configuration resources, with a per-resource, per-verb switch that answers
// 403 the way RBAC does.
//
// It exists so the injector can be run against the SAME client stack the
// controller-runtime manager builds — a real informer cache and the real
// cache-backed client — instead of a fake client, which has no informer and so
// cannot wait for one. That wait is the whole of issue #1431.
type fakeAPIServer struct {
	srv *httptest.Server

	mu              sync.Mutex
	resourceVersion int
	validating      map[string]*admissionregistrationv1.ValidatingWebhookConfiguration
	mutating        map[string]*admissionregistrationv1.MutatingWebhookConfiguration
	// forbidden holds "<resource>/<verb>" entries the server answers 403 for.
	forbidden map[string]bool
	// requests counts "<resource>/<verb>" requests served, allowed or not.
	requests map[string]int
}

// startFakeAPIServer serves an un-injected validating and mutating webhook
// configuration.
func startFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()

	f := &fakeAPIServer{
		resourceVersion: 1,
		validating: map[string]*admissionregistrationv1.ValidatingWebhookConfiguration{
			testWebhookConfigName: {
				TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfiguration"},
				ObjectMeta: metav1.ObjectMeta{Name: testWebhookConfigName, ResourceVersion: "1"},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{{
					Name:                    "meshconfig.aether.io",
					SideEffects:             ptr(admissionregistrationv1.SideEffectClassNone),
					AdmissionReviewVersions: []string{"v1"},
				}},
			},
		},
		mutating: map[string]*admissionregistrationv1.MutatingWebhookConfiguration{
			testMutatingWebhookConfigName: {
				TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "MutatingWebhookConfiguration"},
				ObjectMeta: metav1.ObjectMeta{Name: testMutatingWebhookConfigName, ResourceVersion: "1"},
				Webhooks: []admissionregistrationv1.MutatingWebhook{{
					Name:                    "pod.aether.io",
					SideEffects:             ptr(admissionregistrationv1.SideEffectClassNone),
					AdmissionReviewVersions: []string{"v1"},
				}},
			},
		},
		forbidden: map[string]bool{},
		requests:  map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	// Registered first, so it runs last: by then the test's own contexts are
	// cancelled, and closing the client connections ends any watch still open.
	t.Cleanup(func() {
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

// forbid makes the server answer 403 for the given verbs on a resource.
func (f *fakeAPIServer) forbid(resource string, verbs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, verb := range verbs {
		f.forbidden[resource+"/"+verb] = true
	}
}

// allow reverses forbid: the permission has appeared.
func (f *fakeAPIServer) allow(resource string, verbs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, verb := range verbs {
		delete(f.forbidden, resource+"/"+verb)
	}
}

// served reports how many requests of one verb a resource has received.
func (f *fakeAPIServer) served(resource, verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[resource+"/"+verb]
}

// validatingBundle is the caBundle currently stored on the validating webhook.
func (f *fakeAPIServer) validatingBundle() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.validating[testWebhookConfigName].Webhooks[0].ClientConfig.CABundle
}

// mutatingBundle is the caBundle currently stored on the mutating webhook.
func (f *fakeAPIServer) mutatingBundle() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutating[testMutatingWebhookConfigName].Webhooks[0].ClientConfig.CABundle
}

func (f *fakeAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, admissionAPIPrefix)
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "no such path "+r.URL.Path)
		return
	}
	resource, name, _ := strings.Cut(rest, "/")
	if resource != validatingResource && resource != mutatingResource {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "no such resource "+resource)
		return
	}

	verb := requestVerb(r, name)
	f.mu.Lock()
	f.requests[resource+"/"+verb]++
	denied := f.forbidden[resource+"/"+verb]
	f.mu.Unlock()
	if denied {
		writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden,
			fmt.Sprintf("%s.admissionregistration.k8s.io is forbidden: cannot %s resource %q at the cluster scope", resource, verb, resource))
		return
	}

	switch verb {
	case "get":
		f.get(w, resource, name)
	case "update":
		f.update(w, r, resource, name)
	case "list":
		f.list(w, resource)
	case "watch":
		f.watch(w, r)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, r.Method)
	}
}

// requestVerb maps a request onto the RBAC verb the apiserver would authorize.
func requestVerb(r *http.Request, name string) string {
	switch {
	case r.Method == http.MethodPut && name != "":
		return "update"
	case r.Method == http.MethodGet && name != "":
		return "get"
	case r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true":
		return "watch"
	case r.Method == http.MethodGet:
		return "list"
	default:
		return "unsupported"
	}
}

func (f *fakeAPIServer) get(w http.ResponseWriter, resource, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var obj any
	found := false
	if resource == validatingResource {
		obj, found = f.validating[name]
	} else {
		obj, found = f.mutating[name]
	}
	if !found {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, resource+" "+name+" not found")
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (f *fakeAPIServer) update(w http.ResponseWriter, r *http.Request, resource, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.resourceVersion++
	rv := strconv.Itoa(f.resourceVersion)
	if resource == validatingResource {
		var obj admissionregistrationv1.ValidatingWebhookConfiguration
		if err := json.Unmarshal(body, &obj); err != nil {
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
			return
		}
		obj.ResourceVersion = rv
		f.validating[name] = &obj
		writeJSON(w, http.StatusOK, &obj)
		return
	}
	var obj admissionregistrationv1.MutatingWebhookConfiguration
	if err := json.Unmarshal(body, &obj); err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}
	obj.ResourceVersion = rv
	f.mutating[name] = &obj
	writeJSON(w, http.StatusOK, &obj)
}

func (f *fakeAPIServer) list(w http.ResponseWriter, resource string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	listMeta := metav1.ListMeta{ResourceVersion: strconv.Itoa(f.resourceVersion)}
	if resource == validatingResource {
		out := admissionregistrationv1.ValidatingWebhookConfigurationList{
			TypeMeta: metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingWebhookConfigurationList"},
			ListMeta: listMeta,
		}
		for _, obj := range f.validating {
			out.Items = append(out.Items, *obj)
		}
		writeJSON(w, http.StatusOK, &out)
		return
	}
	out := admissionregistrationv1.MutatingWebhookConfigurationList{
		TypeMeta: metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "MutatingWebhookConfigurationList"},
		ListMeta: listMeta,
	}
	for _, obj := range f.mutating {
		out.Items = append(out.Items, *obj)
	}
	writeJSON(w, http.StatusOK, &out)
}

// watch refuses the streaming-list form (the reflector then falls back to a
// plain list) and otherwise holds an event-less watch open until the client
// goes away: the tests need an informer that syncs, not one that is current.
func (f *fakeAPIServer) watch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		writeStatus(w, http.StatusUnprocessableEntity, metav1.StatusReasonInvalid, "sendInitialEvents is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	<-r.Context().Done()
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	writeJSON(w, code, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   reason,
		Code:     int32(code),
		Message:  message,
	})
}

// managerClients is what a controller-runtime manager hands a runnable: the
// cache-backed client of GetClient, the uncached reader of GetAPIReader, and
// the cache whose sync the controller's `cache-sync` readiness check waits on.
// It is built the way the manager builds them (cluster.New), against the fake
// apiserver.
type managerClients struct {
	cached    client.Client
	apiReader client.Reader
	cache     cache.Cache
}

// GetClient mirrors manager.GetClient.
func (m *managerClients) GetClient() client.Client { return m.cached }

// GetAPIReader mirrors manager.GetAPIReader.
func (m *managerClients) GetAPIReader() client.Reader { return m.apiReader }

// newManagerClients builds the manager's client stack against the fake
// apiserver and starts the cache; it stops with ctx.
func (f *fakeAPIServer) newManagerClients(ctx context.Context, t *testing.T) *managerClients {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	// JSON on the wire (the fake server speaks nothing else), and a static REST
	// mapper so nothing needs API discovery.
	cfg := &rest.Config{Host: f.srv.URL, ContentConfig: rest.ContentConfig{ContentType: runtime.ContentTypeJSON}}
	mapper := meta.NewDefaultRESTMapper(nil)
	gv := admissionregistrationv1.SchemeGroupVersion
	mapper.Add(gv.WithKind("ValidatingWebhookConfiguration"), meta.RESTScopeRoot)
	mapper.Add(gv.WithKind("MutatingWebhookConfiguration"), meta.RESTScopeRoot)

	informers, err := cache.New(cfg, cache.Options{Scheme: scheme, Mapper: mapper})
	require.NoError(t, err)
	cached, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper, Cache: &client.CacheOptions{Reader: informers}})
	require.NoError(t, err)
	apiReader, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
	require.NoError(t, err)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = informers.Start(ctx)
	}()
	t.Cleanup(func() { <-stopped })
	require.True(t, informers.WaitForCacheSync(ctx), "an empty cache must report synced")

	return &managerClients{cached: cached, apiReader: apiReader, cache: informers}
}
