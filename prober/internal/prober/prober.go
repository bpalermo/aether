// Package prober is a synthetic mesh-availability prober (proposal 013). It runs
// as a per-node DaemonSet, mesh-managed like any client, and probes the mesh data
// plane from the client side — primarily a proxy local-reply liveness endpoint
// (direct_response, no upstream, no app) so a failure is unambiguously the mesh's
// fault. It emits its OWN pass/fail counter, so it records the connection-level
// failures the proxy-emitted aether_stats metric cannot (the source proxy can't
// report its own outage).
package prober

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const (
	telemetryServiceName = "aether-prober"
	otlpTimeout          = 10 * time.Second
	shutdownTimeout      = 5 * time.Second

	tierLiveness     = "liveness"
	tierReachability = "reachability"

	resultSuccess         = "success"
	resultHTTPError       = "http_error"
	resultConnectionError = "connection_error"
	resultTimeout         = "timeout"
	resultSaturated       = "saturated"
)

// Config configures the prober.
type Config struct {
	Egress              string
	LivenessPath        string
	LivenessAuthority   string
	MeshDomain          string
	ReachabilityTargets []string
	Rate                float64
	Timeout             time.Duration
	MaxConcurrent       int
	OTLPEndpoint        string
}

// DefaultConfig returns the default prober configuration.
func DefaultConfig() Config {
	return Config{
		Egress:            "127.0.0.1:18081",
		LivenessPath:      "/-/-/live",
		LivenessAuthority: "liveness.aether.internal",
		MeshDomain:        "aether.internal",
		Rate:              5,
		Timeout:           2 * time.Second,
		MaxConcurrent:     16,
	}
}

type target struct {
	tier      string
	name      string // metric "target" label
	url       string
	authority string
}

// Prober samples the mesh data plane and records results to OTel.
type Prober struct {
	cfg      Config
	log      logr.Logger
	client   *http.Client
	provider *sdkmetric.MeterProvider // nil when telemetry is disabled
	counter  metric.Int64Counter
	duration metric.Float64Histogram
	targets  []target
}

// New builds a Prober.
func New(ctx context.Context, cfg Config, log logr.Logger, version string) (*Prober, error) {
	meter, provider, err := newMeter(ctx, cfg.OTLPEndpoint, version)
	if err != nil {
		return nil, err
	}
	counter, err := meter.Int64Counter("aether_probe_requests_total",
		metric.WithDescription("Synthetic mesh probe results by tier/target/result."))
	if err != nil {
		return nil, fmt.Errorf("probe counter: %w", err)
	}
	duration, err := meter.Float64Histogram("aether_probe_request_duration_seconds",
		metric.WithDescription("Synthetic mesh probe duration."), metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("probe histogram: %w", err)
	}

	p := &Prober{
		cfg: cfg, log: log, counter: counter, duration: duration, provider: provider,
		client: &http.Client{
			// No client.Timeout: the per-probe deadline is a context so we can tell
			// timeout from connection error. Don't follow redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	// Liveness tier (always): hit the proxy's egress local-reply route. No upstream
	// is involved, so this needs no config.aether.io/upstreams authorization.
	p.targets = append(p.targets, target{
		tier:      tierLiveness,
		name:      "egress",
		url:       "http://" + cfg.Egress + cfg.LivenessPath,
		authority: cfg.LivenessAuthority,
	})
	// Reachability tier (optional): full round-trip to an echo upstream.
	for _, svc := range cfg.ReachabilityTargets {
		p.targets = append(p.targets, target{
			tier:      tierReachability,
			name:      svc,
			url:       "http://" + cfg.Egress + "/",
			authority: svc + "." + cfg.MeshDomain,
		})
	}
	return p, nil
}

func newMeter(ctx context.Context, endpoint, version string) (metric.Meter, *sdkmetric.MeterProvider, error) {
	if endpoint == "" {
		return noop.NewMeterProvider().Meter(telemetryServiceName), nil, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(telemetryServiceName),
			semconv.ServiceVersion(version),
		),
		resource.WithFromEnv(), // picks up OTEL_RESOURCE_ATTRIBUTES (k8s.node.name)
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource: %w", err)
	}
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithTimeout(otlpTimeout),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	return mp.Meter(telemetryServiceName), mp, nil
}

// Run samples all targets until ctx is cancelled, then flushes telemetry.
func (p *Prober) Run(ctx context.Context) error {
	p.log.Info("prober starting", "targets", len(p.targets), "rate", p.cfg.Rate, "egress", p.cfg.Egress)
	var wg sync.WaitGroup
	for _, t := range p.targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			p.runTarget(ctx, t)
		}(t)
	}
	wg.Wait()
	if p.provider != nil {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := p.provider.Shutdown(sctx); err != nil {
			p.log.Error(err, "telemetry shutdown")
		}
	}
	return nil
}

