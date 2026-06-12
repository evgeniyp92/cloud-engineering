# Day 35 — Loki & Alloy: the Log Pipeline

> **Time:** ~3.5 h · **Builds on:** Days 30, 32, 34

## Objectives

- Explain Loki's index-labels-only design and the tradeoff against Elasticsearch-style full-text indexing
- Deploy Loki (single binary) and Grafana Alloy as ArgoCD apps, with the datasource provisioned via GitOps
- Query podlab's structured JSON logs with LogQL: stream selectors, line filters, parsers, label filters
- Derive metrics from logs and cross-check them against Prometheus — two pillars, one truth

## Concepts

### Logs complete metrics

Day 33's alert tells you the error *rate* is up. Metrics can never tell you **which** error — `code="500"` is one label value covering a thousand different stack traces. Cardinality forbids putting the error message in a label. Logs are the opposite tradeoff: arbitrary detail per event, expensive to aggregate. The workflow is always: **metric says something's wrong → logs say what**. Day 36 wires that workflow into one screen.

### Why Loki ≠ Elasticsearch

Elasticsearch indexes (almost) **every token of every log line** — full-text search is instant, but the index often outweighs the data, ingestion burns CPU, and you pay for an index on petabytes you'll never query.

Loki's bet: **index only the labels, store the content as compressed chunks** (in object storage in real deployments). A log *stream* is a unique label set — `{namespace="podlab-prod", pod="podlab-abc", container="podlab"}` — and only those few labels are indexed. Queries work in two phases: the tiny index narrows to matching streams + time range, then Loki **brute-force greps the chunks** (parallelized).

Honest consequences:

| | Loki | Elasticsearch |
|---|---|---|
| storage cost | low (compressed chunks, minimal index) | high (index ≥ data) |
| "all logs for this pod, last hour" | instant | instant |
| "find this UUID across everything, 30 days" | slow — greps 30 days of chunks | fast — it's indexed |
| ingestion cost | trivial | heavy |
| operations | small footprint | a career |

If your dominant query is needle-in-haystack across long ranges with no label hints, Loki is the wrong tool — that honesty lands well in interviews. For Kubernetes ("logs of *this* service, *this* time window"), labels narrow everything anyway, and Loki is dramatically cheaper.

### Architecture modes & the collector

Loki ships the same binary in three deployment shapes: **single binary** (monolith — laptops and small prod up to tens of GB/day), **simple scalable** (read/write/backend split), and **microservices**. We run single binary with filesystem storage; the values change, not the concepts.

Logs reach Loki via a collector. **Grafana Alloy** is the current one (successor to promtail, which is feature-frozen). Alloy is a programmable telemetry pipeline; ours will be four components long:

```text
discovery.kubernetes (find pods via the API)
   -> discovery.relabel (keep namespace/pod/container as stream labels)
      -> loki.source.kubernetes (tail container logs via the kubelet API)
         -> loki.write (push to Loki)
```

`loki.source.kubernetes` tails logs through the Kubernetes API — no hostPath mounts into `/var/log/pods` (the classic promtail approach, still common; trade: API-tailing is simpler, file-tailing survives kubelet API hiccups and preserves position across restarts better). Running as a DaemonSet with **clustering** enabled, the Alloy instances distribute the pod list among themselves instead of each tailing everything.

### LogQL = label selector + pipeline

```logql
{namespace="podlab-prod"}                          # stream selector — REQUIRED, uses the index
  |= "error"                                       # line filter: cheap substring grep
  | json                                           # parser: JSON fields -> ephemeral labels
  | status >= 500                                  # label filter on a parsed field
  | line_format "{{.method}} {{.path}} {{.status}}"
```

Order matters for cost: line filters (`|=`, `!=`, `|~`) run on raw bytes — put them early. Parsers (`| json`, `| logfmt`) are per-line CPU — put them after the cheap filters cut volume.

And the bridge back to metrics — **log-derived metrics**:

```logql
sum(rate({namespace="podlab-prod"} | json | level="ERROR" [5m]))
```

A counter that nobody instrumented: the error rate *according to the logs*.

## Lab

### 1. Loki as an ArgoCD app

