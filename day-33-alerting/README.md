# Day 33 — Alerting: PrometheusRule & Alertmanager

> **Time:** ~3.5 h · **Builds on:** Days 30, 32

## Objectives

- Write alert rules as a `PrometheusRule` in Git: expr, `for`, severity labels, templated annotations
- Configure Alertmanager routing, grouping, silences, and inhibition through GitOps
- Fire a real alert by breaking podlab, watch it travel Prometheus → Alertmanager → webhook receiver, then resolve
- Argue for symptom-based alerting and the page-vs-ticket split

## Concepts

### Alert on symptoms, not causes

A cause-based alert ("pod restarted", "CPU > 80%") fires constantly in a healthy Kubernetes cluster — pods restart, CPUs spike, nobody cares. A **symptom**-based alert fires when *users* feel something: error ratio up, latency up, traffic gone. Causes are for dashboards (you look at them *after* the symptom pages you); symptoms are for alerts. The standard symptom sets:

- **Four golden signals** (Google SRE): latency, traffic, errors, saturation.
- **RED** (per service): Rate, Errors, Duration — Day 36 builds this as a dashboard.

Second axis — **page vs ticket**. A page wakes a human at 03:00; it must be urgent, actionable, and user-visible (`severity: critical`). Everything else is a ticket: real, but it can wait for business hours (`severity: warning`). Routing by `severity` label is how Alertmanager implements the split. Every alert that pages but isn't actionable trains your on-call to ignore pages — alert fatigue is a security incident in slow motion.

### Anatomy of an alert rule

```yaml
- alert: PodlabHighErrorRate                       # name (becomes the alertname label)
  expr: <error ratio> > 0.05                       # ANY series returned = alert instance per series
  for: 2m                                          # must be true CONTINUOUSLY this long
  labels:
    severity: warning                              # routing handle — Alertmanager matches on labels
  annotations:                                     # human payload — templated per firing series
    summary: "podlab error rate is {{ $value | humanizePercentage }} in {{ $labels.namespace }}"
```

Lifecycle: expr returns series → **pending** (yellow) → still true after `for` → **firing** (red) → expr empty → resolved. `for` is your flap filter: a 10-second blip never pages anyone. Note the data model: one *rule* can produce many *alert instances* — one per series the expr returns (e.g. per namespace). `{{ $labels.x }}` and `{{ $value }}` template each instance's annotations.

### The delivery path

```text
Prometheus (evaluates rules every ~30s)
   | pushes firing/resolved alerts
   v
Alertmanager ── groups ── routes ── silences? ── inhibited? ──> receiver (webhook/Slack/PagerDuty)
```

Prometheus decides **what is wrong**; Alertmanager decides **who hears about it and how often**:

- **Grouping** — 30 pods crash, you get *one* notification listing 30 alerts. `group_by` picks the bundling key; `group_wait` (how long to collect the initial batch, ~30s), `group_interval` (how often to send updates about a changed group), `repeat_interval` (re-nag frequency for unresolved alerts, hours).
- **Routing** — a tree of matchers; first matching route wins (unless `continue: true`). Standard shape: match on `severity`.
- **Silences** — ad-hoc mutes with a matcher, an expiry, and a *comment* (audit trail). For planned maintenance.
- **Inhibition** — "if X fires, suppress Y": when the critical "app is gone" alert fires, the warning "app is slow" alert is noise. Configured, not ad-hoc.

### The alert that watches the watchers

`absent(up{job="podlab"})` returns 1 only when **no such series exists**. If podlab vanishes — or its ServiceMonitor is deleted, or scraping silently breaks — every threshold alert about podlab goes quiet too, because their exprs return nothing. **An alert whose data disappears doesn't fire; it evaporates.** Every service needs one absence alert; it's the alert that fires when the thing that fires alerts is gone.

## Lab

### 1. A home for monitoring config in Git

Create a directory app in **k8s-gitops** for rules (Day 34 adds dashboards to it):

```text
k8s-gitops/
├── argocd/apps/monitoring-config.yaml    # new Application, wave 1, dir below -> ns monitoring
└── monitoring/
    ├── kustomization.yaml                # namespace: monitoring / resources: [rules/podlab-alerts.yaml]
    └── rules/podlab-alerts.yaml
```

<details><summary>Solution — Application + kustomization</summary>

