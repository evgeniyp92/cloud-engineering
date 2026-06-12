# Day 36 — Correlation: the RED Dashboard & the Debugging Drill

> **Time:** ~3 h · **Builds on:** Days 32, 33, 34, 35

## Objectives

- Build a single "debug podlab" screen: RED panels, per-version breakdown, and live logs sharing one variable and time range
- Walk a complete incident — alert → dashboard → logs → root cause — and write the timeline
- Explain RED vs USE and why correlation depends on consistent labels across signals
- Describe exemplars and where today's setup hits its ceiling (the bridge to tracing)

## Concepts

### The debugging story

Every real incident follows the same arc, and your tooling either supports it or fights it:

```text
ALERT          "podlab error rate > 5%"          metric — Day 33
  -> DASHBOARD  which namespace? which version?   metrics, sliced — Days 32/34
    -> LOGS      which request? what error?        logs — Day 35
      -> FIX      rollback / patch / scale          Git, of course
```

The failure mode of most orgs isn't missing data — it's that each arrow costs five minutes of tool-switching, label-retyping, and time-range-resetting. Today's goal: make every arrow a glance or one click. Mean-time-to-recovery is mostly mean-time-to-*understanding*.

### RED, formalized — and USE for the other half

**RED** is the request-side method: for every *service*, show

- **R**ate — requests/sec (`sum(rate(requests_total[..]))`)
- **E**rrors — failing fraction (`5xx / all`)
- **D**uration — latency distribution (p50/p95/p99 from histograms)

Three numbers answer "is this service healthy *for its users*?" — symptoms, exactly what Day 33 said to alert on. RED works for anything request-shaped: HTTP services, gRPC, queues (consume rate / failed messages / processing time).

**USE** is the resource-side method: for every *resource* (node, disk, CPU), **U**tilization, **S**aturation, **E**rrors. The Day 30 built-in dashboards are USE-flavored. The pairing: RED tells you users are hurting; USE tells you whether the cause is resource starvation. App dashboards lead with RED and link to USE — not the reverse.

### Correlation needs shared labels

Click from a metrics panel to logs and you implicitly assert: `namespace="podlab-prod"` *means the same thing* in Prometheus and Loki. That's only true because both pipelines derive labels from the same Kubernetes metadata — the operator's ServiceMonitor machinery (Day 32) and your Alloy relabel rules (Day 35) both emit `namespace`, `pod`, `container`. Check it:

```promql
podlab_http_requests_total{namespace="podlab-prod"}   # Prometheus: namespace, pod, container...
```

```logql
{namespace="podlab-prod", container="podlab"}          # Loki: same names, same values
```

This alignment is a *convention you maintain*, not magic — rename a label on one side (a careless relabel rule) and every cross-signal link silently breaks. It's also why platform teams standardize labels before they standardize anything else.

Grafana features that exploit the alignment:

- **Split Explore** — metrics left, logs right, shared (synced) time range. The zero-setup correlation tool.
- **Dashboard variables** — one `$namespace` feeding both Prometheus *and* Loki queries on the same dashboard. Variables are datasource-agnostic strings.
- **Panel data links** — a click on a panel opens Explore pre-filtered to the same scope and time range.
- **Derived fields** — regex a value (like a trace ID) out of log lines into a clickable link. Day 37 wires this to Tempo.

## Lab

### 1. Build `red-podlab` — one screen to debug podlab

The target layout — one screen, top-down in the order you'd debug:

```text
┌─ $namespace ▼ ──────────────────────────────────────────────────┐
│ ROW 1: RED          [ Rate/ns ]  [ ERROR % ●stat ]  [ p95/p99 ] │  is it broken?
│ ROW 2: WHICH        [ error % by VERSION ]  [ pods-by-version ] │  what changed?
│ ROW 3: WHY          [ live error LOGS  | json | status>=500   ] │  what exactly?
└─────────────────────────────────────────────── shared time range┘
```

New dashboard (click-first, commit-after — the Day 34 workflow). Variable `namespace` exactly as Day 34 (`label_values(podlab_build_info, namespace)`, multi + All). Three rows:

**Row 1 — RED** (the "is it broken?" row)

| Panel | Type | Query |
|---|---|---|
| Rate | time series | `sum by (namespace) (rate(podlab_http_requests_total{namespace=~"$namespace"}[$__rate_interval]))` |
| Errors | stat | `sum(rate(podlab_http_requests_total{namespace=~"$namespace",code=~"5.."}[$__rate_interval])) / sum(rate(podlab_http_requests_total{namespace=~"$namespace"}[$__rate_interval]))` — thresholds 1% / 5% as Day 34 |
| Duration | time series | `histogram_quantile(0.95, sum by (le) (rate(podlab_http_request_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))` + a p99 query |

