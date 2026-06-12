# Day 32 — Scrape Your Own App: ServiceMonitors

> **Time:** ~2.5 h · **Builds on:** Days 27 (ApplicationSet overlays), 30, 31

## Objectives

- Trace how the operator turns a ServiceMonitor into live scrape targets (selector → Service → Endpoints)
- Add a ServiceMonitor for podlab to the GitOps repo so dev/stage/prod get scraped with zero manual config
- Run the request-rate, p95-latency, and error-ratio queries against your own app's metrics
- Use `relabelings` vs `metricRelabelings` to shape targets and drop noisy series

## Concepts

### Why podlab isn't scraped yet

podlab has exposed `/metrics` since Day 1, and Prometheus has been running since Day 30 — yet **Status → Targets** shows no podlab. Prometheus doesn't scan the cluster for `/metrics` endpoints; it scrapes exactly what its config tells it to, and the operator only generates config from ServiceMonitors (and PodMonitors) that Prometheus's selectors match. Nothing selects podlab. Monitoring is opt-in by design: you declare *intent* with an object, the operator does the plumbing.

### ServiceMonitor → scrape config, step by step

```text
ServiceMonitor                       Prometheus CR (from the chart)
  spec.selector: app=podlab            serviceMonitorSelector: {}        <- matches ALL SMs
  spec.endpoints[0].port: http         (because ...NilUsesHelmValues=false, Day 30)
        |                                        |
        v                                        v
   the OPERATOR watches both, renders a scrape_config:
     kubernetes_sd_configs: role=endpoints, namespace=<SM's ns>
     relabel rules: keep only Endpoints of Services labeled app=podlab, port named "http"
        |
        v
   config-reloader sidecar hot-reloads Prometheus
        |
        v
   Prometheus asks the API server for Endpoints -> one TARGET PER READY POD IP
```

Two subtleties people miss:

1. **The selector matches the *Service's* labels, not the pods'.** The Service's Endpoints object then supplies pod IPs. Wrong-but-similar labels = silent nothing. (No Service at all? That's what **PodMonitor** is for — it selects pods directly. Use it for headless/no-Service workloads or when you must scrape every pod of a StatefulSet individually.)
2. **`port` refers to the Service port *name*.** An unnamed port can't be referenced — this is why "give every Service port a name" is a platform-team house rule.

### namespaceSelector — and why we don't need one

A ServiceMonitor by default only looks at Services **in its own namespace**. `namespaceSelector: {any: true}` widens it cluster-wide. You could write one ServiceMonitor in `monitoring` with `any: true` and label-match all podlab Services — but your podlab base renders **per overlay**: every env's ApplicationSet Application applies the base into its own namespace. Put the ServiceMonitor *in the base* and you automatically get one per namespace, each scoped to its own env, deleted when the env is deleted, visible in each env's ArgoCD app. Monitoring config travels with the workload — the cleaner pattern, and exactly what the operator's CRD design enables.

### Relabeling: two hooks, two purposes

| Field (in SM endpoint) | Runs | Use for |
|---|---|---|
| `relabelings` | **before** the scrape, on target metadata (`__meta_kubernetes_*`) | choosing/renaming targets, copying pod labels onto all their series |
| `metricRelabelings` | **after** the scrape, on every ingested sample | dropping noisy/high-cardinality series before they hit the TSDB |

Example you'll use constantly in real jobs — drop Go runtime internals you never look at:

```yaml
metricRelabelings:
  - sourceLabels: [__name__]
    regex: "go_(gc|memstats).*"
    action: drop
```

Dropped at ingestion = never stored = cardinality you don't pay for. This is the #1 lever when a Prometheus server starts eating memory.

## Lab

### 1. Confirm the gap

Prometheus UI → Status → Targets: no podlab. And:

```promql
up{namespace=~"podlab-.*"}    # empty
```

### 2. Name the Service port

In your **k8s-gitops** repo, open podlab's base Service (e.g. `podlab/base/service.yaml` — adjust to your Day 22/27 layout). The port must be **named** `http`:

