# Day 37 — Tracing: OpenTelemetry + Tempo

> **Time:** ~4 h · **Builds on:** Days 35, 36 (and Day 1's image build)

## Objectives

- Explain spans, traces, and W3C context propagation — and the question only traces can answer
- Deploy Tempo as an ArgoCD app and add the Tempo datasource via GitOps
- Instrument podlab with the OpenTelemetry Go SDK (a real, guided code change), ship it as `podlab:v2-traced` through GitOps
- Cross-check a span's duration against the `duration_ms` in its matching JSON log line via the shared `trace_id`

## Concepts

### The question metrics and logs can't answer

Your Day 36 drill nailed *what/where/which*. Now imagine podlab calls guestbook-api, which calls Postgres, and a user reports one slow request: **"where did this request spend its 800ms?"** Metrics aggregate — your p95 panel can't isolate one request. Logs are per-service islands — three services logged three lines about it, with nothing connecting them and no record that *this* guestbook call belonged to *that* podlab request. You'd need every hop of one request stitched together with timings. That's a **trace**.

- A **span** is one timed operation: name, start, duration, attributes (`http.route`, `http.status_code`), status, and a parent span ID.
- A **trace** is the tree of spans sharing one `trace_id` — a request's life across every service it touched. Reading one is debugging-by-waterfall: the 800ms is *visibly* inside the `SELECT` span, or spread across 40 sequential calls that should've been one batch.

### Context propagation — the part interviews probe

Spans are emitted independently by each service. They form one tree only because the calling service sends its trace context along with the request. The W3C standard header:

```text
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
              |  trace_id (32 hex)               | parent span id  |sampled
```

Service A starts a span, injects `traceparent` into the outbound HTTP call; service B extracts it and parents its own span underneath. No header, no trace — a single uninstrumented hop (or a proxy that strips headers) cuts the tree at that point. "Context propagation" = inject + extract, and the otelhttp middleware you'll add does both automatically.

### OpenTelemetry: three things wearing one name

| Piece | Lives | Job |
|---|---|---|
| **API/SDK** | *in your code* | create spans, manage context; SDK adds batching, sampling, exporting |
| **OTLP** | the wire | the vendor-neutral protocol (gRPC :4317 / HTTP :4318) for telemetry |
| **Collector** | the pipeline | receive → process (batch, sample, redact) → export, outside the app |

OTel won the instrumentation war: instrument once against the vendor-neutral API, point OTLP wherever you like — Tempo today, Jaeger or a SaaS tomorrow — with zero code change. (OTel covers metrics and logs too; we use it for traces and keep our Prometheus/Loki pipelines.)

### Tempo: Loki's philosophy applied to traces

Jaeger-with-Elasticsearch indexes every span attribute. Tempo indexes almost nothing: traces go to cheap (object) storage, retrieval is by trace ID, and **TraceQL** searches recent data by scanning — same bet as Loki, same economics. You rarely *browse* traces anyway; you arrive holding an ID from a log line or an exemplar.

The production-shaped path is `app → OTLP → Collector/Alloy → Tempo`, giving you a central place for sampling and enrichment. Today we go **`app → OTLP → Tempo` direct** — the Tempo chart's distributor already listens on 4317/4318, so it's the least-config path; adding Alloy in the middle later changes one env var.

## Lab

### Part 1 — infrastructure

**1. Tempo as an ArgoCD app.** Note the chart moved to the grafana-community repo (`helm repo add grafana-community https://grafana-community.github.io/helm-charts`). Create `argocd/apps/tempo.yaml`: ns `monitoring`, single binary, small persistence, 24h retention.

<details><summary>Solution</summary>

```yaml
# argocd/apps/tempo.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: tempo
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://grafana-community.github.io/helm-charts   # chart's current home
    chart: tempo
    targetRevision: 2.2.2          # pin what `helm search repo grafana-community/tempo` shows
    helm:
      valuesObject:
        tempo:
          retention: 24h
          resources:
            requests: {cpu: 100m, memory: 256Mi}
            limits: {memory: 768Mi}
          # OTLP receivers on 4317 (gRPC) and 4318 (HTTP) are enabled by default
        persistence:
          enabled: true
          size: 3Gi
  destination:
    server: https://kubernetes.default.svc
    namespace: monitoring
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

</details>

```sh
git add argocd/apps/tempo.yaml && git commit -m "Add Tempo" && git push
argocd app sync root && argocd app wait tempo --health
kubectl get svc -n monitoring tempo   # ports include 3200 (query), 4317, 4318
```

**2. Datasources via GitOps** — extend `grafana.additionalDataSources` in `argocd/apps/monitoring.yaml`: add Tempo, and give the Day 35 Loki entry a **derived field** that turns the `trace_id` in podlab's logs into a click-through to Tempo:

```yaml
        grafana:
          additionalDataSources:
            - name: Loki
              type: loki
              uid: loki
              access: proxy
              url: http://loki.logging.svc:3100
              jsonData:
                derivedFields:
                  - name: TraceID
                    matcherRegex: '"trace_id":"(\w+)"'
                    datasourceUid: tempo
                    url: "$${__value.raw}"   # $$ escapes Grafana's env-var expansion
            - name: Tempo
              type: tempo
              uid: tempo
              access: proxy
              url: http://tempo.monitoring.svc:3200
```

(Derived-field config shifts slightly between Grafana majors; if the link doesn't render after Part 3, prototype it in the datasource UI and mirror the working JSON back into values — the traces→logs direction is in the stretch goals.)

Commit, push, sync.

### Part 2 — instrument podlab (the real work)

This is the course's one guided change to app code. The complete instrumented file is [`main.go`](main.go) in this folder — read it, but make the edits yourself in `apps/podlab/main.go`; they're small and each teaches something.

**1. Dependencies:**

```sh
cd ~/Code/cloud-engineer-course/apps/podlab
go get go.opentelemetry.io/otel \
       go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp \
       go.opentelemetry.io/otel/sdk \
       go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
go mod tidy        # may bump the go directive — fine, the Dockerfile builds with a newer toolchain
```

**2. The four edits** (full diff in the details below):

1. **`initTracer()`** — OTLP/HTTP exporter (`otlptracehttp.New(ctx)` reads `OTEL_EXPORTER_OTLP_ENDPOINT`; an `http://` scheme implies insecure), a `Resource` with `service.name=podlab` + `service.version=VERSION`, a batching `TracerProvider`, and the W3C `TraceContext` propagator. Returns the provider's `Shutdown` for flush-on-exit.
2. **Wrap the mux**: `handler = otelhttp.NewHandler(mux, "podlab", otelhttp.WithFilter(...))` — one server span per request, probes and `/metrics` filtered out. Gate the whole thing on the env var being set, so the image is still safe everywhere tracing isn't configured.
3. **`instrument()`** — append `trace_id` to the request log line via `trace.SpanContextFromContext(r.Context())`. Eight characters of code; it's the bridge between *all three* pillars.
4. **`handleError`** — start a manual child span `roll-the-dice` with an attribute and an error status, to see the span API and parent/child nesting (everything else gets spans "for free" from otelhttp; real code mixes both).

<details><summary>Solution — the changed regions (full file: <a href="main.go">main.go</a>)</summary>

```go
// imports added:
import (
    // ...existing...
    "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
    "go.opentelemetry.io/otel/trace"
)

func initTracer(ctx context.Context) (func(context.Context) error, error) {
    exporter, err := otlptracehttp.New(ctx) // endpoint from OTEL_EXPORTER_OTLP_ENDPOINT
    if err != nil {
        return nil, fmt.Errorf("otlp exporter: %w", err)
    }
    res := resource.NewWithAttributes(
        semconv.SchemaURL,
        semconv.ServiceName("podlab"),
        semconv.ServiceVersion(version),
    )
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(res),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{}, propagation.Baggage{}))
    return tp.Shutdown, nil
}

// in instrument(), before slog.Info — build attrs as []any, then:
    if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
        attrs = append(attrs, "trace_id", sc.TraceID().String())
    }
    slog.Info("request", attrs...)

// top of handleError:
    _, span := otel.Tracer("podlab").Start(r.Context(), "roll-the-dice")
    defer span.End()
    // ... and once rate is parsed / on failure:
    span.SetAttributes(attribute.Float64("podlab.error_rate", rate))
    span.SetStatus(codes.Error, "simulated failure")

// in main(), between mux setup and the server:
    var handler http.Handler = mux
    if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
        shutdown, err := initTracer(context.Background())
        if err != nil {
            slog.Error("tracing init failed, continuing without traces", "error", err)
        } else {
            defer shutdown(context.Background())
            handler = otelhttp.NewHandler(mux, "podlab",
                otelhttp.WithFilter(func(r *http.Request) bool {
                    return r.URL.Path != "/healthz" && r.URL.Path != "/metrics"
                }))
            slog.Info("tracing enabled", "endpoint", endpoint)
        }
    }
    server := &http.Server{Addr: ":" + port, Handler: handler}
```

</details>

**3. Build and load:**

```sh
docker build -t podlab:v2-traced .
kind load docker-image podlab:v2-traced --name course
```

**4. Deploy through Git** — in the **prod overlay** of k8s-gitops: bump the image tag and set the endpoint (paths per your Day 22/27 layout):

```yaml
# podlab/overlays/prod/kustomization.yaml
images:
  - name: podlab
    newTag: v2-traced
```

```yaml
# env patch in the prod overlay (add to the existing patch or a new one)
- op: add
  path: /spec/template/spec/containers/0/env/-
  value:
    name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://tempo.monitoring.svc:4318
```

Commit, push, sync — prod rolls to the traced build while dev/stage stay on v1. A GitOps deploy of your own instrumented code.

### Part 3 — see your spans

Traffic: `HOST=podlab-prod.localhost ERROR_RATE=0.3 ../day-31-promql/traffic.sh`, plus a few slow ones: `curl "http://podlab-prod.localhost:8080/load?seconds=2"`.

Grafana → **Explore → Tempo** → TraceQL:

```traceql
{resource.service.name="podlab"}
{resource.service.name="podlab" && span:status=error}
{resource.service.name="podlab" && name="roll-the-dice"}
```

Open a trace: the `podlab` server span with `http.method`, `http.route` (or `http.target`), `http.status_code`, and your `service.version=v2-traced` resource attribute — and under `/error` requests, the nested `roll-the-dice` child with `podlab.error_rate` and red error status.

**Close the loop** — Explore → Loki:

```logql
{namespace="podlab-prod"} | json | trace_id != ""
```

Every request log now carries `trace_id`. Expand a line: the **TraceID derived field** renders as a button → click → that exact trace opens in Tempo. Compare the server span's duration to the line's `duration_ms` — they should agree within a millisecond or two (otelhttp measures from slightly outside `instrument()`). Same request, three independent signals, one ID.

**Honest scoping:** a single-service trace is a one-span tree (plus your manual child) — it proves the pipeline but undersells the value. Tracing pays off at hop *two*: when service A calls service B with the `traceparent` header, the waterfall appears. That's the first stretch goal, and it's worth the time.

## Verify ✅

- [ ] `kubectl get pods -n podlab-prod -o jsonpath='{.items[0].spec.containers[0].image}'` → `podlab:v2-traced`; pod logs show `"tracing enabled"`
- [ ] TraceQL `{resource.service.name="podlab"}` returns traces; a span shows `service.version=v2-traced` and HTTP attributes
- [ ] An `/error` trace contains the nested `roll-the-dice` span with error status
- [ ] A log line's `trace_id` opens the matching trace (derived-field button, or paste the ID into Tempo's TraceQL/Trace-ID search)
- [ ] That span's duration ≈ the log line's `duration_ms`
- [ ] dev/stage still run v1 untouched (`kubectl get deploy podlab -n podlab-dev -o jsonpath='{.spec.template.spec.containers[0].image}'`)