**Row 2 — "which version?"** (the canary-killer row)

Error ratio **split by version** via the Day 32 `build_info` join — when a bad release ships, this panel points at it before anyone reads a log:

```promql
sum by (namespace, version) (
  rate(podlab_http_requests_total{namespace=~"$namespace", code=~"5.."}[$__rate_interval])
  * on (namespace, pod) group_left (version) podlab_build_info)
/
sum by (namespace, version) (
  rate(podlab_http_requests_total{namespace=~"$namespace"}[$__rate_interval])
  * on (namespace, pod) group_left (version) podlab_build_info)
```

Time series, legend `{{namespace}} {{version}}`. Add a latency **heatmap** beside it if you want the distribution view: query `sum by (le) (increase(podlab_http_request_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval]))`, format **Heatmap** — buckets over time, outliers visible as a smear instead of averaged away. Keep Day 34's pods-by-version table here too (copy the panel).

**Row 3 — the logs** (same variable, same time range — that's the whole trick)

**Logs panel**, datasource Loki:

```logql
{namespace=~"$namespace", container="podlab"} | json | status >= 500
```

Panel options: enable *Show time*. Scrolling errors, live, scoped by the very same `$namespace` dropdown driving the metrics above.

**Data link** (the one-click arrow): edit the Errors stat → Panel options → **Data links** → New:

- Title: `Open error logs in Explore`
- URL (one line; `${namespace}` and the time-range macro carry your context):

```text
/explore?left={"datasource":"loki","queries":[{"expr":"{namespace=~\"${namespace}\", container=\"podlab\"} | json | status >= 500"}],"range":{"from":"${__from}","to":"${__to}"}}
```

Fiddly to type, magical to click: panel → Explore, pre-filtered, same window. (If the JSON-in-URL fights you, the no-setup fallback is split Explore — open Explore, run the metric query, **Split**, switch right pane to Loki, sync time ranges.)

Save as **red-podlab**.

### 2. Commit it (Day 34 muscle memory)

Export (externally-shared mode) → `k8s-gitops/monitoring/dashboards/red-podlab.json` → second entry in the configMapGenerator:

```yaml
  - name: dashboard-red-podlab
    files: [dashboards/red-podlab.json]
    options:
      disableNameSuffixHash: true
      labels: {grafana_dashboard: "1"}
```

Push, sync, delete the hand-built copy, use the provisioned one from here on.

### 3. THE DRILL — walk the story end to end

Set the stage with clean traffic to two environments (two terminals):

```sh
HOST=podlab-dev.localhost  ../day-31-promql/traffic.sh
HOST=podlab-prod.localhost ../day-31-promql/traffic.sh
```

Dashboard green everywhere. Note the time. Now, third terminal — **break prod only**:

```sh
HOST=podlab-prod.localhost ERROR_RATE=0.4 ../day-31-promql/traffic.sh
```

Walk it — *in character*, as if paged. **Write each timestamp down as you go**:

1. **The alert.** Prometheus → Alerts: `PodlabHighErrorRate` pending → firing (~3–7 min: rate window + `for`). Alertmanager notifies hook-sink (Day 33): `kubectl logs -n hooks deploy/hook-sink --tail=3` — there's your "page".
2. **The dashboard.** Open red-podlab, `$namespace` = All. Row 1: error stat red. Rate normal — not a traffic spike. Duration normal — not an overload. *Errors only.*
3. **Scope it.** Per-namespace rate panel and row 2: prod red, dev clean. Version panel: all errors under `v1` — one version, one namespace. (Mid-canary on Day 39, this exact panel shows v2 red while v1 stays green — that's the auto-rollback signal.)
4. **The logs.** Row 3 (or click your data link): `{"level":"ERROR","msg":"simulated failure","rate":0.4}` lines, plus the request lines with `path=/error, status=500`. "Root cause": something is hammering `/error` in prod. In real life this is the malformed-input stack trace or the dead upstream — the *which*, which no metric can hold.
5. **The fix.** Ctrl-C the error loop ("rollback shipped"). Watch the stat drain back through yellow to green; the alert resolves; hook-sink logs the resolution.

**Write the timeline** (keep it — this is your Day 49 demo script and your interview story):

```text
14:02 error traffic starts (the "bad deploy")
14:06 PodlabHighErrorRate fires; webhook delivered
14:08 dashboard: errors 13% in prod only; dev clean; all errors on v1
14:09 logs: "simulated failure" on /error — root cause identified
14:11 "rollback" (traffic stopped)
14:17 alert resolved; error ratio < 1%
MTTR: ~11 min, of which UNDERSTANDING took 3.
```

### 4. Correlation drills — prove the pillars agree

While the error traffic is still running (or rerun it), two question-first exercises:

**Q1.** Using *only Loki*, find the exact minute the incident started. Then confirm it from *only Prometheus*. Do the two timestamps agree?

<details><summary>Answer</summary>

Loki — graph the error-log rate and read where it leaves zero:

```logql
sum(count_over_time({namespace="podlab-prod"} | json | level="ERROR" [1m]))
```

Prometheus — same shape from the counter:

```promql
sum(rate(podlab_http_requests_total{namespace="podlab-prod", code="500"}[1m])) > 0
```

They should agree to within a scrape interval (~15s). The metrics edge is slightly *smoother* (rate over a window) — which is exactly why the log-derived view is better for pinpointing onset, and the metric view better for alerting. Two tools, same truth, different resolutions.
</details>

**Q2.** During the incident, what fraction of *all* prod errors came from `/error`, computed from logs — and does the metrics side agree?

<details><summary>Answer</summary>

```logql
sum by (path) (count_over_time({namespace="podlab-prod"} | json | status >= 500 [10m]))
```

vs

```promql
sum by (path) (increase(podlab_http_requests_total{namespace="podlab-prod", code=~"5.."}[10m]))
```

Both should attribute ~100% to `/error`. If they ever disagree in real life, you've found either log loss (Alloy backpressure) or an uninstrumented code path — both are bugs worth a ticket.
</details>

### 5. Exemplars — where this setup tops out

Your drill answered *what* (errors), *where* (prod, v1), *which* (simulated failure on `/error`). It cannot answer: *"for this one slow request, where did its 800ms go?"* — metrics aggregate, logs are per-service islands.

**Exemplars** are the teaser: individual trace IDs attached to histogram bucket samples at scrape time, rendered by Grafana as dots on your latency panels — click a dot, jump to that exact request's trace. The plumbing exists in Prometheus (`--enable-feature=exemplar-storage`) and Grafana, but the payoff requires the app to *have* trace IDs to attach. That's tomorrow: OpenTelemetry, Tempo, and instrumenting podlab itself.

## Verify ✅

- [ ] red-podlab is provisioned from Git (read-only in the UI) with three rows of live data
- [ ] One screen answers rate, errors, duration, *and* shows the matching error logs without leaving the page
- [ ] `$namespace` switches every panel — including the Loki logs panel — together
- [ ] The drill reproduced end-to-end: alert fired → dashboard isolated prod/v1 → logs showed "simulated failure" → stop → resolve
- [ ] Your written timeline exists (commit it to the repo — `monitoring/drill-timeline.md` is a fine home)
- [ ] Data link (or split Explore) jumps from the error panel to pre-filtered Loki logs

## Interview corner 💬

**"Explain the RED method. Why those three?"**
Rate, Errors, Duration per service — they're the complete user-experience surface of a request-driven system: how much work, how often it fails, how slow it is. Everything else (CPU, restarts, queue depth) is a *cause* that only matters when one of these three symptoms moves. We alert on RED, dashboard RED first, and keep USE (utilization/saturation/errors per resource) one link away for the "why". It also standardizes: every service's top row looks identical, so on-call can read any team's dashboard.

**"How do you actually correlate metrics and logs in practice?"**
Shared labels, end to end. Both our Prometheus scrape pipeline and the Alloy log pipeline derive `namespace`/`pod`/`container` from the same Kubernetes metadata, so one dashboard variable drives PromQL and LogQL panels on the same screen, and a panel data link opens Explore pre-filtered to the same scope and window. The non-obvious part is that this is a maintained convention — one sloppy relabel rule and correlation silently dies — which is why label standards are a platform-team deliverable.

**"Walk me through a recent incident debugging flow."**
(Tell the drill — it's real, you ran it:) Error-ratio alert fired for one namespace; the RED dashboard showed rate and latency flat with errors at 13%, isolating it to prod and, via a build-info join, to a single version; the embedded logs panel showed the failing endpoint and error message within a minute; rollback, watched the ratio drain, alert auto-resolved. Total time-to-understanding ~3 minutes because no tool-switching: one screen, shared labels, linked logs.

## Stretch goals

- Add a USE row: container CPU (`rate(container_cpu_usage_seconds_total{namespace=~"$namespace"}[...])`) and working-set memory vs limits — now the "is it resources?" question is on-screen too.
- Enable exemplar storage ahead of Day 37: `prometheusSpec.enableFeatures: [exemplar-storage]` in monitoring.yaml.
- Re-run the drill but break *stage* instead — confirm you can scope it without reading any query, purely from the dashboard.

## Cleanup

Keep everything (dashboard, rules, hooks, both traffic-script habits). Stop the three traffic loops. The full monitoring stack stays through Day 50 — Day 39's canary analysis queries the exact metrics behind row 2.
