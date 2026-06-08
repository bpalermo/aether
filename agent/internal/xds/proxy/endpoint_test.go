package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClusterLoadAssignment(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{name: "standard", serviceName: "my-service"},
		{name: "empty", serviceName: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cla := NewClusterLoadAssignment(tt.serviceName)
			require.NotNil(t, cla)
			assert.Equal(t, tt.serviceName, cla.GetClusterName())
			assert.Empty(t, cla.GetEndpoints())
		})
	}
}