```yaml
ports:
  - name: http          # <- the ServiceMonitor will reference this NAME
    port: 8080
    targetPort: 8080
```

Also note the Service's labels — you'll match them next:

```sh
kubectl get svc -n podlab-dev podlab --show-labels
```

### 3. Add the ServiceMonitor to the base

Create `podlab/base/servicemonitor.yaml` and register it in the base `kustomization.yaml`. Requirements:

- `selector.matchLabels` matching **your Service's labels** exactly
- One endpoint: port `http` (by name), path `/metrics`, interval `15s`
- No `namespaceSelector` — each overlay's copy scopes itself to its own namespace

<details><summary>Solution</summary>

```yaml
# podlab/base/servicemonitor.yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: podlab
spec:
  selector:
    matchLabels:
      app: podlab           # MUST equal the labels on the Service object itself
  endpoints:
    - port: http            # the Service port NAME from step 2
      path: /metrics
      interval: 15s
```

```yaml
# podlab/base/kustomization.yaml — add to resources:
resources:
  - deployment.yaml
  - service.yaml
  - servicemonitor.yaml
```

</details>

### 4. Close the loop through Git

```sh
cd ~/Code/k8s-gitops
git add podlab/base/
git commit -m "Scrape podlab: named port + ServiceMonitor in base"
git push
argocd app list | grep podlab        # wait for the three apps to sync (or argocd app sync ...)
kubectl get servicemonitors -A | grep podlab    # one per env: podlab-dev/stage/prod
```

Within ~30s of the sync, Prometheus **Status → Targets** grows three `serviceMonitor/podlab-*/podlab/0` sections, targets **UP**. Pause on what just happened: you committed one YAML file and three environments became monitored. No Prometheus config edited, no monitoring team pinged, no manual step. **This is the GitOps-to-monitoring loop** — it's the answer when an interviewer asks why config-as-CRDs matters.

### 5. Generate traffic and query your own app

```sh
HOST=podlab-prod.localhost ERROR_RATE=0.3 ../day-31-promql/traffic.sh   # spare terminal
HOST=podlab-dev.localhost ./traffic.sh                                  # clean traffic, another terminal
```

Give it 2–3 minutes of data, then — these three queries are **the** queries; they come back as alert exprs on Day 33, RED dashboard panels on Day 36, and canary analysis on Day 39. Type them, don't paste:

**Request rate by namespace and status code:**

```promql
sum by (namespace, code) (rate(podlab_http_requests_total[5m]))
```

**p95 latency per namespace:**

```promql
histogram_quantile(0.95,
  sum by (le, namespace) (rate(podlab_http_request_duration_seconds_bucket[5m])))
```

**Error ratio per namespace:**

```promql
sum by (namespace) (rate(podlab_http_requests_total{code=~"5.."}[5m]))
/
sum by (namespace) (rate(podlab_http_requests_total[5m]))
```

Expect roughly `0.1` for `podlab-prod` (the `/error?rate=0.3` calls are a third of its traffic at ~30% failure each) and nothing/0 for `podlab-dev`.

### 6. The "target isn't there" debugging ladder

Break it on purpose to learn the diagnosis path. Edit the live Service (yes, hand-edit — selfHeal will fix it): `kubectl edit svc podlab -n podlab-dev`, rename the port from `http` to `web`. Within a minute the dev target disappears. Now climb the ladder you'll use for the rest of your career:

```sh
# 1. Does the ServiceMonitor exist where you think?
kubectl get servicemonitor -n podlab-dev podlab -o yaml

# 2. Does its selector match the SERVICE's labels (not the pods')?
kubectl get svc -n podlab-dev podlab --show-labels

# 3. Does the named port exist on the Service?
kubectl get svc -n podlab-dev podlab -o jsonpath='{.spec.ports[*].name}'   # <- "web", the bug

# 4. Does the Service have ready Endpoints at all?
kubectl get endpointslices -n podlab-dev -l kubernetes.io/service-name=podlab

# 5. Does Prometheus even select this SM? (the Day 30 nil-uses-helm-values trap)
kubectl get prometheus -n monitoring -o jsonpath='{.items[0].spec.serviceMonitorSelector}'
```