```yaml
# argocd/apps/monitoring-config.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: monitoring-config
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"     # after the stack (wave 0) — the CRDs must exist
spec:
  project: default
  source:
    repoURL: https://github.com/<you>/k8s-gitops.git   # your repo URL, as in other apps
    targetRevision: main
    path: monitoring
  destination:
    server: https://kubernetes.default.svc
    namespace: monitoring
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

```yaml
# monitoring/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: monitoring
resources:
  - rules/podlab-alerts.yaml
```

</details>

### 2. Write the PrometheusRule

`monitoring/rules/podlab-alerts.yaml`, two alerts. Requirements:

- **PodlabHighErrorRate** — Day 32's error-ratio expr, per namespace, `> 0.05` for `2m`, `severity: warning`, annotations using `$labels.namespace` and `$value`
- **PodlabAbsent** — `absent(up{job=~".*podlab.*"})` for `2m`, `severity: critical`
- Thanks to Day 30's `ruleSelectorNilUsesHelmValues: false`, no special labels needed for pickup

<details><summary>Solution</summary>

```yaml
# monitoring/rules/podlab-alerts.yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: podlab-alerts
spec:
  groups:
    - name: podlab
      rules:
        - alert: PodlabHighErrorRate
          expr: |
            sum by (namespace) (rate(podlab_http_requests_total{code=~"5.."}[5m]))
            /
            sum by (namespace) (rate(podlab_http_requests_total[5m]))
            > 0.05
          for: 2m
          labels:
            severity: warning
          annotations:
            summary: >-
              podlab in {{ $labels.namespace }} is serving
              {{ $value | humanizePercentage }} errors
            description: >-
              More than 5% of requests to podlab in {{ $labels.namespace }} returned
              5xx over the last 5 minutes. Check the RED dashboard, then the logs.
        - alert: PodlabAbsent
          expr: absent(up{job=~".*podlab.*"})
          for: 2m
          labels:
            severity: critical
          annotations:
            summary: "No podlab targets are being scraped at all"
            description: >-
              up{} for podlab has no series: every podlab pod is gone, or scraping
              is broken. All other podlab alerts are blind right now.
```

</details>

Commit, push, sync. Confirm pickup: Prometheus UI → **Alerts** → both rules listed, green/inactive.

### 3. Deploy the webhook receiver — podlab as alert sink

No Slack, no internet: podlab returns `200` to any `POST /` and logs it as JSON. Perfect receiver.

```sh
kubectl create namespace hooks
kubectl create deployment hook-sink -n hooks --image=podlab:v1 --port=8080
kubectl expose deployment hook-sink -n hooks --port=8080
```

(Imperative on purpose — it's a lab prop. Stretch goal: move it to Git.)

### 4. Alertmanager config via GitOps

Edit `argocd/apps/monitoring.yaml` (Day 30) and add `alertmanager.config` to `valuesObject`. Requirements: root route to a `"null"` receiver; `severity=critical` → the webhook; `severity=warning` → a `ticket` receiver (null body + comment — stands in for Jira); group by `alertname, namespace`; an inhibit rule muting warnings when a critical fires for the same namespace. **Also** route the webhook for warnings *today* so you can see delivery (move it to ticket after).

<details><summary>Solution — add under <code>valuesObject:</code></summary>

```yaml
        alertmanager:
          config:
            route:
              receiver: "null"
              group_by: [alertname, namespace]
              group_wait: 15s          # short for the lab; ~30s in real life
              group_interval: 1m
              repeat_interval: 12h
              routes:
                - matchers: ['severity = "critical"']
                  receiver: hook-sink
                - matchers: ['severity = "warning"']
                  receiver: hook-sink   # demo: see delivery. Later: switch to "ticket"
            receivers:
              - name: "null"
              - name: ticket            # pretend ticket queue — a real setup posts to Jira/GitHub
              - name: hook-sink
                webhook_configs:
                  - url: http://hook-sink.hooks.svc:8080/
                    send_resolved: true
            inhibit_rules:
              - source_matchers: ['severity = "critical"']
                target_matchers: ['severity = "warning"']
                equal: [namespace]      # only inhibit warnings about the SAME app/ns
          ingress:
            enabled: true
            # ... (keep what you already had)
```

</details>

Commit/push/sync, then check it landed: `open http://alertmanager.localhost:8080` → **Status** → your route tree in the config dump.

### 5. Fire it for real

```sh
HOST=podlab-prod.localhost ERROR_RATE=0.5 ../day-31-promql/traffic.sh
```

Now watch the lifecycle (this takes ~5 min of patience — `[5m]` rate window + `for: 2m`):