Create `argocd/apps/loki.yaml` in **k8s-gitops**. Requirements: chart `loki` from `https://grafana.github.io/helm-charts` (pin the version `helm search repo grafana/loki` shows), ns `logging` + CreateNamespace, sync-wave 0; values: SingleBinary mode, auth off, filesystem storage, tsdb schema, ~48h retention, **caches and canary disabled** (the chart's memcached defaults alone would eat your laptop), scalable components zeroed.

<details><summary>Solution</summary>

```yaml
# argocd/apps/loki.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: loki
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://grafana.github.io/helm-charts
    chart: loki
    targetRevision: 7.0.0            # pin what helm search shows today
    helm:
      valuesObject:
        deploymentMode: SingleBinary
        loki:
          auth_enabled: false        # single-tenant lab; multi-tenancy is Loki's auth model
          commonConfig:
            replication_factor: 1
          storage:
            type: filesystem         # real prod: S3/GCS via this same key
          schemaConfig:
            configs:
              - from: "2024-04-01"
                store: tsdb
                object_store: filesystem
                schema: v13
                index: {prefix: index_, period: 24h}
          limits_config:
            retention_period: 48h
          compactor:
            retention_enabled: true  # retention is the compactor's job
            delete_request_store: filesystem
        singleBinary:
          replicas: 1
          persistence: {size: 5Gi}
          resources:
            requests: {cpu: 100m, memory: 256Mi}
            limits: {memory: 1Gi}
        # zero out the scalable-mode components
        read: {replicas: 0}
        write: {replicas: 0}
        backend: {replicas: 0}
        gateway: {enabled: false}    # we'll talk to loki:3100 directly
        # laptop survival: these defaults are sized for real clusters
        chunksCache: {enabled: false}
        resultsCache: {enabled: false}
        lokiCanary: {enabled: false}
        test: {enabled: false}
  destination:
    server: https://kubernetes.default.svc
    namespace: logging
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions: [CreateNamespace=true]
```

</details>

### 2. Alloy as an ArgoCD app

`argocd/apps/alloy.yaml`: chart `alloy` from the same repo, ns `logging`, wave 1. The chart runs a DaemonSet by default and its default RBAC already covers `pods` and `pods/log`. Your work is the pipeline in `alloy.configMap.content` — write the four components from Concepts, keeping `namespace`, `pod`, `container`, `app`, and a `job` label, with clustering on.

<details><summary>Solution</summary>

```yaml
# argocd/apps/alloy.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: alloy
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  project: default
  source:
    repoURL: https://grafana.github.io/helm-charts
    chart: alloy
    targetRevision: 1.9.0            # pin current
    helm:
      valuesObject:
        alloy:
          clustering:
            enabled: true            # DaemonSet members split the tailing work
          configMap:
            content: |
              // 1. Discover every pod via the Kubernetes API.
              discovery.kubernetes "pods" {
                role = "pod"
              }

              // 2. Choose stream labels. FEW and BOUNDED — this set defines stream cardinality.
              discovery.relabel "pod_logs" {
                targets = discovery.kubernetes.pods.targets
                rule {
                  source_labels = ["__meta_kubernetes_namespace"]
                  target_label  = "namespace"
                }
                rule {
                  source_labels = ["__meta_kubernetes_pod_name"]
                  target_label  = "pod"
                }
                rule {
                  source_labels = ["__meta_kubernetes_pod_container_name"]
                  target_label  = "container"
                }
                rule {
                  source_labels = ["__meta_kubernetes_pod_label_app"]
                  target_label  = "app"
                }
                rule {
                  source_labels = ["__meta_kubernetes_namespace", "__meta_kubernetes_pod_container_name"]
                  separator     = "/"
                  target_label  = "job"
                }
              }

              // 3. Tail container logs through the kubelet API — no host mounts.
              loki.source.kubernetes "pods" {
                targets    = discovery.relabel.pod_logs.output
                forward_to = [loki.write.local.receiver]
                clustering { enabled = true }
              }

              // 4. Push to Loki.
              loki.write "local" {
                endpoint {
                  url = "http://loki.logging.svc:3100/loki/api/v1/push"
                }
              }
  destination:
    server: https://kubernetes.default.svc
    namespace: logging
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

</details>

```sh
git add argocd/apps/loki.yaml argocd/apps/alloy.yaml
git commit -m "Log pipeline: Loki single-binary + Alloy collector" && git push
argocd app sync root && argocd app wait loki alloy --health
kubectl get pods -n logging        # loki-0 + 3 alloy pods (one per node)
```

### 3. The datasource — via GitOps, not click-ops

Resist the Connections → Add datasource button. Add to `argocd/apps/monitoring.yaml` under `grafana:` in `valuesObject` (this rides the datasource-sidecar mechanism from Day 34):

```yaml
        grafana:
          additionalDataSources:
            - name: Loki
              type: loki
              uid: loki              # fixed uid — Day 37 references it for trace links
              access: proxy
              url: http://loki.logging.svc:3100
```

Commit/push/sync; the Grafana pod restarts with the new datasource.

### 4. LogQL drills (Explore → Loki)

Start error traffic first: `HOST=podlab-prod.localhost ERROR_RATE=0.3 ../day-31-promql/traffic.sh`. Question first, then peek.

**Q1.** All podlab-prod logs, then only lines containing `error`.

<details><summary>Answer</summary>

```logql
{namespace="podlab-prod"}
{namespace="podlab-prod"} |= "error"
```
Note every line is JSON — podlab's slog output. Raw grep works but is blunt: it also matches `/error` in the path field of successful requests.
</details>

**Q2.** Parse the JSON and show only real HTTP 5xx responses.

<details><summary>Answer</summary>

```logql
{namespace="podlab-prod", container="podlab"} | json | status >= 500
```
After `| json`, every field (`level`, `status`, `duration_ms`, `path`…) is a queryable label *for this query only* — parsed at read time, never stored or indexed.
</details>

**Q3.** Requests slower than 100ms.

<details><summary>Answer</summary>

```logql
{namespace="podlab-prod"} | json | duration_ms > 100
```
Probably sparse — hit `http://podlab-prod.localhost:8080/load?seconds=5` a few times and re-run.
</details>

**Q4.** Error-log rate as a chart (the log-derived metric).

<details><summary>Answer</summary>

```logql
sum(rate({namespace="podlab-prod"} | json | level="ERROR" [5m]))
```
Switch Explore to the graph view — a metric panel powered entirely by logs.
</details>

**Q5.** Catch podlab saying goodbye: trigger a rollout and find the SIGTERM drain message.

<details><summary>Answer</summary>

```sh
kubectl rollout restart deployment podlab -n podlab-dev
```

```logql
{namespace="podlab-dev"} |= "draining"
```
You'll find `{"level":"WARN","msg":"signal received, draining connections","signal":"terminated"}` — Day 10's graceful shutdown, now observable in retrospect instead of by being fast with `kubectl logs -f`. (ArgoCD selfHeal may additionally revert the restart annotation — harmless here, the pods cycle either way. The drift-free alternative: `kubectl delete pod -n podlab-dev -l app=podlab`.)
</details>

**Q6.** Count requests by status code from **logs** (last 5m), then cross-check against **metrics**.

<details><summary>Answer</summary>

```logql
sum by (status) (count_over_time({namespace="podlab-prod", container="podlab"} | json | __error__="" [5m]))
```

versus, in a Prometheus query:

```promql
sum by (code) (increase(podlab_http_requests_total{namespace="podlab-prod"}[5m]))
```

The numbers should agree within a few percent (scrape timing, `increase` extrapolation). **This is the lesson**: logs and metrics are two encodings of the same events; when they agree you trust both, and when they disagree you've found an instrumentation bug. Day 36 puts this correlation on one screen.
</details>

### 5. Label hygiene — the trap

You just saw `| json` mint labels like `status` and `duration_ms` at query time. Why not make `status` a real *stream* label in Alloy, and skip the parsing? Because every distinct label combination is a **separate stream** — its own index entries and its own chunks. `status` alone ×5, ×`path` ×6, ×`method`… your streams-per-pod explode and each chunk gets tiny and uncompressed — you've rebuilt Elasticsearch's cost with none of its speed. House rule: **stream labels = bounded infrastructure dimensions** (namespace, pod, container, app). Everything inside the line stays inside the line, parsed at read time.

## Verify ✅

- [ ] `argocd app get loki` and `alloy` → Synced/Healthy; `kubectl get pods -n logging` shows loki-0 + one alloy per node
- [ ] Grafana → Connections → Data sources lists Loki (provisioned — no UI edits possible)
- [ ] Q2 returns lines with parsed fields visible in the log detail view
- [ ] Q4's chart roughly matches `sum(rate(podlab_http_requests_total{namespace="podlab-prod",code="500"}[5m]))` from Prometheus
- [ ] Q6: log-counted totals ≈ metric-counted totals per status code
- [ ] The drain message from Q5 is in your query history

## Interview corner 💬

**"Why would you pick Loki over Elasticsearch — and when wouldn't you?"**
Loki indexes only stream labels and stores log content as compressed chunks, so storage and ingestion cost a fraction of a full-text index, and operations are far lighter. Kubernetes queries are label-shaped anyway — "this service, this window" — so the small index does the narrowing and a parallel grep does the rest. I wouldn't pick it where the dominant query is unscoped needle-in-haystack over long retention (security forensics, "find this token anywhere, 90 days") — that's what a full-text index is for, and pretending otherwise just moves the cost to query time.

**"What makes a good Loki label?"**
Bounded, infrastructure-level, known-before-parsing: namespace, app, container, environment. Anything high-cardinality or content-derived — status code, user ID, trace ID, path — must *not* be a stream label, because each combination creates a new stream with its own index entries and chunks; you get millions of tiny chunks and Loki's economics invert. Content belongs in the line, extracted at query time with `| json`.

**"How do you alert on something that only appears in logs?"**
Turn it into a metric: a LogQL metric query (`sum(rate({app="x"} | json | level="ERROR" [5m]))`) evaluated by Loki's ruler, which speaks the same alerting rules format and pushes to the same Alertmanager. Better long-term answer: if you alert on it routinely, instrument it as a real counter in the app — metrics are cheaper to evaluate and don't depend on log shipping being healthy.

## Stretch goals

- Add a "Recent errors" logs panel to the Day 34 dashboard (`{namespace=~"$namespace"} | json | status >= 500`) — a preview of Day 36.
- Use `| line_format "{{.method}} {{.path}} -> {{.status}} in {{.duration_ms}}ms"` to make a human-readable view of prod traffic.
- Query Loki's own metrics in Prometheus (`loki_ingester_streams_created_total`, `rate(loki_distributor_lines_received_total[5m])`) — the log system is itself a scrape target.

## Cleanup

Nothing. The `logging` namespace (Loki + Alloy) **stays through Day 50** — Days 36 and 37 build directly on it. Stop traffic loops when done.
