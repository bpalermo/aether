package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestNodeInternalIP(t *testing.T) {
	tests := []struct {
		name      string
		addresses []corev1.NodeAddress
		want      string
	}{
		{
			name: "internal IP present among others",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-1"},
				{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.7"},
			},
			want: "10.0.0.7",
		},
		{
			name:      "no internal IP",
			addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "1.2.3.4"}},
			want:      "",
		},
		{
			name:      "no addresses",
			addresses: nil,
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{Status: corev1.NodeStatus{Addresses: tt.addresses}}
			assert.Equal(t, tt.want, nodeInternalIP(node))
		})
	}
}
