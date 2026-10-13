package registrytest

import (
	"fmt"
	"strings"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// recorder is a testing.TB that records failures instead of failing, so a test
// can assert that a contract helper REJECTS a listing. Only the methods the
// helpers call are implemented; anything else panics on the nil embedded TB,
// which is the right outcome for a helper that starts using something new.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

const (
	classHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	classTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	classUDP  = registryv1.PortProtocol_PORT_PROTOCOL_UDP

	listHTTP = registryv1.Service_PROTOCOL_HTTP
	listTCP  = registryv1.Service_PROTOCOL_TCP
	listUDP  = registryv1.Service_PROTOCOL_UDP
)

type (
	classes  = map[uint32]registryv1.PortProtocol
	listings = map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
)

func row(ip string, port uint32, served []uint32, portProtocols classes) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{Ip: ip, ClusterName: "c", Port: port, Ports: served, PortProtocols: portProtocols}
}

func mixed(ip string) *registryv1.ServiceEndpoint {
	return row(ip, 8080, []uint32{8080, 9000}, classes{8080: classHTTP, 9000: classTCP})
}

func TestRequirePerPortProtocols(t *testing.T) {
	tests := []struct {
		name string
		in   listings
		// want is one substring per expected failure, in order; empty means
		// the listing satisfies the contract.
		want []string
	}{
		{
			name: "a single-class service in its own listing",
			in: listings{
				listHTTP: {"ns/web": {row("10.0.0.1", 8080, []uint32{8080}, classes{8080: classHTTP})}},
				listTCP:  {"ns/db": {row("10.0.0.2", 5432, []uint32{5432}, classes{5432: classTCP})}},
			},
		},
		{
			name: "a mixed pod is in both listings with the same row",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP:  {"ns/mixed": {mixed("10.0.0.1")}},
			},
		},
		{
			name: "the served set is compared as a set",
			in: listings{
				// Neither row is in sorted order, and the two differ.
				listHTTP: {"ns/mixed": {
					row("10.0.0.1", 8080, []uint32{9000, 8080, 7070}, classes{7070: classHTTP, 8080: classHTTP, 9000: classTCP}),
				}},
				listTCP: {"ns/mixed": {
					row("10.0.0.1", 8080, []uint32{8080, 7070, 9000}, classes{7070: classHTTP, 8080: classHTTP, 9000: classTCP}),
				}},
			},
		},
		{
			name: "a row written before port_protocols existed is exempt from rule 1",
			in: listings{
				listTCP: {"ns/old": {row("10.0.0.1", 5432, nil, nil)}},
			},
		},
		{
			name: "two services, or two clusters, sharing an IP are different endpoints",
			in: listings{
				listHTTP: {
					"ns/a": {row("10.0.0.1", 8080, []uint32{8080}, classes{8080: classHTTP})},
					"ns/c": {{Ip: "10.0.0.3", ClusterName: "east", Port: 8080, PortProtocols: classes{8080: classHTTP}}},
				},
				listTCP: {
					"ns/b": {row("10.0.0.1", 9000, []uint32{9000}, classes{9000: classTCP})},
					"ns/c": {{Ip: "10.0.0.3", ClusterName: "west", Port: 9000, PortProtocols: classes{9000: classTCP}}},
				},
			},
		},
		{
			name: "an HTTP-only pod returned under TCP",
			in: listings{
				listHTTP: {"ns/web": {row("10.0.0.1", 8080, []uint32{8080}, classes{8080: classHTTP})}},
				listTCP:  {"ns/web": {row("10.0.0.1", 8080, []uint32{8080}, classes{8080: classHTTP})}},
			},
			want: []string{`service "ns/web" endpoint 10.0.0.1 is listed under PROTOCOL_TCP but advertises no PROTOCOL_TCP port`},
		},
		{
			name: "a TCP and UDP pod returned under HTTP",
			in: listings{
				listHTTP: {"ns/dns": {row("10.0.0.1", 53, []uint32{53, 5353}, classes{53: classUDP, 5353: classTCP})}},
			},
			want: []string{`is listed under PROTOCOL_HTTP but advertises no PROTOCOL_HTTP port`},
		},
		{
			name: "the same pod with a different declared port in each listing",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP: {"ns/mixed": {
					row("10.0.0.1", 9000, []uint32{8080, 9000}, classes{8080: classHTTP, 9000: classTCP}),
				}},
			},
			want: []string{`service "ns/mixed" endpoint 10.0.0.1 differs between the PROTOCOL_HTTP and PROTOCOL_TCP listings: port 8080 vs 9000`},
		},
		{
			name: "the same pod with a different class for one port in each listing",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listUDP: {"ns/mixed": {
					row("10.0.0.1", 8080, []uint32{8080, 9000}, classes{8080: classHTTP, 9000: classUDP}),
				}},
			},
			want: []string{`differs between the PROTOCOL_HTTP and PROTOCOL_UDP listings`},
		},
		{
			name: "the same pod with a different served set in each listing",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP: {"ns/mixed": {
					row("10.0.0.1", 8080, []uint32{9000}, classes{8080: classHTTP, 9000: classTCP}),
				}},
			},
			want: []string{`ports [8080 9000] vs [9000]`},
		},
		{
			name: "a pre-field row beside the same pod's full row",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP:  {"ns/mixed": {row("10.0.0.1", 8080, []uint32{8080, 9000}, nil)}},
			},
			want: []string{`differs between the PROTOCOL_HTTP and PROTOCOL_TCP listings`},
		},
		{
			name: "one bad pod among good ones is the only one reported",
			in: listings{
				listHTTP: {"ns/mixed": {
					mixed("10.0.0.1"), mixed("10.0.0.2"),
					row("10.0.0.3", 8080, []uint32{8080}, classes{8080: classHTTP}),
				}},
				listTCP: {"ns/mixed": {
					mixed("10.0.0.1"), mixed("10.0.0.2"),
					row("10.0.0.3", 8080, []uint32{8080}, classes{8080: classHTTP}),
				}},
			},
			want: []string{`endpoint 10.0.0.3 is listed under PROTOCOL_TCP`},
		},
		{
			// The listing of a backend that files a pod under its pod-level
			// protocol alone (#1672 on the kubernetes backend): every row it
			// returns is right, and the pod has no raw-TCP cluster.
			name: "a mixed pod missing from the listing of its second class",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP:  {},
				listUDP:  {},
			},
			want: []string{`service "ns/mixed" endpoint 10.0.0.1 declares a PROTOCOL_TCP port and is missing from the PROTOCOL_TCP listing`},
		},
		{
			name: "a mixed pod missing from the listing of its declared port's class",
			in: listings{
				listHTTP: {"ns/other": {row("10.0.0.9", 8080, []uint32{8080}, classes{8080: classHTTP})}},
				listTCP:  {"ns/mixed": {mixed("10.0.0.1")}},
			},
			want: []string{`service "ns/mixed" endpoint 10.0.0.1 declares a PROTOCOL_HTTP port and is missing from the PROTOCOL_HTTP listing`},
		},
		{
			name: "a pod of three classes in one listing is reported once per missing listing",
			in: listings{
				listHTTP: {},
				listTCP:  {"ns/all": {row("10.0.0.1", 9000, nil, classes{8080: classHTTP, 9000: classTCP, 5353: classUDP, 5354: classUDP})}},
				listUDP:  {},
			},
			want: []string{
				`declares a PROTOCOL_HTTP port and is missing from the PROTOCOL_HTTP listing`,
				`declares a PROTOCOL_UDP port and is missing from the PROTOCOL_UDP listing`,
			},
		},
		{
			name: "one of two mixed pods missing from the second listing is the only one reported",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1"), mixed("10.0.0.2")}},
				listTCP:  {"ns/mixed": {mixed("10.0.0.2")}},
			},
			want: []string{`endpoint 10.0.0.1 declares a PROTOCOL_TCP port and is missing from the PROTOCOL_TCP listing`},
		},
		{
			// The same address in another service, or another cluster, does
			// not stand in for the missing row.
			name: "the missing row is not found under another service or cluster",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listTCP: {
					"ns/other": {mixed("10.0.0.1")},
					"ns/mixed": {{Ip: "10.0.0.1", ClusterName: "west", Port: 9000, PortProtocols: classes{9000: classTCP}}},
				},
			},
			want: []string{
				`service "ns/mixed" endpoint 10.0.0.1 declares a PROTOCOL_TCP port and is missing from the PROTOCOL_TCP listing`,
				`service "ns/other" endpoint 10.0.0.1 declares a PROTOCOL_HTTP port and is missing from the PROTOCOL_HTTP listing`,
			},
		},
		{
			// Completeness is checked among the protocols the caller listed:
			// a caller that did not list TCP has said nothing about it.
			name: "a mixed pod when the caller did not pass its second listing",
			in: listings{
				listHTTP: {"ns/mixed": {mixed("10.0.0.1")}},
				listUDP:  {},
			},
		},
		{
			name: "a row written before port_protocols existed is exempt from completeness",
			in: listings{
				listHTTP: {},
				listTCP:  {"ns/old": {row("10.0.0.1", 5432, []uint32{5432, 8080}, nil)}},
				listUDP:  {},
			},
		},
		{
			// A port with no class names no listing, even when the caller
			// passes a listing under the zero protocol.
			name: "a port of an unspecified class asks for no listing",
			in: listings{
				registryv1.Service_PROTOCOL_UNSPECIFIED: {},
				listHTTP:                                {"ns/web": {row("10.0.0.1", 8080, nil, classes{8080: classHTTP, 9000: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED})}},
			},
		},
		{
			name: "a pod in all three listings",
			in: listings{
				listHTTP: {"ns/all": {row("10.0.0.1", 8080, nil, classes{8080: classHTTP, 9000: classTCP, 5353: classUDP})}},
				listTCP:  {"ns/all": {row("10.0.0.1", 8080, nil, classes{8080: classHTTP, 9000: classTCP, 5353: classUDP})}},
				listUDP:  {"ns/all": {row("10.0.0.1", 8080, nil, classes{8080: classHTTP, 9000: classTCP, 5353: classUDP})}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			RequirePerPortProtocols(rec, tt.in)
			if len(rec.errors) != len(tt.want) {
				t.Fatalf("got %d failures, want %d: %q", len(rec.errors), len(tt.want), rec.errors)
			}
			for i, want := range tt.want {
				if !strings.Contains(rec.errors[i], want) {
					t.Errorf("failure %d = %q, want it to contain %q", i, rec.errors[i], want)
				}
			}
		})
	}
}

// TestRequireNamespaceQualifiedKeysRejects is the negative control of the
// keying contract, which had none: a bare key and a cross-namespace endpoint
// must each fail it.
func TestRequireNamespaceQualifiedKeysRejects(t *testing.T) {
	ok := &recorder{}
	RequireNamespaceQualifiedKeys(ok, map[string][]*registryv1.ServiceEndpoint{
		"team-a/web": {{Ip: "10.0.0.1", KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "team-a"}}},
	})
	if len(ok.errors) != 0 {
		t.Fatalf("a namespace-qualified listing failed the contract: %q", ok.errors)
	}

	bad := &recorder{}
	RequireNamespaceQualifiedKeys(bad, map[string][]*registryv1.ServiceEndpoint{
		"web":        {{Ip: "10.0.0.1"}},
		"team-a/web": {{Ip: "10.0.0.2", KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "team-b"}}},
	})
	if len(bad.errors) != 2 {
		t.Fatalf("got %d failures, want 2 (a bare key, a cross-namespace endpoint): %q", len(bad.errors), bad.errors)
	}
}