1. Prometheus → **Alerts**: `PodlabHighErrorRate` turns yellow **pending**, with `namespace=podlab-prod`.
2. After 2 continuous minutes: red **firing**.
3. Alertmanager UI: the alert appears, grouped under `alertname=PodlabHighErrorRate, namespace=podlab-prod`.
4. Delivery proof — the webhook POST in podlab-the-receiver's logs:

```sh
kubectl logs -n hooks deploy/hook-sink --tail=5
# {"level":"INFO","msg":"request","path":"/","method":"POST",...}   <- method=POST = Alertmanager calling
```

### 6. Silence it

Alertmanager UI → the alert → **Silence**: matcher `alertname=PodlabHighErrorRate`, duration 1h, comment `Day 33 lab — testing silences`. The alert stays firing in *Prometheus* (truth doesn't change) but vanishes from Alertmanager's active view and stops notifying — check hook-sink logs go quiet past the next `repeat_interval`/group update. CLI equivalent (amtool ships inside the Alertmanager container):

```sh
kubectl exec -n monitoring alertmanager-monitoring-kube-prometheus-alertmanager-0 -c alertmanager -- \
  amtool silence query --alertmanager.url=http://localhost:9093
```

Expire the silence in the UI when done.

### 7. Resolve

Stop the error traffic (Ctrl-C; optionally restart with `ERROR_RATE=0`). Within ~5–7 minutes (the rate window must drain below 5%) the alert resolves; with `send_resolved: true` a final POST lands in hook-sink's logs. Optionally test inhibition: scale podlab to zero in *one* env via Git (or `kubectl scale deploy podlab -n podlab-dev --replicas=0` and let selfHeal undo it later) → `PodlabAbsent` only fires if **all** podlab targets vanish — discuss with yourself why `absent()` over a job regex behaves that way, and what a per-namespace absence alert would need (`absent()` can't be aggregated `by (namespace)`; you'd write one rule per env or use `up == 0`).

## Verify ✅

- [ ] `kubectl get prometheusrules -n monitoring podlab-alerts` exists and Prometheus → Alerts lists both rules
- [ ] You watched pending → firing on the Prometheus Alerts page
- [ ] Alertmanager UI showed the alert grouped, with your `severity: warning` label
- [ ] `kubectl logs -n hooks deploy/hook-sink | grep POST` shows webhook deliveries (and later, the resolved notification)
- [ ] Your silence suppressed notifications while Prometheus kept showing the alert firing
- [ ] After stopping error traffic, the alert left the firing state on its own

## Interview corner 💬

**"How do you decide what should page someone at 3am?"**
Page on symptoms users feel — error ratio, latency, availability — never on causes like CPU or restarts. The test: is it urgent, is it actionable, does a user notice? If any answer is no, it's a ticket. Mechanically: `severity: critical` routes to the pager, `warning` to a queue, and inhibition rules stop a critical from dragging its warning-level shadows along. Every false page erodes on-call trust, so we'd rather under-page and review tickets daily.

**"Prometheus fired but nobody got notified. Walk me through debugging."**
Two systems, four checkpoints. (1) Prometheus Alerts page — is it *firing* or stuck pending (check `for`)? (2) Did it reach Alertmanager — Status page lists Alertmanagers; check the AM UI for the alert. (3) Inside Alertmanager — is a silence or inhibition eating it, does any route match its labels (a label typo means it falls to the root receiver)? (4) The receiver — webhook returning non-2xx shows in Alertmanager logs. The classic culprits: selector labels (rule never loaded — operator selector mismatch), `for` too long, and a route matcher that doesn't match.

**"What is the watchdog/absent pattern?"**
Threshold alerts fail silent: no data means no series means no alert. Two defenses: `absent()` rules per service that fire when the metric disappears, and the stack's always-firing `Watchdog` alert wired to a dead-man's-switch receiver — if the watchdog *stops* arriving, the pipeline itself is down. You alert on the absence of signal, not just bad signal.

## Stretch goals

- Move hook-sink into k8s-gitops as a proper app (its own `argocd/apps/hooks.yaml`).
- Add a recording rule to the same PrometheusRule: `namespace:podlab_error_ratio:rate5m`, then rewrite the alert expr to use it — config dedup in action (Day 39's analysis can reuse it too).
- Flip the warning route to the `ticket` receiver and verify warnings stop reaching hook-sink while criticals still would.

## Cleanup

Keep **everything**: the PrometheusRule, Alertmanager config, and the `hooks` namespace all stay — Day 36's drill and the Day 49 capstone demo re-fire this exact alert. Expire any leftover silences (they'd eat Day 36's drill). Stop traffic loops if you're done for the day.
