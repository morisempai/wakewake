# ADR-0013: OpenTelemetry runtime bootstrap and tracing conventions

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-29
- **Deciders:** SRE, Development (pending human sign-off)
- **Related stories/issues:** ADR-0007 (observability stack), #23 (correlation id across the Stripe webhook), M4 milestone

## Context

ADR-0007 committed to instrumenting every service with the OpenTelemetry SDK, exporting OTLP to a
collector that fans out to Tempo/Prometheus/Loki, and it deferred *"sampling and retention... defined
with SLOs in M4."* The stack it describes already exists in `infra/` and Grafana's Loki datasource is
pre-provisioned with the derived field `matcherRegex "trace_id":"(\w+)"` that links a log line to its
trace.

What does **not** exist is the runtime wiring. No service ever constructs a `TracerProvider`, so no
spans are produced and `trace_id` is never emitted — `shared/platform/logging` will stamp it *the
moment a valid span is in the context*, but nothing ever puts one there. `shared/platform/obs/` is an
empty placeholder reserved for exactly this bootstrap. The result today is that the whole Grafana
tracing surface is dark despite being fully built.

M4 (scoped by the maintainer to **tracing continuity**, metrics deferred) must supply that wiring:
one place that boots OTel, a canonical way to instrument an HTTP server and an HTTP client, and a
clear statement of how `trace_id` relates to the `correlation_id` we already thread everywhere.

Forces that shape the decision:

- **The dev inner loop runs the collector down.** The root `docker-compose.yml` deliberately does not
  start the Grafana stack. If the bootstrap fails hard without a collector, or refuses to record
  spans without an exporter, then `trace_id` never appears locally and developers debug blind.
- **`correlation_id` is the human-facing id; `trace_id` is the machine id.** A customer quotes a
  correlation id in a bug report; an engineer pivots on a trace id in Tempo. They are different
  identifiers with different lifetimes and must both survive — conflating them loses one audience.
- **The async event bus is not HTTP.** W3C trace context propagates naturally over HTTP headers, but
  the RabbitMQ envelope (a `contracts/` wire shape) carries `correlation_id` and *not* `traceparent`.
  Adding trace context to the envelope is an AsyncAPI contract change, out of scope for a milestone
  the maintainer scoped to "continuity, metrics deferred."
- **Middleware order is load-bearing and invisible when wrong.** Put the tracing middleware in the
  wrong position and every log line looks fine, every test passes, and no line carries a `trace_id`.

## Decision

**1. One bootstrap: `shared/platform/obs`.** A single `obs.Init(ctx, Config) (shutdown, error)` builds
the process-wide `TracerProvider`, sets the global tracer and propagator, and returns a shutdown
function services defer. Services never touch the OTel SDK directly; they call `obs.Init` in `main`
and use the two helpers below. This keeps the SDK version and configuration in one CODEOWNERS-gated
place, the same reasoning as the logger and correlation packages.

**2. OTLP/gRPC exporter to the collector, batched.** Export to `OTEL_EXPORTER_OTLP_ENDPOINT`
(default `otel-collector:4317`), TLS-insecure in dev, via a `BatchSpanProcessor`. gRPC because the
collector's OTLP gRPC receiver is already listening on 4317.

**3. Record even with no collector.** When no endpoint is configured, `Init` still installs a
*recording* `TracerProvider` with no exporter. Spans are created and sampled — so `trace_id` and
`span_id` appear in logs — they are simply never shipped. This is what makes traces visible in the
inner loop (collector down) and is the difference between "tracing works locally" and "tracing works
only in the full stack." A down collector never blocks or fails a service; the gRPC exporter connects
lazily and drops on the floor if the collector is unreachable.

**4. Sampling: `ParentBased(AlwaysSample)` by default, ratio-overridable.** Dev wants every trace.
Production sampling ratio is an ops knob (`Config.SampleRatio` / env), left at 1.0 here — ADR-0007
deferred the *number* to M4-with-SLOs, and we set the *mechanism* now while leaving the production
ratio to be tuned against real SLOs rather than guessed. `ParentBased` ensures a sampled upstream
request keeps its whole downstream trace.

