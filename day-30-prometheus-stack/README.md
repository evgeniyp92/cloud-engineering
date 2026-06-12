# Day 30 — Prometheus & kube-prometheus-stack

> **Time:** ~3 h · **Builds on:** Days 24, 26 (ArgoCD app-of-apps), 8 (metrics-server)

## Objectives

- Explain Prometheus's pull model, the exposition format, and what a TSDB stores
- Distinguish metrics **from** apps (instrumentation, node-exporter) from metrics **about** Kubernetes objects (kube-state-metrics)
- Install kube-prometheus-stack as an ArgoCD Application with laptop-friendly values
- Navigate the Prometheus UI (targets, service discovery, TSDB status) and log into Grafana

## Concepts

### Why pull, not push?

Most monitoring systems you've met (StatsD, CloudWatch agents) *push*: the app fires data at a collector. Prometheus inverts this — it **scrapes** an HTTP endpoint (`/metrics`) on every target on a schedule. Three reasons this wins in Kubernetes:

1. **Target discovery from the source of truth.** Prometheus asks the Kubernetes API "which pods/endpoints exist?" and scrapes them. Nothing to configure in the app; a new replica is monitored seconds after it's Ready.
2. **`up{}` for free.** Every scrape produces a synthetic `up` metric: 1 if the target answered, 0 if not. With push, a dead app simply goes silent — you can't tell "crashed" from "nothing to report". With pull, *absence is a signal* (Day 33 builds an alert on exactly this).
3. **No client-side buffering.** The app keeps counters in memory; Prometheus carries the storage burden. Your app can't lose metrics in a network blip — the counter is still there at the next scrape.

The cost: short-lived jobs can't be scraped (that's what the Pushgateway is for — niche, don't reach for it by default).

### The TSDB and the data model

Prometheus stores **time series**: a metric name plus a unique set of label key/values, with `(timestamp, float64)` samples appended every scrape interval. `podlab_http_requests_total{path="/",method="GET",code="200"}` is *one series*; change any label value and it's a different series. Series count — **cardinality** — is the resource that kills Prometheus servers, not sample volume. Never put unbounded values (user IDs, full URLs, pod IPs) in labels.

### The exposition format — reading podlab's `/metrics`

```sh
kubectl run -n default tmp --rm -it --image=curlimages/curl --restart=Never -- \
  curl -s http://podlab.podlab-dev.svc:8080/metrics | grep podlab
```

Annotated:

```text
# HELP podlab_http_requests_total Total HTTP requests handled, by path, method and status code.
# TYPE podlab_http_requests_total counter        <- counter: only ever goes up (resets to 0 on restart)
podlab_http_requests_total{code="200",method="GET",path="/"} 42

# TYPE podlab_http_request_duration_seconds histogram   <- histogram: cumulative buckets
podlab_http_request_duration_seconds_bucket{path="/",le="0.005"} 40   <- 40 requests took <= 5ms
podlab_http_request_duration_seconds_bucket{path="/",le="+Inf"} 42    <- all requests (== _count)
podlab_http_request_duration_seconds_sum{path="/"} 0.123              <- total seconds spent
podlab_http_request_duration_seconds_count{path="/"} 42

# TYPE podlab_build_info gauge        <- gauge: can go up or down; here it's an info-style gauge
podlab_build_info{color="none",version="v1"} 1    <- value is always 1; the LABELS carry the data
```

Counter, gauge, histogram — those three types cover 95% of real instrumentation. Day 31 teaches the query side (`rate`, `histogram_quantile`).

### The exporter ecosystem — FROM vs ABOUT

| Exporter | What it exposes | FROM or ABOUT? |
|---|---|---|
| your app (`podlab /metrics`) | requests, latency, business logic | **FROM** the app — only it knows |
| node-exporter (DaemonSet) | CPU, memory, disk, network of each *node* | **FROM** the host OS |
| cAdvisor (built into kubelet) | per-*container* CPU/memory (`container_*`) | **FROM** the container runtime |
| kube-state-metrics | `kube_pod_status_phase`, `kube_deployment_status_replicas_unavailable`… | **ABOUT** Kubernetes objects |

The FROM/ABOUT distinction matters: kube-state-metrics never talks to your pods — it watches the API server and converts *object state* into metrics. "Is the deployment fully rolled out?" is an ABOUT question (kube-state-metrics); "is the app fast?" is a FROM question (app metrics). You need both, and interviewers love asking which is which.

### The Prometheus Operator — config as Kubernetes objects

Classic Prometheus is configured by one giant `prometheus.yml`. That file is a merge conflict magnet and can't be owned per-team. The **Prometheus Operator** replaces it with CRDs:

| CRD | Replaces | You'll use it on |
|---|---|---|
| `Prometheus` | the server deployment + global config | today (the chart manages it) |
| `ServiceMonitor` / `PodMonitor` | `scrape_configs` entries | Day 32 |
| `PrometheusRule` | recording/alerting rule files | Day 33 |
| `Alertmanager` / `AlertmanagerConfig` | Alertmanager deployment/config | Day 33 |

The operator watches these objects and regenerates the real config on the fly. Why this wins in GitOps: a team adds monitoring for its app by committing a `ServiceMonitor` *next to its Deployment* — same repo, same PR, same ArgoCD sync. No central config file, no monitoring-team bottleneck.

**kube-prometheus-stack** is the umbrella Helm chart: operator + Prometheus + Alertmanager + Grafana + node-exporter + kube-state-metrics + a curated pile of dashboards and alert rules. One chart, a working monitoring platform.

One flag you must understand **today**: by default the chart tells Prometheus to only select ServiceMonitors carrying the Helm release label — i.e. *only the chart's own monitors*. Setting `serviceMonitorSelectorNilUsesHelmValues: false` makes Prometheus select **every ServiceMonitor in every namespace**. Without it, your Day 32 ServiceMonitor would be silently ignored — the single most common "why isn't my app scraped" bug.

## Lab

### 1. Look at the chart before installing

```sh
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm search repo prometheus-community/kube-prometheus-stack   # note the current version — pin it below
helm show values prometheus-community/kube-prometheus-stack | wc -l   # ~5000 lines; you'll set ~30
```

### 2. Write the ArgoCD Application

Create `argocd/apps/monitoring.yaml` in your **k8s-gitops** repo. Requirements:

- Helm chart `kube-prometheus-stack` from the prometheus-community repo, **pinned** to the version you found above
- Destination: ns `monitoring`, sync-wave `0`, automated sync + selfHeal + prune
- Sync options: `CreateNamespace=true` and `ServerSideApply=true` (the stack's CRDs are bigger than the 262KB annotation limit client-side apply needs — without SSA the sync fails)
- Lean values inline (`helm.valuesObject`): retention 2d, no persistent storage, selector-nil flags false, ingresses for `prometheus.localhost` / `grafana.localhost` / `alertmanager.localhost`, kind's unreachable control-plane components disabled

<details><summary>Solution</summary>

```yaml
# argocd/apps/monitoring.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: monitoring
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://prometheus-community.github.io/helm-charts
    chart: kube-prometheus-stack
    targetRevision: 77.5.0        # pin what `helm search repo` showed you today
    helm:
      valuesObject:
        prometheus:
          ingress:
            enabled: true
            ingressClassName: nginx
            hosts: [prometheus.localhost]
            paths: ["/"]
          prometheusSpec:
            retention: 2d
            # CRITICAL: pick up ServiceMonitors/Rules from ANY namespace,
            # not just ones labeled as part of this Helm release. Day 32/33 depend on this.
            serviceMonitorSelectorNilUsesHelmValues: false
            podMonitorSelectorNilUsesHelmValues: false
            ruleSelectorNilUsesHelmValues: false
            resources:
              requests: {cpu: 200m, memory: 512Mi}
              limits: {memory: 1536Mi}
            # no storageSpec => emptyDir. Restart loses history; fine for 2d retention on a laptop.
        alertmanager:
          ingress:
            enabled: true
            ingressClassName: nginx
            hosts: [alertmanager.localhost]
            paths: ["/"]
        grafana:
          ingress:
            enabled: true
            ingressClassName: nginx
            hosts: [grafana.localhost]
        # kind runs these on 127.0.0.1 inside the control-plane node — unreachable
        # for scraping. Disable the components AND their default alert rules so the
        # Targets page stays honest instead of permanently red.
        kubeScheduler: {enabled: false}
        kubeControllerManager: {enabled: false}
        kubeEtcd: {enabled: false}
        kubeProxy: {enabled: false}
        defaultRules:
          rules:
            etcd: false
            kubeProxy: false
            kubeControllerManager: false
            kubeSchedulerAlerting: false
            kubeSchedulerRecording: false
  destination:
    server: https://kubernetes.default.svc
    namespace: monitoring
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true
```

</details>

### 3. Ship it

```sh
cd ~/Code/k8s-gitops
git add argocd/apps/monitoring.yaml
git commit -m "Add kube-prometheus-stack as ArgoCD app"
git push
argocd app sync root          # or wait for the root app's poll
argocd app wait monitoring --health --timeout 600
kubectl get pods -n monitoring    # operator, prometheus-0, alertmanager-0, grafana, kube-state-metrics, 3x node-exporter
```

First sync takes a few minutes (CRDs + images). Watch it in k9s (`:pods` in `monitoring`).

### 4. Log into both UIs

```sh
open http://prometheus.localhost:8080
kubectl get secret -n monitoring monitoring-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
open http://grafana.localhost:8080      # user: admin
```

### 5. Tour Prometheus

- **Status → Targets**: every target listed exists because the chart shipped a ServiceMonitor for it. Match each job to its FROM/ABOUT row in the table above: `node-exporter` (FROM hosts), `kube-state-metrics` (ABOUT objects), `kubelet` (cAdvisor, FROM containers), `apiserver`, `coredns`, plus the stack's own components. Note what's **not** there: podlab. Nothing selects it — Day 32 fixes that.
- **Status → Service Discovery**: per job, "discovered" vs "kept" targets — relabeling decides who survives. Remember this page; Day 32 explains the machinery.
- **Status → TSDB Status**: top series counts by metric name. Look who's at the top (usually apiserver histograms) — cardinality made visible.

### 6. First queries (Graph tab)

```promql
up                                          # every target, 1 or 0 — your free liveness signal
count by (job) (up)                         # targets per job
node_memory_MemAvailable_bytes              # FROM node-exporter, one series per node
kube_pod_status_phase{phase="Running"}      # ABOUT pods, from kube-state-metrics
```

Flip to the Graph view on `node_memory_MemAvailable_bytes` and watch real samples accumulate.

### 7. Tour Grafana

Dashboards → search "Kubernetes / Compute Resources / Namespace (Pods)". Pick `podlab-prod`. CPU/memory per pod with zero work from you — these panels read cAdvisor + kube-state-metrics, both already scraped. The chart shipped the dashboards too.

## Verify ✅

- [ ] `argocd app get monitoring` → Synced + Healthy
- [ ] `kubectl get servicemonitors -n monitoring` lists ~10 monitors (the chart's own)
- [ ] Prometheus **Status → Targets**: all targets UP (scheduler/etcd/controller-manager/kube-proxy absent because you disabled them — that's the "explained" part)
- [ ] `count by (job) (up)` in the Prometheus UI returns a non-empty vector
- [ ] Grafana login works with the password from the secret
- [ ] "Kubernetes / Compute Resources / Namespace (Pods)" dashboard shows data for `podlab-prod`
- [ ] `kubectl get prometheus -n monitoring -o yaml | grep -A2 serviceMonitorSelector` shows an **empty** (`{}`) selector — proof the nil-uses-helm-values flag landed

## Interview corner 💬

**"Why does Prometheus pull instead of push? What breaks with pull?"**
Pull lets Prometheus discover targets from the orchestrator's source of truth (the Kubernetes API), gives a built-in liveness signal (`up`) since a dead target fails its scrape, and keeps clients stateless — no buffering or backpressure in the app. What breaks: short-lived batch jobs may finish between scrapes (Pushgateway exists for that), and targets must be network-reachable from Prometheus, which matters across NAT/edge environments.

**"What's the difference between kube-state-metrics and metrics-server?"**
metrics-server feeds the *resource pipeline* — live CPU/memory for `kubectl top` and HPA, no history, not for monitoring. kube-state-metrics converts Kubernetes *object state* (deployment replicas, pod phases, job status) into Prometheus metrics — nothing about resource usage. They overlap zero percent; a real cluster runs both.

**"Why use the Prometheus Operator instead of prometheus.yml?"**
Scrape config, alert rules, and Alertmanager config become CRDs, so each team ships monitoring config in the same Git repo and PR as the app it monitors, validated by the API server and synced by GitOps. The operator merges everything into the live config — no central file, no monitoring-team bottleneck, and RBAC controls who can add scrape targets.

## Stretch goals

- Add a 5Gi PVC via `prometheusSpec.storageSpec.volumeClaimTemplate` (kind's `standard` StorageClass) and confirm history survives a `kubectl delete pod prometheus-monitoring-kube-prometheus-prometheus-0`.
- Open **Status → Configuration** in Prometheus and find the scrape job the operator generated for kube-state-metrics — this is the prometheus.yml you didn't have to write.
- `kubectl get prometheusrules -n monitoring` and skim `kubernetes-apps` — these shipped rules are Day 33's reading material.

## Cleanup

**Nothing.** The monitoring namespace and everything in it **stays through Day 50** — every remaining observability day, the canary analysis on Day 39, and the capstone build on it. You may delete the throwaway `tmp` curl pod if it lingers (`kubectl delete pod tmp` — `--rm` usually handles it).