Rungs 1–4 are Kubernetes-side; rung 5 is operator-side. The Prometheus UI's **Service Discovery** page shows rung-5 survivors: if the SM appears there but with zero kept targets, the bug is at rungs 2–4; if the SM isn't listed at all, rung 5. Wait for selfHeal to repair the port (or `argocd app sync` the dev app) and confirm the target returns.

### 7. The build_info join — slice traffic by version

`podlab_http_requests_total` has no `version` label (good — it would double cardinality on every release). `podlab_build_info{version,color}` carries it, one series per pod. Join them:

```promql
sum by (namespace, version) (
  rate(podlab_http_requests_total[5m])
  * on (namespace, pod) group_left (version)
  podlab_build_info
)
```

`on(namespace, pod)` matches each request-rate series to its pod's single build_info series; `group_left(version)` copies `version` across the many-to-one match. On Day 39 a canary means *two* versions serving simultaneously — this query is how you see which one is failing.

## Verify ✅

- [ ] Prometheus **Status → Targets** shows three podlab sections, every endpoint **UP**
- [ ] `up{namespace=~"podlab-.*"}` returns one series per podlab pod, all `1`
- [ ] The error-ratio query returns ≈ 0.1 for `podlab-prod` (with `ERROR_RATE=0.3` traffic running) and empty/0 for dev
- [ ] The build_info join returns series labeled `{namespace=..., version="v1"}`
- [ ] You broke the dev target via the port rename, diagnosed it with the ladder, and watched selfHeal restore it
- [ ] `git log --oneline -1` in k8s-gitops shows your ServiceMonitor commit — the only change you made anywhere

## Interview corner 💬

**"Walk me through how a ServiceMonitor becomes an actual scrape."**
The Prometheus operator watches ServiceMonitors that the Prometheus CR's `serviceMonitorSelector` matches. For each one it renders a `scrape_config` using Kubernetes endpoints service-discovery scoped to the SM's namespaceSelector, plus relabel rules that keep only Endpoints belonging to Services matching the SM's label selector and the named port. A config-reloader sidecar hot-reloads Prometheus, which then resolves the Endpoints to ready pod IPs — one target per pod. Common failure modes: the SM's selector matches pod labels instead of Service labels, the port name doesn't exist on the Service, or Prometheus's selector doesn't match the SM at all (the Helm chart's release-label default).

**"ServiceMonitor vs PodMonitor — when each?"**
ServiceMonitor selects Services and scrapes their Endpoints — the default, since most workloads have a Service and you inherit its abstraction. PodMonitor selects pods directly: use it when there's no Service (batch workers, DaemonSets exposing metrics only), or when the Service would hide pods you need individually.

**"Your app team says their metrics exploded Prometheus memory. What do you do?"**
Check TSDB status for top series counts, find the offending metric, then either fix the instrumentation (an unbounded label like user ID or request path) or stop ingesting it: `metricRelabelings` with `action: drop` on the ServiceMonitor discards series post-scrape, pre-storage. Relabeling at the SM level means the fix ships as a Git change to the team's own repo, not an emergency edit to central config.

## Stretch goals

- Add a `metricRelabelings` rule to the ServiceMonitor dropping `go_(gc|memstats).*`, push it, and confirm with `{__name__=~"go_memstats.*", namespace="podlab-dev"}` going stale.
- Scale prod to 3 replicas (in the overlay, via Git!) and watch the target count grow — then check the error-ratio query still works unchanged. That's the point of `sum by`.
- Inspect what the operator generated: `kubectl get secret -n monitoring prometheus-monitoring-kube-prometheus-prometheus -o jsonpath='{.data.prometheus\.yaml\.gz}' | base64 -d | gunzip | grep -A20 "podlab"`.

## Cleanup

Nothing. The ServiceMonitor lives in the podlab base permanently — Days 33, 36, and 39 depend on these metrics. Keep at least the prod traffic loop running if you're continuing to Day 33 today; otherwise just rerun `traffic.sh` tomorrow.
