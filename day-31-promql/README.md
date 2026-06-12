# Day 31 — PromQL Bootcamp

> **Time:** ~3 h · **Builds on:** Day 30

## Objectives

- Read any PromQL expression by typing it: instant vector, range vector, or scalar
- Use `rate()`, `increase()`, aggregations with `by`/`without`, `topk`, and `offset` correctly
- Compute percentiles from histograms with `histogram_quantile` and explain its accuracy limits
- Apply the "rate then sum, never sum then rate" rule and explain *why* (counter resets)

## Concepts

### Everything is typed

Every PromQL expression evaluates to one of:

| Type | Looks like | Example |
|---|---|---|
| **Instant vector** | a set of series, *one sample each* (the latest) | `up`, `node_memory_MemAvailable_bytes` |
| **Range vector** | a set of series, *a window of samples each* | `up[5m]` — can't be graphed directly |
| **Scalar** | a single number | `42`, `scalar(sum(up))` |

Selectors filter by labels: `=` exact, `!=` negate, `=~` regex, `!~` negated regex.

```promql
kube_pod_info{namespace="podlab-prod"}
kube_pod_info{namespace=~"podlab-.*"}
apiserver_request_total{verb!~"WATCH|CONNECT"}
```

Type errors are the #1 beginner wall: `rate(up)` fails because `rate()` *needs a range vector* — it computes per-second change *over a window*, so it must see multiple samples. `rate(up[5m])` works (and is meaningless — `up` isn't a counter; rate only makes sense on counters).

### rate vs irate vs increase

- `rate(c[5m])` — per-second average increase over the last 5m. Smooth, robust. **Default choice** for graphs and alerts.
- `irate(c[5m])` — slope between the *last two samples* only. Twitchy; only for zoomed-in debugging of spiky data. Never alert on it.
- `increase(c[5m])` — `rate * 300`: total increase over the window. For humans ("how many requests in the last hour"), not for comparing across windows.

All three handle **counter resets**: when a pod restarts, its counter drops to 0. `rate()` detects any decrease and treats it as a reset, adding the post-reset value as new growth. This is *why ordering matters*:

> **Rate then sum, never sum then rate.**
> `sum(rate(c[5m]))` ✅ — reset detection runs per-series, where resets actually happen, then you add clean per-second rates.
> `rate(sum(c)[5m])` ❌ — first, you literally can't (you'd need a subquery); second, summing first *hides* resets: if pod A's counter drops 10,000 while pod B keeps climbing, the summed series just dips a bit — `rate` interprets that dip as a giant reset and fabricates a spike. Aggregation destroys the per-series information reset-detection needs.

### Aggregation

```promql
sum by (namespace) (rate(container_cpu_usage_seconds_total[5m]))   # keep ONLY namespace
sum without (instance, pod) (rate(...))                            # drop these, keep the rest
topk(5, ...)   bottomk(3, ...)   count(...)   avg(...)   max(...)
```