## Interview corner 💬

**"Metrics, logs, traces — when do you actually need traces?"**
Metrics tell you *that* something is wrong and where, cheaply, over time — alert on them. Logs tell you *what* happened inside one service — rich detail, no cross-service structure. Traces answer the question the other two structurally can't: where one specific request spent its time *across services*. If you run a monolith, you may never need them; from the second network hop on, latency debugging without traces is guesswork. The force-multiplier is the shared trace ID stamped into logs and exemplars, which turns three silos into one navigable system.

**"What is context propagation and what breaks it?"**
Each service emits its spans independently; they assemble into one trace only because the caller passes the trace context — trace ID, parent span ID, sampling decision — along with the request, standardized as the W3C `traceparent` header. The instrumented client *injects* it; the instrumented server *extracts* it and parents its spans underneath. It breaks at any uninstrumented hop: a service that doesn't forward the header, a proxy that strips it, or background work spawned without carrying the Go `context.Context`/equivalent — each produces orphaned trace fragments instead of one tree.

**"Why OpenTelemetry rather than instrumenting for Jaeger/Datadog/X directly?"**
OTel separates the instrumentation API (in code, vendor-neutral, stable) from the export pipeline (OTLP protocol plus an optional Collector). Instrument once; the backend is config — we point OTLP at Tempo today and could re-point at any vendor without touching application code. The Collector adds an ops control point for sampling, batching, and redaction that doesn't live in app teams' codebases.

