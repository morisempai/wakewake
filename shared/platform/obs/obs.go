// Package obs is the single OpenTelemetry bootstrap every service shares.
//
// It exists for the same reason the logger and correlation packages do: if five services each
// stand up their own TracerProvider, they drift on sampler, resource attributes, and propagator,
// and the drift is invisible until traces silently fail to join. ADR-0013 records the decisions
// this package implements.
//
// A service wires observability in three places:
//
//	shutdown, err := obs.Init(ctx, obs.Config{Service: "booking", Endpoint: otlpEndpoint, Insecure: true})
//	defer shutdown(context.Background())
//	// server, outermost middleware first — the order is load-bearing (see Handler):
//	h := obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(mux)), "booking")
//	// client:
//	client := &http.Client{Transport: obs.RoundTripper(nil)}
//
// With no Endpoint configured, spans are still recorded (so trace_id appears in logs) but never
// exported — which is what makes traces visible in the dev inner loop where the collector is down.
package obs

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// Config configures the OTel bootstrap. Service is the only required field.
type Config struct {
	// Service is the service.name resource attribute — how this process shows up in Tempo and
	// on every span. Required.
	Service string

	// Endpoint is the OTLP/gRPC collector address. It accepts either the OTel-standard URL form
	// ("http://otel-collector:4317", "https://collector:4317") or a bare "host:port". With a URL,
	// the scheme decides TLS (http => insecure, https => secure) and Insecure is ignored.
	// Empty means "record spans but export nothing": trace_id and span_id still reach the logs,
	// they are simply not shipped. This is the dev-inner-loop default where the collector is down.
	Endpoint string

	// Insecure disables TLS on the exporter connection when Endpoint is a bare host:port (a URL's
	// scheme takes precedence). The dev collector has no TLS; real environments set this false.
	Insecure bool

	// SampleRatio in the open interval (0,1) samples that fraction of root traces. Any other
	// value (the zero value included) means always sample — the right default for dev, and the
	// place a production ratio is dialled in against SLOs (ADR-0007 deferred the number to M4).
	SampleRatio float64

	// ResourceAttrs are extra resource attributes (e.g. deployment.environment, service.version)
	// stamped on every span emitted by this process.
	ResourceAttrs map[string]string
}

// Init builds the process-wide TracerProvider, installs it and the W3C propagator as the OTel
// globals, and returns a shutdown function to defer. It never blocks on or fails because of an
// unreachable collector: the gRPC exporter connects lazily and drops spans if the collector is
// down, so a missing observability backend never takes a service down with it.
func Init(ctx context.Context, c Config) (func(context.Context) error, error) {
	if c.Service == "" {
		return nil, fmt.Errorf("obs: Config.Service is required")
	}

	// Set the propagator unconditionally — even with no exporter, an inbound traceparent must be
	// honored so log lines carry the upstream trace_id.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	attrs := []attribute.KeyValue{attribute.String("service.name", c.Service)}
	for k, v := range c.ResourceAttrs {
		attrs = append(attrs, attribute.String(k, v))
	}
	res := resource.NewSchemaless(attrs...)

	sampler := sdktrace.ParentBased(sdktrace.AlwaysSample())
	if c.SampleRatio > 0 && c.SampleRatio < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}

	if c.Endpoint != "" {
		var grpcOpts []otlptracegrpc.Option
		if strings.Contains(c.Endpoint, "://") {
			// OTel-standard URL form: the scheme decides TLS.
			grpcOpts = append(grpcOpts, otlptracegrpc.WithEndpointURL(c.Endpoint))
		} else {
			grpcOpts = append(grpcOpts, otlptracegrpc.WithEndpoint(c.Endpoint))
			if c.Insecure {
				grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
			}
		}
		exp, err := otlptracegrpc.New(ctx, grpcOpts...)
		if err != nil {
			return nil, fmt.Errorf("obs: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// Handler wraps next with OTel HTTP server instrumentation. It MUST be the outermost middleware,
// with correlation and logging inside it:
//
//	obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(mux)), service)
//
// The server span has to start before correlation minting and logging run, or the log lines those
// inner layers emit will not carry a trace_id — which looks fine in tests and silently breaks
// trace↔log correlation in Grafana. See ADR-0013.
func Handler(next http.Handler, service string) http.Handler {
	return otelhttp.NewHandler(next, service,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// RoundTripper returns an http.RoundTripper that propagates BOTH W3C trace context (traceparent)
// and the X-Correlation-Id header on every outbound request. Wrap every service-to-service client
// (and the gateway proxy transport) with it — a bare correlation.RoundTripper propagates the
// correlation id but breaks the trace at the first hop.
func RoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(correlation.RoundTripper{Base: base})
}