`by` keeps listed labels; `without` drops listed labels. Prefer `by` in dashboards (explicit), `without` in recording rules (preserves dimensions you didn't think of).

`offset 1h` shifts the lookup window: `rate(c[5m] offset 1h)` is "the rate as of an hour ago" — great for now-vs-then comparisons.

### Histograms: _bucket / _sum / _count

A Prometheus histogram is just three counter families:

```text
podlab_http_request_duration_seconds_bucket{le="0.05"}  130   <- requests <= 50ms (CUMULATIVE)
podlab_http_request_duration_seconds_bucket{le="0.1"}   140   <- includes the 130 above
podlab_http_request_duration_seconds_bucket{le="+Inf"}  142   <- everything
podlab_http_request_duration_seconds_sum               3.4    <- total seconds
podlab_http_request_duration_seconds_count             142    <- == +Inf bucket
```

- Average latency = `rate(_sum[5m]) / rate(_count[5m])`.
- Percentiles: `histogram_quantile(0.99, sum by (le) (rate(..._bucket[5m])))`. It finds the bucket containing the 99th-percentile observation and **linearly interpolates within it**. Accuracy is bounded by bucket boundaries: if your buckets are `0.1` and `0.25` and the true p99 is 110ms, you'll get something *between* 100 and 250ms, assuming uniform spread. Coarse buckets → confident-looking wrong numbers. Always `sum by (le)` first — `le` is the one label `histogram_quantile` cannot live without.
- You **cannot average percentiles** across pods. You *can* sum bucket rates across pods and compute one true percentile — the killer feature of histograms over client-side summaries.

### Vector arithmetic (gently)

Binary ops match series by *identical label sets*. When the sides have different labels:

```promql
container_memory_working_set_bytes{container!=""}
  / on (namespace, pod, container)               # match using only these labels
kube_pod_container_resource_limits{resource="memory"}
```

`on(...)` (or `ignoring(...)`) controls the join keys. When one side has *many* series per match (many-to-one), add `group_left(extra_labels)` on the many side — Day 32 uses this to stamp `version` onto request rates via `podlab_build_info`. That's as deep as we go today.

## Lab

> podlab's own metrics aren't scraped yet (that's Day 32 — nothing selects them). Today you train on metrics that are **already flowing**: `apiserver_request_*`, `node_*`, `kube_*`, and cAdvisor's `container_*`. They're better teachers anyway — real volume, real cardinality.

### 1. Start background traffic

Keep a loop running so cAdvisor metrics for podlab move. Copy [`traffic.sh`](traffic.sh) somewhere handy:

```sh
kubectl get ingress -A                      # find your podlab prod host
chmod +x traffic.sh
HOST=podlab-prod.localhost ERROR_RATE=0.3 ./traffic.sh   # leave running in a spare terminal
```

### 2. Exercises

Open `http://prometheus.localhost:8080/graph`. For each exercise: write the query yourself first, run it, *then* open the answer. Use the **Table** view to inspect labels, **Graph** to see shape.

**Warm-up — selectors & instant vectors**

**Q1.** How many scrape targets are down right now?

<details><summary>Answer</summary>

```promql
count(up == 0)
```
Empty result = zero down (filters that match nothing return nothing, not 0). `sum(up == 0)` would also be empty; `count(up) - count(up == 1)` gives an actual 0.
</details>

**Q2.** How many pods exist per namespace?

<details><summary>Answer</summary>

```promql
count by (namespace) (kube_pod_info)
```
ABOUT metric from kube-state-metrics — one series per pod, so counting series counts pods.
</details>

**Q3.** Which pods are not in `Running` phase right now?

<details><summary>Answer</summary>

```promql
kube_pod_status_phase{phase!="Running"} == 1
```
`kube_pod_status_phase` emits a series per pod *per phase*, value 1 for the current one — the `== 1` filter is essential or you'll list every pod in every phase.
</details>

**Q4.** Show all metrics about the podlab deployment's replica counts.

<details><summary>Answer</summary>

```promql
{__name__=~"kube_deployment_status_replicas.*", deployment="podlab"}
```
`__name__` is a label too — regex-matching it explores metric families.
</details>

**Rates & aggregation**

**Q5.** API server request rate, broken down by verb.

<details><summary>Answer</summary>

```promql
sum by (verb) (rate(apiserver_request_total[5m]))
```
Rate per series first, then sum — LIST/WATCH/GET dominate; that's controllers doing their job.
</details>

**Q6.** What fraction of API server requests are failing (5xx)?

<details><summary>Answer</summary>

```promql
sum(rate(apiserver_request_total{code=~"5.."}[5m]))
/
sum(rate(apiserver_request_total[5m]))
```
The error-ratio pattern. Memorize the shape — Day 32 aims it at podlab, Day 33 alerts on it, Day 39 auto-rolls-back on it.
</details>

**Q7.** CPU usage of each *node*, as a percentage. Hint: `node_cpu_seconds_total` is a counter of seconds spent per core per `mode`; a core spends 1 second per second *somewhere*.

<details><summary>Answer</summary>

```promql
(1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m]))) * 100
```
The idle trick: `rate` of idle-seconds per core is the fraction of time idle (0–1); average over cores, subtract from 1. Cleaner than summing the eleven non-idle modes.
</details>

**Q8.** Top 5 pods by memory (working set), cluster-wide.

<details><summary>Answer</summary>

```promql
topk(5, sum by (namespace, pod) (container_memory_working_set_bytes{container!=""}))
```
`container!=""` drops cAdvisor's pod-level aggregate series (empty container label) that would double-count.
</details>

**Q9.** Per-namespace container CPU usage in cores.

<details><summary>Answer</summary>

```promql
sum by (namespace) (rate(container_cpu_usage_seconds_total{container!=""}[5m]))
```
"Seconds of CPU per second" = cores. Compare `monitoring` against `podlab-prod` — observability isn't free.
</details>

**Q10.** How many requests has the API server handled in the last 10 minutes (a number, not a rate)?

<details><summary>Answer</summary>

```promql
sum(increase(apiserver_request_total[10m]))
```
`increase` for human-readable totals; it extrapolates over scrape gaps so expect a non-integer.
</details>

**Histograms, offset, joins**

**Q11.** p99 API server request latency by verb (exclude WATCH/CONNECT — they're long-lived by design).

<details><summary>Answer</summary>

```promql
histogram_quantile(0.99,
  sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])))
```
`le` must survive the aggregation; every other label you keep becomes a separate percentile line.
</details>

**Q12.** Which deployments have unavailable replicas right now?

<details><summary>Answer</summary>

```promql
kube_deployment_status_replicas_unavailable > 0
```
Empty = healthy. Break something (`kubectl scale deploy podlab -n podlab-dev --replicas=5` if dev nodes can't fit them… or just trust it) and re-run.
</details>

**Q13.** Container restarts in the last hour, only where > 0.

<details><summary>Answer</summary>

```promql
increase(kube_pod_container_status_restarts_total[1h]) > 0
```
</details>

**Q14.** Is the API server busier now than 1 hour ago? Return the difference in req/s.

<details><summary>Answer</summary>

```promql
sum(rate(apiserver_request_total[5m]))
- sum(rate(apiserver_request_total[5m] offset 1h))
```
</details>

**Q15.** Memory usage of every container as a fraction of its memory *limit*.

<details><summary>Answer</summary>

```promql
container_memory_working_set_bytes{container!=""}
  / on (namespace, pod, container)
kube_pod_container_resource_limits{resource="memory"}
```
A FROM metric (cAdvisor) joined to an ABOUT metric (kube-state-metrics) via `on(...)`. Containers without limits vanish from the result — joins are implicit filters too.
</details>

### 3. Recording rules — a preview

Q11's query is expensive and you'll want it on dashboards, in alerts, and in canary analysis. A **recording rule** evaluates it on a schedule and stores the result as a new cheap series, e.g. `namespace:apiserver_request_p99:histogram_quantile`. You'll write rules inside a `PrometheusRule` on Day 33 — same CRD as alerts. For now just know: if you paste the same heavy query in a third place, that's the signal to record it.

## Verify ✅

- [ ] Q6 (apiserver error ratio) returns a value (likely ~0)
- [ ] Q7 (node CPU %) returns one series per node — three for your kind cluster
- [ ] Q15 (memory vs limit) returns a non-empty vector with `namespace`, `pod`, `container` labels
- [ ] You can say the rate-then-sum rule out loud with the reset explanation, without notes
- [ ] `traffic.sh` is still running (leave it — Day 32 needs warm counters)

## Interview corner 💬

**"Explain `rate()` to a junior engineer."**
Counters only count up — total requests since the process started. The raw number is useless; the *slope* is the signal. `rate(x[5m])` looks at the last 5 minutes of samples and returns the average per-second increase — turning "14,203,001 requests ever" into "37 requests/second right now". It also auto-corrects for restarts: a counter dropping to zero is recognized as a reset, not negative traffic. One rule to remember: always `sum(rate(...))`, never `rate(sum(...))` — reset detection only works on individual series.

**"How do you get a p99 from Prometheus histograms, and what are the caveats?"**
`histogram_quantile(0.99, sum by (le) (rate(metric_bucket[5m])))` — rate the buckets, aggregate keeping `le`, then compute the quantile. Caveats: (1) the answer is a *linear interpolation within one bucket*, so precision depends entirely on bucket boundaries near your p99 — defaults top out around 10s and can be wildly coarse; (2) never average percentiles across instances — aggregate bucket rates instead, which histograms uniquely allow; (3) if p99 sits in the last bucket, you just get the lower bound of `+Inf`'s neighbor — a flat line that *looks* fine while latency burns.

**"rate vs increase vs irate — when do you use each?"**
`rate` for graphs and alerts (smooth per-second average over the window). `increase` is `rate` times the window — same data, human units, for "requests this hour" stat panels. `irate` uses only the last two samples — high-resolution debugging zooms only; alerting on it is a paging-at-3am generator because a single spiky sample fires it.

## Stretch goals

- Rewrite Q7 using `sum by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[5m])) / count by (instance) (...)` and confirm both forms agree.
- Explore `node_cpu_seconds_total` modes: graph `sum by (mode) (rate(node_cpu_seconds_total[5m]))` — where does iowait show up when you run `HOST=... ./traffic.sh`?
- Read about *subqueries* (`max_over_time(rate(x[5m])[1h:])` — "the worst 5m rate in the last hour") and find today's peak apiserver rate.

## Cleanup

Nothing to delete. Keep `traffic.sh` — Days 32, 33, and 36 reuse it. The monitoring stack stays through Day 50.