func (p *Prober) runTarget(ctx context.Context, t target) {
	interval := time.Second
	if p.cfg.Rate > 0 {
		interval = time.Duration(float64(time.Second) / p.cfg.Rate)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Bound in-flight probes so a hung hop never blocks the open-loop ticker.
	sem := make(chan struct{}, max(1, p.cfg.MaxConcurrent))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case sem <- struct{}{}:
				go func() {
					defer func() { <-sem }()
					p.probe(ctx, t)
				}()
			default:
				p.record(t, resultSaturated, 0)
			}
		}
	}
}

func (p *Prober) probe(ctx context.Context, t target) {
	rctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, t.url, nil)
	if err != nil {
		p.record(t, resultConnectionError, 0)
		return
	}
	req.Host = t.authority
	// Mark the probe not-sampled so the mesh proxy's HCM doesn't create and export
	// a span per probe. The proxy's tracing is parent-based: a request that already
	// carries a trace context honors its sampled decision instead of rolling
	// random_sampling, so a cleared flag suppresses the span. Without this, at the
	// probe rate with proxy tracing enabled every probe becomes a synthetic
	// liveness-probe trace in Tempo (the access-log health-check exclusion has no
	// tracing equivalent for the direct_response liveness route).
	req.Header.Set("traceparent", notSampledTraceparent())
	start := time.Now()
	resp, err := p.client.Do(req)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		p.record(t, classifyErr(rctx, err), elapsed)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		p.record(t, resultSuccess, elapsed)
		return
	}
	p.record(t, resultHTTPError, elapsed)
}

// notSampledTraceparent builds a W3C traceparent with the sampled flag clear
// ("-00"). Because the proxy honors a propagated decision over its own
// random_sampling, this keeps every probe out of the proxy's exported spans. The
// trace-id and parent-id are random: an all-zero trace-id is invalid per W3C and
// would be rejected, re-enabling sampling and defeating the suppression.
func notSampledTraceparent() string {
	var buf [24]byte // 16-byte trace-id followed by 8-byte parent-id
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read does not fail on supported platforms; fall back to a
		// fixed nonzero id so the cleared flag is still honored.
		return "00-0000000000000000000000000000ace0-00000000000000a1-00"
	}
	return "00-" + hex.EncodeToString(buf[0:16]) + "-" + hex.EncodeToString(buf[16:24]) + "-00"
}

// classifyErr maps a request error to a result. connection_error is the class the
// proxy-emitted metric is blind to (no response produced / listener down).
func classifyErr(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return resultTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return resultTimeout
	}
	return resultConnectionError
}

func (p *Prober) record(t target, result string, elapsed float64) {
	attrs := metric.WithAttributes(
		attribute.String("tier", t.tier),
		attribute.String("target", t.name),
		attribute.String("result", result),
	)
	p.counter.Add(context.Background(), 1, attrs)
	if elapsed > 0 {
		p.duration.Record(context.Background(), elapsed, attrs)
	}
}
