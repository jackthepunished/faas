# Trace propagation — `TRACEPARENT` for your handler

The one-box FaaS platform stamps every incoming request with a W3C
[`traceparent`](https://www.w3.org/TR/trace-context/) header and
forwards it to your function in the `TRACEPARENT` environment
variable (issue #555 layer 4). You can opt into OpenTelemetry
auto-instrumentation and the platform's trace will join your
spans, all the way to the OTLP collector you point at
`OTEL_EXPORTER_OTLP_ENDPOINT`.

This page is the operator's quick-start; the spec contract is in
`docs/faas_implementation_spec.md` §16 (tracing).

## What the platform gives you

- **Header name (HTTP)**: `traceparent` — the standard W3C name.
  The one-box gateway already accepts and forwards it.
- **Env var (runner)**: `TRACEPARENT` — same value, set on every
  request to your handler. Format is
  `00-<trace_id 32 hex>-<span_id 16 hex>-<flags 2 hex>`, e.g.
  `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`.
- **Lifetime**: the trace_id is minted at the gateway (or carried
  in from the inbound `traceparent`); the span_id identifies the
  specific `gateway.handler` span. A new trace is minted on every
  cold boot; warm wake reuses the live span.
- **Other env vars (no action required)**: `TRACESTATE` and
  `BAGGAGE` are also stamped when present. They are empty for
  internal traffic.

You do not need to read or write `TRACEPARENT` for the platform's
own spans to work — the platform's `sched.wake`, `vmmd.create_*`,
`guest.resume`, and `guest.readiness` spans are joined on the same
trace_id automatically (issue #555 layer 3, merged).

## Auto-instrumentation: Node 22 / 24

Add the OTel SDK and the auto-instrumentation hooks to your app's
`dependencies`, then opt the handler into env propagation. The
auto-instrumentation reads `TRACEPARENT` from the process env and
joins every outbound span to the same trace.

```json
// package.json
{
  "dependencies": {
    "@opentelemetry/api": "^1.9.0",
    "@opentelemetry/auto-instrumentations-node": "^0.52.0",
    "@opentelemetry/exporter-trace-otlp-http": "^0.55.0",
    "@opentelemetry/sdk-node": "^0.55.0"
  }
}
```

```js
// tracing.js — required: OTel SDK must be initialized BEFORE any
// instrumented module (express, http, pg, etc.) is required.
const { NodeSDK } = require('@opentelemetry/sdk-node');
const { getNodeAutoInstrumentations } = require('@opentelemetry/auto-instrumentations-node');
const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-http');
const { Resource } = require('@opentelemetry/resources');

// OTEL_PROPAGATORS picks up TRACEPARENT from the process env and
// joins every outbound span to the platform's trace. The default
// "tracecontext,baggage" already does this; we list it explicitly
// so a misconfiguration that overrides OTEL_PROPAGATORS doesn't
// silently drop traceparent propagation.
const propagators = (process.env.OTEL_PROPAGATORS || 'tracecontext,baggage')
  .split(',')
  .map((name) => name.trim());

const sdk = new NodeSDK({
  resource: new Resource({ 'service.name': process.env.FAAS_APP_SLUG || 'app' }),
  traceExporter: new OTLPTraceExporter({
    // The platform forwards the env var to the runner; you can
    // override via app.json's `env` block.
    url: process.env.OTEL_EXPORTER_OTLP_ENDPOINT,
  }),
  instrumentations: [getNodeAutoInstrumentations()],
});
sdk.start();
process.on('SIGTERM', () => sdk.shutdown());
```

> **Why this works:** `NodeSDK` wires an `EnvMapPropagator` (the
> `OTEL_PROPAGATORS=tracecontext` propagator) that reads `TRACEPARENT`
> from `process.env` on every span start. The platform stamps
> `TRACEPARENT` per request, so each child span joins the
> `gateway.handler` trace_id automatically — no manual extraction
> needed. If you set `OTEL_PROPAGATORS` yourself, keep `tracecontext`
> in the list, or the join breaks silently.

```js
// handler.js — your existing handler, unchanged. The auto-
// instrumentation will create child spans for each HTTP route,
// database call, and outbound fetch under the platform's
// trace_id.
const express = require('express');
const app = express();
app.get('/', (req, res) => res.send('hi'));
app.listen(process.env.PORT || 8080);
```

```json
// app.json — pin the OTLP endpoint for your collector
{
  "env": {
    "OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel-collector.faas.svc:4318"
  }
}
```

That's it. A request to your function now shows up in your
collector as a single trace with one parent (`gateway.handler`)
and a tree of child spans: `http.server` (your route handler) →
`pg.query` (if you hit a database) → `http.client` (any outbound
fetch).

## Auto-instrumentation: Python 3.12 / 3.13

```toml
# pyproject.toml
[project]
dependencies = [
  "opentelemetry-distro[otlp]>=0.48b0",
  "opentelemetry-instrumentation>=0.48b0",
]
```

```bash
# build step
pip install opentelemetry-bootstrap
opentelemetry-bootstrap -a install
```

```bash
# Procfile or app.json's `command` — bootstrap must run BEFORE
# your handler imports.
export OTEL_PROPAGATORS=tracecontext,baggage
exec opentelemetry-instrument \
  --service_name "${FAAS_APP_SLUG}" \
  --exporter_otlp_endpoint "${OTEL_EXPORTER_OTLP_ENDPOINT}" \
  --exporter_otlp_protocol http/protobuf \
  gunicorn app:app
```

The `opentelemetry-instrument` wrapper installs an
`OTELPropagatorsEnv` (an EnvMapPropagator equivalent) that reads
`TRACEPARENT` from the process env and joins every Flask/FastAPI/
Django/psycopg span to the platform's trace. We set
`OTEL_PROPAGATORS=tracecontext,baggage` explicitly so a custom
propagator in the parent image doesn't silently drop the join.

## What the platform does NOT do

- **No library pre-installation.** The runner image ships with
  the standard library only. You bring your own OTel SDK. The
  runner sandbox is small (130 MB fleet target, ADR-040); we do
  not pay for a 30 MB SDK on disk per app when most apps will
  not opt in.
- **No auto-detection.** Set `OTEL_EXPORTER_OTLP_ENDPOINT` in
  `app.json`'s `env` (or rely on the platform's default if the
  operator has set one at the cluster level) to turn on export.
  Without it, spans are still joined to the platform's
  `gateway.handler` trace and visible via
  `GET /v1/traces/{trace_id}` — you just don't get the
  collector-side view.
- **No head-based sampling override.** The platform samples
  100% for the first 100 root spans of every new deployment
  (acceptance #5), then falls back to the head ratio in
  `OTEL_TRACES_SAMPLER_ARG` (default 1.0). Your handler's
  child spans are NOT subject to the platform's sampler — the
  parent's `SampledFlag=true` is what reaches your SDK, so
  every child span you create is recorded.

## Cross-daemon trace query

If you have observer access to the box, `GET /v1/traces/{trace_id}`
returns the full span tree for any wake within the last 24 hours
(PR #617 ring buffer, default 100k entries). The shape is the
[`Trace` schema](../../api/openapi.yaml) — every span carries
`trace_id`, `span_id`, `parent_span_id`, `name`, `start_time`,
`end_time`, `status`, and an `attributes` map (`app_id`,
`deployment_id`, `instance_id`, etc.).

```bash
curl -sH "X-Faas-Trace-Auth: $OBSERVER_TOKEN" \
  https://faas.example.com/v1/traces/4bf92f3577b34da6a3ce929d0e0e4736 | jq
```

## See also

- `docs/faas_implementation_spec.md` §16 — tracing contract
- `docs/adr/` — architectural decisions (PR #617 / issue #555)
- W3C TraceContext: https://www.w3.org/TR/trace-context/
