# Day 34 — Grafana: Dashboards as Code

> **Time:** ~3 h · **Builds on:** Days 30, 32

## Objectives

- Build a multi-panel podlab dashboard by hand, choosing the right visualization per question
- Template it with a `$namespace` variable so one dashboard serves dev/stage/prod
- Provision it from Git via the ConfigMap sidecar pattern — and prove the UI can no longer lose it
- Weigh import-by-ID community dashboards against GitOps-provisioned ones

## Concepts

### Click to explore, commit to keep

Grafana's editor is excellent, and you *should* click around — it's how you learn what a panel can do and how you debug at 3am. But a dashboard that lives only in Grafana's database is a pet: unversioned, unreviewable, gone when the PVC dies, different in every cluster. The workflow that works: **prototype by clicking, then export the JSON and commit it**. From then on Git is the source of truth and the UI copy is read-only. Today you do exactly that round-trip.

### Grafana anatomy

| Piece | What it is |
|---|---|
| **Datasource** | a connection (Prometheus, Loki, Tempo…). Provisioned by the stack chart — yours is already there |
| **Panel** | one visualization + one or more queries against a datasource |
| **Query** | PromQL (today), LogQL (Day 35), TraceQL (Day 37); with a legend format like `{{namespace}}` |
| **Transformations** | post-query reshaping (joins, renames, filters) — used for the version table today |
| **Variables** | dropdowns (`$namespace`) interpolated into every query |
| **Rows** | collapsible panel groups — Day 36's RED dashboard uses them |

### Choosing a visualization

The visualization is an *answer format*. Match it to the question:

| Question shape | Use | Today's example |
|---|---|---|
| "how does it change over time?" | **time series** | request rate, latency |
| "what is it *right now*, am I ok?" | **stat** (+ thresholds) | error ratio |
| "how close to a limit?" | **gauge** | — (saturation panels) |
| "what's the breakdown / inventory?" | **table** | pods by version |
| "how is a distribution shaped over time?" | **heatmap** | latency buckets (Day 36) |

The classic mistake is time-series-for-everything: an error-ratio *stat* that's green/yellow/red answers "am I ok?" in 200ms from across the room; the same data as a squiggle makes you think.

### Template variables

A variable defined as `label_values(podlab_build_info, namespace)` queries Prometheus for every value of the `namespace` label on that metric and renders a dropdown. Every panel query then says `{namespace="$namespace"}` (or `=~"$namespace"` for multi-select). One dashboard, N environments — instead of three drifting copies. This is also why consistent labels across environments (which your kustomize overlays give you for free) are a monitoring feature, not a cosmetic one.

### The sidecar pattern — GitOps' provisioning hook

kube-prometheus-stack's Grafana pod runs a **sidecar** container that watches the API server for ConfigMaps labeled `grafana_dashboard: "1"` (in the release namespace by default). Found one? It drops the JSON into Grafana's provisioning directory and Grafana loads it — marked *provisioned*, meaning the UI cannot save over it or delete it. So the full loop is:

```text
dashboard JSON in Git ──> ArgoCD applies ConfigMap ──> sidecar sees label ──> Grafana loads it
        ^                                                                        |
        └――――――――――― the ONLY way to change it is a commit ←―――――――――――――――――――――┘
```

Datasources have a twin sidecar (`grafana_datasource` label) — Day 35 uses the chart's `additionalDataSources` value, which rides the same mechanism.

## Lab

### Part 1 — build "podlab overview" by clicking

Start traffic so panels have something to show: `HOST=podlab-prod.localhost ERROR_RATE=0.3 ../day-31-promql/traffic.sh` plus a clean `HOST=podlab-dev.localhost` loop.

Grafana (`http://grafana.localhost:8080`) → **Dashboards → New → New dashboard**.

**1. Variable first** (gear icon → Variables → New):
- Name `namespace`, type **Query**, datasource Prometheus
- Query: `label_values(podlab_build_info, namespace)`
- Enable *Multi-value* and *Include All option*. Save.

**2. Panel: Request rate** — time series

