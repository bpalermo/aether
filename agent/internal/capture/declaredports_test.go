package capture

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// TestDeclaredPorts: a generated Service's REAL ports are the ones the
// registrar names "p<port>-tcp" / "p<port>-udp". The HTTP ones are those with
// appProtocol http over TCP; every other real port is "other". The mesh's own
// two ports and anything else on the Service are not ports of the service.
func TestDeclaredPorts(t *testing.T) {
	app := func(s string) *string { return &s }
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "mesh", Port: 18081},
		{Name: "mesh-tcp", Port: 18082},
		{Name: "p9090-tcp", Port: 9090, AppProtocol: app("http")},
		{Name: "p8080-tcp", Port: 8080, AppProtocol: app("http"), Protocol: corev1.ProtocolTCP},
		{Name: "p9000-tcp", Port: 9000, AppProtocol: app("tcp")},
		{Name: "p5353-udp", Port: 5353, Protocol: corev1.ProtocolUDP},
		{Name: "p7000-tcp", Port: 7000},                    // no appProtocol: not known to be HTTP
		{Name: "http", Port: 80, AppProtocol: app("http")}, // an older registrar's placeholder
		{Name: "p8080-tcp", Port: 8080, AppProtocol: app("http")},
	}}}
	http, other := declaredPorts(svc)
	assert.Equal(t, []uint32{8080, 9090}, http, "sorted, once each")
	assert.Equal(t, []uint32{5353, 7000, 9000}, other)

	http, other = declaredPorts(&corev1.Service{})
	assert.Empty(t, http)
	assert.Empty(t, other)
}