## Stretch goals

- **Service #2** (the real payoff): give guestbook-api an env var `PODLAB_URL`, have one endpoint call podlab using an `otelhttp.NewTransport(http.DefaultTransport)` client, instrument it the same way — then find a *two-service* trace and admire the waterfall.
- Production-shape the pipeline: add `otelcol.receiver.otlp` → `otelcol.exporter.otlp` (→ Tempo) components to Day 35's Alloy config, expose 4317/4318 via `alloy.extraPorts`, and re-point podlab's `OTEL_EXPORTER_OTLP_ENDPOINT` at Alloy.
- Configure the Tempo datasource's *traces→logs* link (`tracesToLogsV2` in `jsonData`, target the `loki` uid) so the jump works in both directions.
- Enable exemplars end-to-end (Day 36 stretch + `OTEL_METRICS_EXEMPLAR_FILTER`) and click from a latency panel dot straight into a trace.

## Cleanup

Nothing to delete — Tempo, the datasources, and `podlab:v2-traced` in prod **stay through Day 50**: Day 39's canary work and the Day 49 capstone demo use this exact setup. The whole observability platform you built in Phase 5 — Prometheus, Alertmanager, Grafana, Loki, Alloy, Tempo — is now a permanent resident of the cluster. Stop the traffic loops.
