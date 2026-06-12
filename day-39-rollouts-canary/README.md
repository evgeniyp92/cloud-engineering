# Day 39 — Argo Rollouts II: Canary + Automated Analysis

> **Time:** ~4 h · **Builds on:** Days 38, 33–36 (Prometheus, RED dashboard)

## Objectives

- Run a canary release where ingress-nginx shifts **real traffic** in precise percentage steps: 20 → 50 → 100.
- Gate and guard the release with **AnalysisRuns** that query your Prometheus from Phase 5 — a deploy that judges itself.
- Break a release on purpose and watch the platform **roll it back automatically**: analysis fails → abort → traffic back to stable, no human in the loop.
- Walk away with a rehearsed five-minute demo — this is the single best "show me something" answer you have for interviews.

## Concepts

### Canary: judge with real traffic

Blue-green answered "does v2 work when *we* test it?" Canary answers a harder question: "does v2 work under *real* traffic?" — the only place latency regressions, memory creep, and that one weird client actually show up. The strategy: send a small slice of production traffic to the new version, **measure**, and only widen the slice if the numbers hold. If they don't, retreat automatically. It's the closest thing to a self-driving deploy, and it's built from three parts: traffic splitting, step definitions, and analysis.

### How traffic actually splits

A canary `setWeight: 20` can be implemented two ways, and the difference matters:

| Mechanism | How | Precision | Needs |
|---|---|---|---|
| **Replica ratio** | run ~20% of pods as canary; the Service round-robins across all | approximate (quantized by replica count: with 3 replicas your choices are 0/33/67/100) | nothing |
| **Traffic routing** | the controller programs a proxy — for ingress-nginx, it maintains a second *canary Ingress* with `nginx.ingress.kubernetes.io/canary: "true"` and `canary-weight: "20"` annotations | exact 20%, independent of replica counts | an ingress controller or mesh |

You have ingress-nginx, so today uses **`trafficRouting.nginx`**. You give the Rollout your existing Ingress (`stableIngress`) plus two Services (`stableService`, `canaryService`); the controller clones the Ingress, points the clone at the canary Service, and turns the weight knob on its annotations at each step. Watching those annotations change *is* watching the release happen.

### Steps: the release as data

```yaml
steps:
  - setWeight: 20          # 20% of traffic → canary
  - pause: {duration: 1m}  # hold; background analysis is judging
  - analysis: {...}        # explicit gate: one measurement must pass
  - setWeight: 50
  - pause: {duration: 1m}
  - setWeight: 100
```

`pause: {duration: 1m}` holds for a fixed time; `pause: {}` (no duration) holds **forever** until someone runs `promote` — that's how you mix automation with a human checkpoint. The step list is the release process, reviewable in git like everything else.

### Analysis: the judge

Two CRDs:

- **AnalysisTemplate** — the reusable definition: which provider (Prometheus, Datadog, a Job, a web hook…), what query, what counts as success, how often to measure.
- **AnalysisRun** — one execution, created by the controller during a rollout. It's a first-class object: `kubectl get analysisrun` shows you every measurement and value.

The fields that matter:

| Field | Meaning |
|---|---|
| `provider.prometheus.address/query` | where and what to ask |
| `successCondition` | expression over `result` — Prometheus returns a vector, so you index it: `result[0] >= 0.95` |
| `interval` | re-measure every N (e.g. `30s`) |
| `count` | how many measurements. Omitted: **one** measurement for an inline step; **continuous until the rollout ends** for background analysis |
| `failureLimit` | how many failed measurements before the whole run fails (and the rollout aborts) |

Two ways to attach it, and you'll use both:

1. **Inline** (`- analysis:` as a step): a blocking gate at a specific point. The rollout does not advance until it passes.
2. **Background** (`strategy.canary.analysis`): runs continuously alongside *all* steps from `startingStep` onward — a watchdog. The moment it fails, the rollout aborts mid-step, whatever step that is.

### The math of the success-rate query

```promql
sum(rate(podlab_http_requests_total{namespace="rollouts-lab",code!~"5.."}[2m]))
/
sum(rate(podlab_http_requests_total{namespace="rollouts-lab"}[2m]))
```

Numerator: requests/sec that did **not** return 5xx. Denominator: all requests/sec. The ratio is the success rate, 0.0–1.0; `>= 0.95` means "tolerate up to 5% errors". `rate(...[2m])` smooths over two minutes — long enough to not flap on a single failed request, short enough to react within a couple of intervals. Edge case to know: if there's **no traffic**, both sides are zero, the query returns an empty vector / NaN, and the measurement errors out — which is why a traffic generator runs throughout this lab (and why real canaries need a minimum-traffic guard).