**5. Propagation: W3C `tracecontext` + `baggage`.** `correlation_id` stays in its own
`X-Correlation-Id` header (ADR-0007, unchanged) — it is **not** folded into baggage. Both identifiers
land on every log line (`shared/platform/logging` already does this). We additionally stamp
`correlation_id` as a span attribute so Tempo can pivot from a trace to its logs, closing the loop in
both directions.

**6. Canonical HTTP wiring, with a fixed middleware order.**
- Server: `obs.Handler(next, serviceName)` wraps `otelhttp`. The required order, outermost → inner,
  is **`obs.Handler` → `correlation.Middleware` → `httpx.LogMiddleware` → the mux**. The server span
  must start before anything logs (so log lines carry `trace_id`), and correlation minting must sit
  inside the span. This ordering is documented on `obs.Handler` and asserted by a test.
- Client: `obs.RoundTripper(base)` = an `otelhttp` transport wrapping `correlation.RoundTripper`, so a
  single wrap propagates **both** `traceparent` and `X-Correlation-Id` on every outbound hop. The
  gateway proxy and any service-to-service client use this instead of a bare `correlation.RoundTripper`.

**7. HTTP hops share a trace; async hops share the correlation id.** Trace context is propagated over
HTTP only. Across a RabbitMQ publish/consume the trace does **not** continue in M4 — the consumer
starts a fresh trace — because that requires a `traceparent` field on the event envelope, an AsyncAPI
contract change deferred past this milestone. What *does* survive every async hop is `correlation_id`
(proven in M2, and #23 fixes its one remaining break at the Stripe webhook). So a booking is **one
`correlation_id` end-to-end** and **one `trace_id` per synchronous HTTP segment**, with the segments
stitched by the shared correlation id in Grafana. We state this rather than overclaim "one trace end
to end," which is not true until the envelope carries trace context.

**8. No metrics in M4.** `Init` does not construct a `MeterProvider`; the maintainer scoped M4 to
tracing continuity. RED metrics and dashboards are a named follow-on.

## Consequences

- (+) The already-built Grafana stack lights up: `trace_id` fires on every log line, and traces
  appear in Tempo, linked to logs by the pre-provisioned derived field — no infra change needed.
- (+) One bootstrap and two helpers mean a service instruments HTTP in/out with three lines and
  cannot get the SDK version or the middleware order wrong on its own.
- (+) Tracing is visible in the inner loop without running the Grafana stack (recording-without-export
  fallback), so developers get `trace_id` correlation for free.
- (+) `correlation_id` and `trace_id` stay distinct and both survive, serving the customer-facing and
  engineer-facing audiences respectively.
- (−) A booking is not a single Tempo trace end-to-end: the async event-bus hops break the trace, and
  the pieces are joined by `correlation_id` rather than by a parent span. Following a booking across
  the pay boundary in Tempo means pivoting on the correlation id, not clicking a child span.
- (−) Propagating trace context over the bus (envelope `traceparent`, span links on consume) is left
  as a follow-on and carries an AsyncAPI contract change when we take it on.
- (−) New operational surface: the OTLP exporter, batch processor, and a sampling ratio to tune
  against SLOs before production.

## Alternatives considered

- **Fold trace context into the event envelope now** — rejected for M4: it is an AsyncAPI contract
  change (a `traceparent`/`trace_context` field, new producer/consumer code in four services) and the
  milestone was scoped to continuity. Recorded as the explicit follow-on that turns "one trace per
  HTTP segment" into "one trace end-to-end."
- **Carry the correlation id in OTel baggage instead of a header** — rejected: baggage is machine
  plumbing and would couple the human-facing id to the tracing SDK. The `X-Correlation-Id` header is
  already the contract in every OpenAPI spec.
- **Fail the service if the collector is unreachable at boot** — rejected: it would make the dev inner
  loop (collector down) unusable and couple service liveness to an observability backend. Best-effort
  export with local recording is the safer default.
- **Let each service construct its own `TracerProvider`** — rejected for the same reason the logger
  and correlation live in `shared/platform`: five independent bootstraps drift on sampler, resource
  attributes, and propagator, and the drift is invisible until traces fail to join.