```promql
sum by (namespace, code) (rate(podlab_http_requests_total{namespace=~"$namespace"}[$__rate_interval]))
```

Legend format: `{{namespace}} {{code}}`. Note `$__rate_interval`: Grafana picks a window matched to the panel's resolution and scrape interval — hardcoded `[5m]` over a 30-day range wastes resolution; over a 5-minute range it over-smooths.

**3. Panel: Error ratio** — **stat**

```promql
sum(rate(podlab_http_requests_total{namespace=~"$namespace", code=~"5.."}[$__rate_interval]))
/
sum(rate(podlab_http_requests_total{namespace=~"$namespace"}[$__rate_interval]))
```

Unit: *Percent (0.0-1.0)*. Thresholds: green base, yellow at `0.01`, red at `0.05` — matching the Day 33 alert threshold, so dashboard yellow/red and alert pending/firing tell one story.

**4. Panel: Latency p50/p95/p99** — time series, three queries (A/B/C), changing only the quantile:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(podlab_http_request_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))
```

Legend: `p95` etc. Unit: seconds.

**5. Panel: Container restarts** — time series

```promql
sum by (namespace, pod) (increase(kube_pod_container_status_restarts_total{namespace=~"$namespace"}[1h]))
```

An ABOUT metric on an app dashboard — restarts are the cause you check when the symptom panels go red.

**6. Panel: Pods by version** — **table**

```promql
podlab_build_info{namespace=~"$namespace"}
```

Set the query to *Instant* (Options → Instant), then **Transformations → Organize fields**: hide `Time`/`Value`/`instance`/`job`, keep `namespace`, `pod`, `version`, `color`. This table is your release-state X-ray — on Day 39 it shows two versions mid-canary.

**7. Test the variable**: flip `$namespace` between `podlab-prod` and `podlab-dev` — error stat red vs green. Save as **"podlab overview"**.

### Part 2 — capture it to Git

**1. Export**: Dashboard → Share (or Export in newer UIs) → **Export** tab → enable **"Export for sharing externally"** (replaces your datasource UID with a portable `${DS_PROMETHEUS}` input) → Save to file.

**2. Commit** it into the Day 33 `monitoring-config` app:

```text
k8s-gitops/monitoring/
├── kustomization.yaml
├── rules/podlab-alerts.yaml
└── dashboards/podlab-overview.json      # <- the export
```

Extend `monitoring/kustomization.yaml` with a configMapGenerator. Requirements: ConfigMap named `dashboard-podlab-overview`, embedding the JSON file, labeled `grafana_dashboard: "1"`, stable name (no hash suffix).

<details><summary>Solution</summary>

```yaml
# monitoring/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: monitoring
resources:
  - rules/podlab-alerts.yaml
configMapGenerator:
  - name: dashboard-podlab-overview
    files:
      - dashboards/podlab-overview.json
    options:
      disableNameSuffixHash: true      # stable name; the sidecar watches content changes anyway
      labels:
        grafana_dashboard: "1"         # the ONLY thing the sidecar looks for