### The honest limitation (read this before Run 2)

This query is **namespace-scoped, not version-scoped**. It measures the *blend* of stable + canary traffic. That's genuinely how many teams start, and it works — at 50% weight, a bad canary drags the blended success rate down past the threshold and triggers the abort. But notice what it can't do: at 5% weight, a canary failing 50% of its requests only moves the blend by 2.5%. Real precision requires attributing errors to a version — and you have the tool for that: `podlab_build_info{version,color}` from Day 36, joinable on `pod`. The per-version query is today's stretch goal; the lab deliberately runs the simple query and makes the bad release loud enough (and the weight high enough) for the blend to catch it. Know the limitation; be able to say it out loud in an interview.

## Lab

### 0. Preflight

Day 38's namespace, controller, and plugin must be in place. Restore replicas if you scaled down:

```sh
kubectl get pods -n argo-rollouts                 # controller Running
kubectl get rollout podlab -n rollouts-lab        # Day 38's blue-green rollout, Healthy
```

Confirm the in-cluster Prometheus address — the AnalysisTemplate needs it exactly right:

```sh
kubectl get svc -n monitoring | grep prometheus
```

You're looking for `kube-prometheus-stack-prometheus` on port `9090` → address `http://kube-prometheus-stack-prometheus.monitoring:9090`. If your service name differs, adjust everywhere below.

### 1. Get rollouts-lab metrics into Prometheus

Your Day 34 ServiceMonitor scrapes the podlab-* namespaces — not `rollouts-lab`. No metrics, no analysis. Add a **PodMonitor** (scrapes pods directly, no Service needed, no double-scraping when two Services select the same pods):

```yaml
# podmonitor.yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: podlab-rollouts-lab
  namespace: monitoring
  labels:
    release: kube-prometheus-stack   # must match your Prometheus's selector (as on Day 34)
spec:
  namespaceSelector:
    matchNames: ["rollouts-lab"]
  selector:
    matchLabels:
      app: podlab-canary
  podMetricsEndpoints:
    - port: http        # the *named* container port — name it in the Rollout below
      path: /metrics
```

```sh
kubectl apply -f podmonitor.yaml
```

(Verify in step 4, once pods with that label exist.)

### 2. The AnalysisTemplate

Requirements — write it, then check against [`analysis-template.yaml`](analysis-template.yaml):

- `AnalysisTemplate` named `podlab-success-rate` in `rollouts-lab`.
- One metric `success-rate`: provider `prometheus`, address from step 0, the success-rate query from Concepts.
- `successCondition: result[0] >= 0.95`, `interval: 30s`, `failureLimit: 1`, **no `count`** (one-shot when used inline, continuous when used in background — exactly the dual use we want).

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AnalysisTemplate
metadata:
  name: podlab-success-rate
  namespace: rollouts-lab
spec:
  metrics:
    - name: success-rate
      interval: 30s
      successCondition: result[0] >= 0.95
      failureLimit: 1
      provider:
        prometheus:
          address: http://kube-prometheus-stack-prometheus.monitoring:9090
          query: |
            sum(rate(podlab_http_requests_total{namespace="rollouts-lab",code!~"5.."}[2m]))
            /
            sum(rate(podlab_http_requests_total{namespace="rollouts-lab"}[2m]))
