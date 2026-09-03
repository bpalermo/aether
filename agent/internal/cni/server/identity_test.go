package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const spiffeIDOverrideMetric = "aether.agent.identity.spiffe_id_override_rejected"

// TestReportRejectedSpiffeIDOverride is the observability half of #669: the
// annotation is ignored, but never silently — a pod carrying it must produce a
// WARN carrying both the requested and the actually-used identity, plus one
// counter increment.
func TestReportRejectedSpiffeIDOverride(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantLog     bool
		wantCount   int64
	}{
		{name: "no annotations"},
		{name: "unrelated annotation", annotations: map[string]string{"config.aether.io/upstreams": "svc-a"}},
		{name: "empty annotation", annotations: map[string]string{xdsconst.AnnotationSpiffeID: ""}},
		{
			name:        "override rejected",
			annotations: map[string]string{xdsconst.AnnotationSpiffeID: "spiffe://example.org/ns/prod/sa/payments"},
			wantLog:     true,
			wantCount:   1,
		},
		{
			name:        "override restating the derived value is still reported",
			annotations: map[string]string{xdsconst.AnnotationSpiffeID: "spiffe://example.org/ns/tenant-a/sa/worker"},
			wantLog:     true,
			wantCount:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			metrics, reader := newTestCNIMetrics(t)
			s := &CNIServer{
				log:         slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				trustDomain: "example.org",
				metrics:     metrics,
			}
			pod := &cniv1.CNIPod{
				Name:           "worker-0",
				Namespace:      "tenant-a",
				ServiceAccount: "worker",
				Annotations:    tt.annotations,
			}

			s.reportRejectedSpiffeIDOverride(context.Background(), s.log, pod)

			got, found := metricSum(t, reader, spiffeIDOverrideMetric)
			if !tt.wantLog {
				assert.Empty(t, buf.String(), "no annotation must produce no log line")
				assert.False(t, found, "no annotation must not touch the counter")
				return
			}

			line := buf.String()
			require.NotEmpty(t, line, "a rejected override must be logged")
			assert.Contains(t, line, `"level":"WARN"`, "the rejection must be at WARN")
			assert.Contains(t, line, tt.annotations[xdsconst.AnnotationSpiffeID], "the requested identity is logged for attribution")
			assert.Contains(t, line, "spiffe://example.org/ns/tenant-a/sa/worker", "the identity actually used is logged")
			assert.Contains(t, line, xdsconst.AnnotationSpiffeID)
			assert.True(t, strings.Contains(line, "rejected"), "the message must say the override was rejected")
			assert.Equal(t, tt.wantCount, got, "%s", spiffeIDOverrideMetric)
		})
	}
}

// TestReportRejectedSpiffeIDOverride_NilMetrics covers the telemetry-disabled
// build: the report path must not panic without instruments.
func TestReportRejectedSpiffeIDOverride_NilMetrics(t *testing.T) {
	s := &CNIServer{log: slog.New(slog.DiscardHandler), trustDomain: "example.org"}
	s.reportRejectedSpiffeIDOverride(context.Background(), s.log, &cniv1.CNIPod{
		Namespace:      "tenant-a",
		ServiceAccount: "worker",
		Annotations:    map[string]string{xdsconst.AnnotationSpiffeID: "spiffe://evil.example/ns/prod/sa/payments"},
	})
}