```

</details>

```sh
cd ~/Code/k8s-gitops
git add monitoring/
git commit -m "Provision podlab overview dashboard from Git"
git push && argocd app sync monitoring-config
```

**3. Prove the loop** — the satisfying part:

1. Wait ~30s, refresh Grafana → Dashboards: "podlab-overview" now appears (likely in a *General*/provisioned folder) **in addition to** your hand-built copy.
2. **Delete your hand-built original** in the UI. Gone.
3. Open the provisioned one: data flows, variable works, but **Save is disabled / it's read-only** — edits now require a commit. Try deleting it from the UI: Grafana refuses or the sidecar resurrects it on the next sync. The dashboard is no longer a pet.

Folder mention: set `grafana.sidecar.dashboards.folderAnnotation: grafana_folder` in the stack values and an annotation `grafana_folder: podlab` in the generator's `options.annotations` to control which folder it lands in — nice once you have ten dashboards.

**4. Read what you committed** — dashboards-as-code means dashboard *PRs*, and you'll review them. The JSON has a learnable skeleton:

```text
{
  "title": "podlab overview",
  "uid": "...",                    <- STABLE identity; links/bookmarks break if it changes
  "panels": [
    {
      "title": "Error ratio",
      "type": "stat",
      "targets": [ {"expr": "sum(rate(...))", "legendFormat": "..."} ],   <- the queries
      "fieldConfig": { "defaults": { "thresholds": {...}, "unit": "percentunit" } },
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0}    <- layout noise; ignore in review
    }
  ],
  "templating": { "list": [ {"name": "namespace", "query": "label_values(...)"} ] },
  "__inputs": [ {"name": "DS_PROMETHEUS", ...} ]      <- the portable datasource handle
}
```

When reviewing a dashboard diff, read `targets[].expr`, `templating`, thresholds, and the `uid` — and let `gridPos`/`id` churn wash over you. Try it now:

```sh
cd ~/Code/k8s-gitops
python3 -c "import json;d=json.load(open('monitoring/dashboards/podlab-overview.json'));print('\n'.join(t.get('expr','') for p in d['panels'] for t in p.get('targets',[])))"
```

Every query on the dashboard, greppable in CI. Teams lint exactly this — "no hardcoded namespace", "rate windows use \$__rate_interval".

### Part 3 — community dashboards

Dashboards → New → **Import** → ID `1860` ("Node Exporter Full") → select your Prometheus datasource. Instant, comprehensive node monitoring.

Tradeoff to internalize: import-by-ID is fast but lives only in this Grafana (click-ops again), and community dashboards often assume label schemes you don't have. For keeps: download the JSON, commit it next to yours, provision it. For exploring: import away.

## Verify ✅

- [ ] `kubectl get configmap -n monitoring dashboard-podlab-overview --show-labels` → has `grafana_dashboard=1`
- [ ] The provisioned "podlab overview" shows live data on all five panels
- [ ] Switching `$namespace` dev ↔ prod flips the error-ratio stat green ↔ red (with `ERROR_RATE=0.3` traffic on prod)
- [ ] Editing the provisioned dashboard offers no Save (or "Cannot save provisioned dashboard")
- [ ] Your hand-built original is deleted; only the Git-provisioned copy remains
- [ ] Dashboard 1860 shows node stats for all three kind nodes
- [ ] The python one-liner lists every PromQL expr from your committed JSON — you can "read" the dashboard without opening Grafana

## Interview corner 💬

**"How do you manage Grafana dashboards across environments/clusters?"**
Dashboards as code: JSON in Git, provisioned via the sidecar pattern — ConfigMaps labeled `grafana_dashboard` that a sidecar container loads into Grafana, applied by ArgoCD like any other manifest. Provisioned dashboards are read-only in the UI, so there's no drift; new clusters get every dashboard on bootstrap; changes are PRs with review and rollback. The UI stays the prototyping tool — build there, export, commit.

**"One dashboard for dev/stage/prod, or one each?"**
One, templated. A `$namespace` (or `$cluster`) query variable from `label_values()` parameterizes every panel, which works because our overlays keep label schemes identical across environments. Copies drift within weeks — the prod copy gets the fixes, dev lies to you. The exception is genuinely different *questions* (capacity planning vs service health), which deserve different dashboards, not different copies.

**"When is a stat panel better than a graph?"**
When the question is "am I OK right now?" rather than "what's the trend?". A thresholded stat is pre-decided: green means walk away. We align stat thresholds with alert thresholds so the dashboard and the pager never disagree — a red stat that doesn't page, or a page with a green dashboard, both destroy trust in the tooling.

## Stretch goals

- Add the `grafana_folder` annotation + `folderAnnotation` value so podlab dashboards get their own folder.
- Add a "traffic by version" panel using Day 32's `group_left` join — you'll want it on Day 39.
- Find the JSON for dashboard 1860, commit and provision it, and delete the imported copy — full GitOps for third-party dashboards.

## Cleanup

Keep everything — the provisioned dashboard is Day 36's foundation and part of the Day 49 demo. You may delete the imported (non-provisioned) 1860 dashboard if you did the stretch goal. Stop traffic loops.
