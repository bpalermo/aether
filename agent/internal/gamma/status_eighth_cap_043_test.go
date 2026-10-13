package gamma

// Eighth adversarial review of the proposal-043 stack (head f5ed0ee5). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A route whose record passes the cap with its keys: 256 backendRefs to user
// Services with 63-character names. The LAST Service of the sorted list
// serves raw TCP on the target port (the tables answer 500 for its share). The
// node that holds the pods writes ResolvedRefs=False. While the record was
// cut at the cap its verdict for that Service was one the record left out: a
// node that held no pod found none, wanted "Not checked" (True), and the two
// wrote over each other at the bounded rate for as long as the route existed,
// with the True standing for most of every wait. The record is whole now
// (written with digests), the node that holds no pod reads the verdict, and
// the two settle on False.
func TestStatus_ARecordTooLongForItsKeysSettles(t *testing.T) {
	pad := strings.Repeat("s", 59)
	longNS := strings.Repeat("n", 63)
	hr := httpRoute("r1", longNS, "front", "x")
	hr.Spec.Rules = nil
	front := generatedService("front", map[int32]string{8080: "http"})
	front.Namespace = longNS
	objects := []client.Object{front, hr}
	last := ""
	for rule := 0; rule < 16; rule++ {
		var refs []gatewayv1.HTTPBackendRef
		for b := 0; b < 16; b++ {
			name := fmt.Sprintf("%s%04d", pad, rule*16+b)
			last = name
			svc := plainService(name, corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
			svc.Namespace = longNS
			objects = append(objects, svc)
			refs = append(refs, gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name), Port: ptr(gatewayv1.PortNumber(80)),
			}}})
		}
		hr.Spec.Rules = append(hr.Spec.Rules, gatewayv1.HTTPRouteRule{BackendRefs: refs})
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	start := clk.t
	judge := r6Reconciler(c, podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
		if service == longNS+"/"+last {
			return map[uint32]registryv1.PortProtocol{8080: ppTCP}, true
		}
		return map[uint32]registryv1.PortProtocol{8080: ppHTTP}, true
	}))
	none := r6Reconciler(c, r6NoPods)
	judge.Now, none.Now = clk.now, clk.now
	resolved := func() string {
		got := &gatewayv1.HTTPRoute{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: longNS, Name: "r1"}, got))
		if len(got.Status.Parents) == 0 {
			return "none"
		}
		return string(meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs)).Status)
	}
	rv := func() string {
		got := &gatewayv1.HTTPRoute{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: longNS, Name: "r1"}, got))
		return got.ResourceVersion
	}
	nodes := []*Reconciler{judge, none}
	names := []string{"judge", "none"}
	due := make([]time.Time, 2)
	writes := []int{0, 0}
	trueFor, falseFor := time.Duration(0), time.Duration(0)
	var log []string
	for clk.t.Sub(start) < 30*time.Minute {
		pending := []bool{clk.t.Equal(start) || (!due[0].IsZero() && !clk.t.Before(due[0])), clk.t.Equal(start) || (!due[1].IsZero() && !clk.t.Before(due[1]))}
		for turn := 0; turn < 100; turn++ {
			next := -1
			for i, p := range pending {
				if p {
					next = i
					break
				}
			}
			if next < 0 {
				break
			}
			pending[next] = false
			before := rv()
			res, err := nodes[next].Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)
			due[next] = time.Time{}
			if res.RequeueAfter > 0 {
				due[next] = clk.t.Add(res.RequeueAfter)
			}
			if rv() != before {
				writes[next]++
				log = append(log, fmt.Sprintf("%v:%s->%s", clk.t.Sub(start), names[next], resolved()))
				pending[0], pending[1] = true, true
			}
		}
		if resolved() == "True" {
			trueFor += time.Second
		} else {
			falseFor += time.Second
		}
		clk.t = clk.t.Add(time.Second)
	}
	t.Logf("30 simulated minutes: the judging node wrote %d times, the node that holds no pod %d times; ResolvedRefs was True for %v and False for %v (the tables answer 500 for one backendRef throughout)", writes[0], writes[1], trueFor, falseFor)
	t.Logf("writes: %v", log)
	assert.LessOrEqual(t, writes[0]+writes[1], 4, "two agents of this build that hold the same rows (or none) settle after a write or two per node")
	assert.Equal(t, "False", resolved(), "the tables answer 500 for one backendRef, and the status that stands says True")
	assert.Zero(t, trueFor, "ResolvedRefs said True while the tables answered 500")
}