```

</details>

```sh
kubectl apply -f analysis-template.yaml
```

### 3. The canary Rollout

Requirements — the complete reference is [`rollout-canary.yaml`](rollout-canary.yaml):

- **Rollout** `podlab-canary` in `rollouts-lab`: 4 replicas, image `podlab:v1`, env `VERSION=1.0.0` / `COLOR=blue`, container port 8080 **named `http`** (the PodMonitor depends on it), readiness probe on `/healthz`, pod label `app: podlab-canary`.
- Two plain **Services** `podlab-stable` and `podlab-canary-svc` (selector `app: podlab-canary`, port 80 → `http`).
- One **Ingress** `podlab-stable`, host `canary.localhost`, backend `podlab-stable` — this is the `stableIngress` the controller will clone.
- `strategy.canary`: `stableService: podlab-stable`, `canaryService: podlab-canary-svc`, `trafficRouting.nginx.stableIngress: podlab-stable`.
- Steps: `setWeight: 20` → `pause: {duration: 1m}` → inline `analysis` using `podlab-success-rate` → `setWeight: 50` → `pause: {duration: 1m}` → `setWeight: 100`.
- **Background analysis**: `strategy.canary.analysis` with the same template, `startingStep: 1` (don't judge before any traffic shifts).

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: podlab-canary
  namespace: rollouts-lab
spec:
  replicas: 4
  selector:
    matchLabels:
      app: podlab-canary
  template:
    metadata:
      labels:
        app: podlab-canary
    spec:
      containers:
        - name: podlab
          image: podlab:v1
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: VERSION
              value: "1.0.0"
            - name: COLOR
              value: "blue"
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            periodSeconds: 5
  strategy:
    canary:
      stableService: podlab-stable
      canaryService: podlab-canary-svc
      trafficRouting:
        nginx:
          stableIngress: podlab-stable
      analysis:                      # background watchdog
        templates:
          - templateName: podlab-success-rate
        startingStep: 1
      steps:
        - setWeight: 20
        - pause: {duration: 1m}
        - analysis:                  # inline one-shot gate
            templates:
              - templateName: podlab-success-rate
        - setWeight: 50
        - pause: {duration: 1m}
        - setWeight: 100
---
apiVersion: v1
kind: Service
metadata:
  name: podlab-stable
  namespace: rollouts-lab
spec:
  selector:
    app: podlab-canary
  ports:
    - name: http
      port: 80
      targetPort: http
---
apiVersion: v1
kind: Service
metadata:
  name: podlab-canary-svc
  namespace: rollouts-lab
spec:
  selector:
    app: podlab-canary
  ports:
    - name: http
      port: 80
      targetPort: http
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: podlab-stable
  namespace: rollouts-lab
spec:
  ingressClassName: nginx
  rules:
    - host: canary.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: podlab-stable
                port:
                  number: 80
```

</details>

```sh
kubectl apply -f rollout-canary.yaml
kubectl argo rollouts get rollout podlab-canary -n rollouts-lab
curl -s http://canary.localhost:8080/ | python3 -m json.tool | grep -E 'version|color'
```

Look at what the controller created behind your Ingress:

```sh
kubectl get ingress -n rollouts-lab
kubectl get ingress podlab-canary-podlab-stable-canary -n rollouts-lab \
  -o jsonpath='{.metadata.annotations}{"\n"}'
```

A second Ingress, same host, annotated `nginx.ingress.kubernetes.io/canary: "true"` and `canary-weight: "0"`. That weight annotation is the knob the whole day turns.

### 4. Traffic + the watch windows

The fixture [`traffic.sh`](traffic.sh) curls `http://canary.localhost:8080/` continuously and prints a rolling version/color/error tally. Start it and leave it running for the rest of the day:

```sh
chmod +x traffic.sh
./traffic.sh
```

Set up your cockpit — three more terminals/tabs:

```sh
kubectl argo rollouts get rollout podlab-canary -n rollouts-lab -w        # terminal 2
watch -n2 'kubectl get ingress podlab-canary-podlab-stable-canary -n rollouts-lab \
  -o jsonpath="weight={.metadata.annotations.nginx\.ingress\.kubernetes\.io/canary-weight}"'   # terminal 3
kubectl argo rollouts dashboard                                           # terminal 4 (optional)
```

