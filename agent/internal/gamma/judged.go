package gamma

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"aethermesh.dev/agent/internal/gatewaystatus"
	"aethermesh.dev/common/referencegrant"
	"aethermesh.dev/common/serviceref"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// One status, many writers.
//
// Every node's agent reconciles every route and writes its one status. All
// but one of the facts a condition is made of are the same on every node: the
// route, the ReferenceGrants, the Services and their ports. Each node computes
// those itself, on every reconcile, and never keeps what another node (or its
// own earlier reconcile) concluded from them: a condition is a function of
// the facts, and a False goes the moment its cause does.
//
// The one fact that is NOT the same on every node is what the pods behind a
// port of a user's Service serve (PodPorts): a node knows it only while it
// holds a row of the service. So that fact alone is written down where every
// agent can read it: the route's own status, in a condition of aether's type
// (JudgedConditionType) beside Accepted and ResolvedRefs, as a list of
// "<ns>/<svc>:<service port>><pod port>=<verdict>". It is for one generation
// of the route (the condition's observedGeneration) and for one target (the
// pod port is in the key: a Service whose targetPort moves is another key).
//
// A node that holds the pods judges the port itself and writes its verdict.
// A node that does not, and only such a node, takes the verdict written
// there, True or False alike, and computes its conditions from it exactly as
// the judging node did. So:
//
//   - a node with knowledge beats a node without (the second adopts);
//   - two nodes with knowledge agree, because they read the same rows;
//   - a node never writes "not checked" over a verdict, and never keeps a
//     verdict about anything but that port;
//   - nodes that hold the same rows of a service, and nodes that hold none,
//     reach a status every one of them would write, after which none writes.
//
// Nodes that hold DIFFERENT rows of one service do not agree, and neither
// does an agent of the chart before; those are bounded, not settled
// (contest.go).
//
// A verdict whose pods are all gone stays (no node can judge the port any
// more, and what was said of it is still the last thing known) until a node
// holds a pod of the Service again or the route's generation moves.

// JudgedConditionType is the condition, on aether's own parent status of a
// route, that carries what the pods behind the route's user-Service ports
// serve, as judged by a node that holds them. Its message is data
// (encodeJudgements); it is always True.
const JudgedConditionType = "gateway.aether.io/PodPortsJudged"

const judgedReason = "Judged"

// The verdicts of a pod port.
const (
	verdictHTTP       = "http"
	verdictTCP        = "tcp"
	verdictUDP        = "udp"
	verdictUndeclared = "undeclared"
)

// judgementKey names one port of a user's Service and the pod port behind it.
func judgementKey(service string, port, pod uint32) string {
	return fmt.Sprintf("%s:%d>%d", service, port, pod)
}

// encodeJudgements renders verdicts for the condition's message: one
// "key=verdict" per port, sorted, so that two nodes with the same verdicts
// write the same bytes.
//
// A condition's message has a cap (gatewaystatus.MaxConditionMessage), and a
// longer one fails the whole status update. A route can name some 250 ports,
// with names of up to 63 characters each way: the record of such a route can
// pass it. Whole verdicts are then left out from the END of the sorted list,
// the same ones on every node. A port whose verdict is left out is one a node
// that holds no pod of the Service cannot read: it says "Not checked" of it,
// and the node that holds the pods writes its own status back at the bounded
// rate of a contested status (contest.go).
func encodeJudgements(j map[string]string) string {
	parts := make([]string, 0, len(j))
	for k, v := range j {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	size, fit := 0, len(parts)
	for i, part := range parts {
		if size += len(part) + len(", "); size-len(", ") > gatewaystatus.MaxConditionMessage {
			fit = i
			break
		}
	}
	return strings.Join(parts[:fit], ", ")
}

// decodeJudgements is the inverse. Anything that is not "key=verdict" with a
// verdict this build knows is dropped: the message is another writer's.
func decodeJudgements(message string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(message, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			continue
		}
		switch v {
		case verdictHTTP, verdictTCP, verdictUDP, verdictUndeclared:
			out[k] = v
		}
	}
	return out
}

// priorJudgements returns the verdicts aether's entries of a route's status
// carry for this generation of the route.
func priorJudgements(parents []gatewayv1.RouteParentStatus, generation int64) map[string]string {
	out := map[string]string{}
	for i := range parents {
		if parents[i].ControllerName != gatewaystatus.MeshControllerName {
			continue
		}
		for _, c := range parents[i].Conditions {
			if c.Type != JudgedConditionType || c.ObservedGeneration != generation {
				continue
			}
			for k, v := range decodeJudgements(c.Message) {
				out[k] = v
			}
		}
	}
	return out
}

// portRef is one (Service, port) a route names, as a parentRef or a backendRef.
type portRef struct {
	service string
	port    uint32
}

// routePortRefs lists the Service ports a route names.
func routePortRefs(routeNamespace string, parents []gatewayv1.ParentReference, backends []gatewayv1.BackendObjectReference) []portRef {
	var out []portRef
	for _, p := range parents {
		if p.Port == nil || (p.Group != nil && string(*p.Group) != "") || p.Kind == nil || string(*p.Kind) != "Service" {
			continue
		}
		ns := string(ptrOr(p.Namespace, ""))
		out = append(out, portRef{service: serviceref.New(orDefault(ns, routeNamespace), string(p.Name)).Key(), port: uint32(*p.Port)})
	}
	for _, ref := range backends {
		if ref.Port == nil || (ref.Group != nil && string(*ref.Group) != "") || (ref.Kind != nil && string(*ref.Kind) != "Service") {
			continue
		}
		ns := referencegrant.BackendNamespace(ref.Namespace)
		out = append(out, portRef{service: serviceref.New(orDefault(ns, routeNamespace), string(ref.Name)).Key(), port: uint32(*ref.Port)})
	}
	return out
}

func ptrOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// judgedFacts returns the facts one route's conditions are computed from, and
// the verdicts to write beside them. For each port the route names that only
// the pods can speak for (portFacts.open), the verdict is this node's own when
// it holds a row of the service (portFacts.verdict), and otherwise, and only
// then, the one another node wrote for that port and target (prior), when
// there is one. A port neither has a verdict for stays unjudged.
//
// facts is not modified: a service whose facts take a verdict in is copied.
func judgedFacts(facts map[string]portFacts, prior map[string]string, refs []portRef) (map[string]portFacts, map[string]string) {
	out := facts
	record := map[string]string{}
	owned := map[string]bool{}
	for _, ref := range refs {
		f, named := out[ref.service]
		if !named {
			continue
		}
		pod, open := f.open(ref.port)
		if !open {
			continue
		}
		key := judgementKey(ref.service, ref.port, pod)
		v, ok := f.verdict(pod)
		if !ok {
			v, ok = prior[key]
		}
		if !ok {
			continue
		}
		if len(owned) == 0 {
			out = maps.Clone(facts)
		}
		if !owned[ref.service] {
			f = f.clone()
			owned[ref.service] = true
		}
		out[ref.service] = f.apply(ref.port, pod, v)
		record[key] = v
	}
	return out, record
}

// judgedCondition is the condition that carries record.
func judgedCondition(record map[string]string) gatewaystatus.Condition {
	return gatewaystatus.Condition{
		Type:    JudgedConditionType,
		Status:  metav1.ConditionTrue,
		Reason:  judgedReason,
		Message: encodeJudgements(record),
	}
}