And the observability payoff: open Grafana (http://grafana.localhost:8080) → your podlab RED dashboard, plus a quick Explore query to confirm scraping works (give the PodMonitor a minute):

```promql
sum by (version, color) (podlab_build_info{namespace="rollouts-lab"})
```

If that returns nothing after ~2 min, debug the PodMonitor: label `release: kube-prometheus-stack` present? port name `http`? Prometheus → Status → Targets.

### 5. RUN 1 — a good release

Edit `rollout-canary.yaml`: `VERSION: "2.0.0"`, `COLOR: "green"` (same healthy image — this release deserves to succeed). Apply:

```sh
kubectl apply -f rollout-canary.yaml
```

Now narrate what you see — this is the demo, learn its beats:

1. **Terminal 2**: canary ReplicaSet appears; step 1/6 `setWeight: 20`; the canary-weight annotation (terminal 3) flips `0 → 20`. `traffic.sh` starts showing ~1 in 5 responses as `2.0.0/green` — real traffic, really split.
2. **Background AnalysisRun is born**: `kubectl get analysisrun -n rollouts-lab` — status `Running`, a new measurement every 30s, value ≈ `1.0`.
3. After the 1m pause, the **inline analysis step** runs one measurement, passes, and the rollout advances: weight `50`. Traffic is now half-and-half — watch the version mix shift in `traffic.sh` and on the Grafana RED dashboard (the `podlab_build_info` join from Day 36 shows both versions live; this is why you built it).
4. Second pause passes → `setWeight: 100` → controller promotes: canary becomes stable, annotation returns to `0` (the *stable* ingress now serves the new version), old ReplicaSet scales down. Status: `✔ Healthy`.

Total: ~3 minutes, zero human actions after `kubectl apply`. Inspect the completed run:

```sh
kubectl get analysisrun -n rollouts-lab
kubectl describe analysisrun -n rollouts-lab $(kubectl get analysisrun -n rollouts-lab -o name | tail -1 | cut -d/ -f2) | tail -20
```

Every measurement, timestamped, with the measured value. This is your audit trail.

### 6. RUN 2 — a bad release, and the platform catches it

Now the flagship moment. The "bad release": bump to `VERSION: "3.0.0"`, `COLOR: "red"` and — because podlab has no built-in failure env — degrade it with traffic: restart the generator in **bad mode**, where every third request hits `/error?rate=0.8` (podlab returns a 500 with 80% probability on that path). Overall ≈ 27% of requests fail → success rate ≈ 0.73, far below 0.95.

> **Be precise about what this simulates.** The errors land on stable *and* canary pods (the weight splits `/error` requests too), and our namespace-scoped query can't tell versions apart — see Concepts. What you're really demonstrating is the *mechanism*: degraded metrics during a rollout → analysis fails → automatic abort. With the per-version query (stretch goal), the same mechanism fires only when the *canary* is at fault. Say this sentence in the interview; it shows you understand your own demo.

```sh
# terminal 1: Ctrl-C the good traffic, then
./traffic.sh bad
```

Edit `VERSION: "3.0.0"` / `COLOR: "red"`, apply, and watch terminal 2:

1. Weight steps to 20, background AnalysisRun starts measuring at step 1.
2. First measurement comes back ≈ `0.73` → `Failed (1)`. `failureLimit: 1` means one more strike.
3. Second failed measurement → **AnalysisRun: Failed → Rollout: aborted**. Weight annotation snaps back to `0`, the red ReplicaSet scales to 0, status **`✖ Degraded`**.
4. `traffic.sh` output: every `/` request serves `2.0.0/green`. **Stable was never replaced.** Nobody touched anything.

```sh
kubectl argo rollouts get rollout podlab-canary -n rollouts-lab    # Degraded, aborted
kubectl get analysisrun -n rollouts-lab                            # the Failed run
curl -s http://canary.localhost:8080/ | grep -o '"version":"[^"]*"'
```

Check Alertmanager (http://grafana.localhost:8080 → Alerting, or your Alertmanager UI): if your Day 35 `PodlabHighErrorRate` rule isn't scoped to only the podlab-* namespaces, it's firing right now — the *whole platform* reacting to one bad release: metrics moved, the dashboard showed it, analysis caught it, the rollout retreated, the alert paged. That's the system you've built over 39 days.

Stop the bad traffic (`Ctrl-C`, restart plain `./traffic.sh`), then clear the Degraded state — the spec still says v3, so retreat the spec:

```sh
kubectl argo rollouts undo podlab-canary -n rollouts-lab
kubectl argo rollouts status podlab-canary -n rollouts-lab   # Healthy
```

(In GitOps, `undo` buys time; the durable fix is `git revert` so ArgoCD doesn't re-assert v3.)

### 7. Write down your demo script

Five minutes, rehearse it twice:

1. *"This is a canary deploy gated by live Prometheus metrics."* Show the Rollout YAML: steps + analysis. (30s)
2. Start traffic, apply a good bump, show weight 20 in the `-w` view **and** the nginx annotation **and** both versions on the Grafana dashboard. (90s)
3. Let it auto-promote to 100. *"No human touched it; the metrics were the approval."* (60s)
4. Apply the bad release with bad traffic, watch analysis fail and auto-abort, curl proves stable unharmed. (90s)
5. Close with the limitation + fix: *"This query judges the namespace blend; production-grade is a per-version query joining `podlab_build_info`."* (30s)

## Verify ✅

- [ ] `kubectl get podmonitor -n monitoring podlab-rollouts-lab` exists, and in Prometheus (http://prometheus.localhost:8080) the query `podlab_build_info{namespace="rollouts-lab"}` returns series
- [ ] `kubectl get ingress -n rollouts-lab` → both `podlab-stable` and `podlab-canary-podlab-stable-canary`
- [ ] During RUN 1 at step 1: the canary ingress annotation `canary-weight` = `20`, and `./traffic.sh` shows a mix of `1.0.0` and `2.0.0`
- [ ] RUN 1 ends with `kubectl argo rollouts status podlab-canary -n rollouts-lab` → `Healthy` and `curl canary.localhost:8080/` → `"version":"2.0.0"`, with **no** `promote` command issued
- [ ] RUN 2: `kubectl get analysisrun -n rollouts-lab` shows a run with `STATUS Failed`; rollout `Degraded`; `curl canary.localhost:8080/` still → `"version":"2.0.0"`
- [ ] After `undo`: rollout `Healthy` again

## Interview corner 💬

**"How do you automate rollback?"**
Don't bolt rollback onto deploys — make the deploy self-judging. With Argo Rollouts, a canary shifts traffic in steps while an AnalysisRun queries Prometheus for SLI metrics (success rate, p99 latency) every 30 seconds; if measurements breach the threshold past the failure limit, the controller aborts and routes 100% back to stable automatically — seconds, no pager, no human. The prerequisites are the real answer: per-version metrics, a traffic-splitting layer, and agreed thresholds. Rollback automation is an observability problem wearing a deployment costume.

**"Canary with vs without a service mesh?"**
Without any routing layer, canary is replica-ratio: weight ≈ canary pods / total pods — quantized and coarse (3 replicas can't do 10%), but zero extra infrastructure. With an ingress controller like nginx, the rollout controller manages weight annotations on a cloned ingress — exact percentages for *north-south* traffic, which covers most web services. A mesh (Istio/Linkerd) adds exact splitting for *east-west* (service-to-service) traffic, plus header-based routing for session-pinned canaries. I reach for the lightest layer that covers the traffic I need to split.

**"What metric gates a deploy, concretely?"**
Start from SLIs: success rate — `1 - (rate of 5xx / rate of all)` over a 2-minute window, threshold like ≥ 99.5%, measured every 30s with a failure limit so one blip doesn't abort. Add p95/p99 latency from the request-duration histogram, compared against the stable baseline rather than an absolute number. Crucially the query must be scoped to the *canary version* — via a version label or a join on a build-info metric — otherwise at 5% weight a fully-broken canary barely moves the blended numbers and the gate is theater. And there must be a minimum-traffic guard: no data should mean "inconclusive", not "pass".

## Stretch goals

- **The per-version query** (do this one — it completes the story). Parameterize the template with `args` and join on `podlab_build_info`:

  ```promql
  sum(rate(podlab_http_requests_total{namespace="rollouts-lab",code!~"5.."}[2m])
      * on(pod) group_left(version) podlab_build_info{namespace="rollouts-lab",version="{{args.canary-version}}"})
  /
  sum(rate(podlab_http_requests_total{namespace="rollouts-lab"}[2m])
      * on(pod) group_left(version) podlab_build_info{namespace="rollouts-lab",version="{{args.canary-version}}"})
  ```

  Add `args: [{name: canary-version}]` to the template and pass the value from the Rollout's analysis blocks. Re-run RUN 2 and confirm it aborts only when the *canary's own* numbers are bad.
- Add a latency metric to the template: `histogram_quantile(0.95, sum by (le) (rate(podlab_http_request_duration_seconds_bucket{namespace="rollouts-lab"}[2m]))) <= 0.5`, drive it with `/load`.
- Replace the final `setWeight: 100` with `pause: {}` (indefinite) — automation up to 50%, human sign-off for full rollout.
- Try `dynamicStableScale: true` and watch the stable ReplicaSet shrink as the canary grows (capacity-neutral canary).

## Cleanup

Keep: **Argo Rollouts, the `rollouts-lab` namespace, `podlab-canary`, the AnalysisTemplate, and the PodMonitor** — Day 40 hangs TLS on this namespace's ingress, and the capstone reuses the whole canary pipeline. Stop `traffic.sh` (Ctrl-C).

Resource pressure option: scale the canary rollout down between days —

```sh
kubectl patch rollout podlab-canary -n rollouts-lab --type merge -p '{"spec":{"replicas":1}}'
```

You may also delete Day 38's blue-green rollout (`kubectl delete rollout podlab -n rollouts-lab`) if you want the namespace lean; keep the canary one.
