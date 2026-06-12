# Day 38 — Argo Rollouts I: Blue-Green

> **Time:** ~3 h · **Builds on:** Days 3, 5, 25–27 (GitOps), 33–36 (observability)

## Objectives

- Explain why a Deployment's `RollingUpdate` strategy can't gate a release on verification, and what progressive delivery adds.
- Install Argo Rollouts **as an ArgoCD Application** — the platform manages its own tooling.
- Run a blue-green release of podlab: new version live on a *preview* URL while production traffic still hits the old one, then promote with one command.
- Abort a bad release and prove production traffic never saw it.

## Concepts

### What a Deployment can't do

You've done dozens of rolling updates since Day 3. Recap what actually happens: the Deployment controller scales up a new ReplicaSet while scaling down the old one, gated only by `maxSurge`/`maxUnavailable` and readiness probes. That gives you three guarantees — and three gaps:

| Rolling update gives you | …but not |
|---|---|
| No downtime (capacity-wise) | **Traffic control** — the Service round-robins across old AND new pods the moment new ones are Ready. Users get a random version mid-rollout. |
| Readiness gating | **Verification** — "Ready" means the probe returns 200. It says nothing about error rates, latency, or whether the new version corrupts data. |
| `kubectl rollout undo` | **Fast, safe rollback** — undo is just another rolling update. If v2 is melting down, you wait for a full re-roll while it keeps serving. |

The readiness probe is a *liveness-grade* signal being asked to make a *release-grade* decision. **Progressive delivery** is the fix: decouple *deploying* the new version (pods exist) from *releasing* it (traffic hits it), and put a gate — human or automated — between the two.

### Argo Rollouts architecture

[Argo Rollouts](https://argo-rollouts.readthedocs.io/) is a Kubernetes controller plus a CRD. Four pieces:

1. **The `Rollout` CRD** — a drop-in replacement for Deployment. Same `spec.template`, same `spec.replicas`, same `selector`; the difference is `spec.strategy` accepts `blueGreen` or `canary` instead of `rollingUpdate`. Under the hood it manages ReplicaSets exactly like a Deployment does — it just orchestrates *when* they scale and *who gets traffic*.
2. **The controller** (namespace `argo-rollouts`) — watches Rollouts and executes the strategy.
3. **The kubectl plugin** (`kubectl argo rollouts ...`) — rich CLI: a live `get rollout -w` view, `promote`, `abort`, `undo`, `retry`.
4. **The dashboard** — a local web UI the plugin serves; same controls, with pictures.

### Blue-green semantics

Two complete environments. *Blue* is what users see; *green* is the candidate. The trick is two Services selecting on a **pod-template hash** that the controller rewrites:

```
                       ┌────────────────────────────┐
 active.localhost ───► │ Service podlab-active      │──► RS rev-1 (blue, v1) ×3
                       │  selector: hash=rev-1      │
                       └────────────────────────────┘
                       ┌────────────────────────────┐
 preview.localhost ──► │ Service podlab-preview     │──► RS rev-2 (green, v2) ×3
                       │  selector: hash=rev-2      │
                       └────────────────────────────┘
                                  ▲
                       promote = controller flips
                       podlab-active's selector to rev-2
```

Key fields under `strategy.blueGreen`:

| Field | What it does |
|---|---|
| `activeService` | Service that always points at the stable ReplicaSet. Production traffic. |
| `previewService` | Service the controller points at the *new* ReplicaSet during an update. Test traffic only. |
| `autoPromotionEnabled: false` | The gate. Without this the controller promotes as soon as green is Ready — which is just an expensive rolling update. `false` means a human (or a pipeline) must run `promote`. |
| `scaleDownDelaySeconds` | How long the old ReplicaSet stays up *after* promotion (default 30s). This is your instant-rollback window: the selector flip back is O(seconds) because blue's pods still exist. |
| `previewReplicaCount` | Optionally run the preview smaller (e.g. 1 pod) until promotion, then scale to full. Saves the 2× cost during the wait. |

Promotion is **a selector edit, not a pod operation**. That's why blue-green switchover and rollback are near-instant: no pods start or stop on the traffic-flip path.

### Where ArgoCD fits

The Rollout *is* a GitOps object: it lives in git, ArgoCD syncs it, and a release starts when you merge a change to the pod template. The promotion gate is the one deliberately human step — ArgoCD will happily show the app `Healthy/Suspended` while a Rollout waits for promotion (it ships a health check for the Rollout CRD). Today you'll work in a scratch namespace with `kubectl apply` to keep the feedback loop tight; Day 39's canary removes the human from the loop entirely.

The cost, stated honestly: full blue-green needs **2× capacity** during every release. That's the price of a complete, instantly-revertible copy.

## Lab

### 1. Install Argo Rollouts as an ArgoCD app

The controller is platform infrastructure, so it goes through GitOps like everything else since Day 26. Create `~/Code/k8s-gitops/argocd/apps/argo-rollouts.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: argo-rollouts
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://argoproj.github.io/argo-helm
    chart: argo-rollouts
    targetRevision: "*"          # or pin the current version — check artifacthub
    helm:
      valuesObject:
        dashboard:
          enabled: false          # we'll use the plugin's local dashboard
  destination:
    server: https://kubernetes.default.svc
    namespace: argo-rollouts
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

```sh
cd ~/Code/k8s-gitops
git add argocd/apps/argo-rollouts.yaml
git commit -m "platform: install argo-rollouts"
git push
```

Watch it arrive (app-of-apps picks it up): open https://argocd.localhost:8443 or run `kubectl get apps -n argocd -w`. Then:

```sh
kubectl get pods -n argo-rollouts
kubectl get crd | grep argoproj
```

You should see `rollouts.argoproj.io`, `analysistemplates.argoproj.io`, `analysisruns.argoproj.io` (those last two are tomorrow's stars), and friends.

### 2. Install the kubectl plugin

```sh
brew install argoproj/tap/kubectl-argo-rollouts
kubectl argo rollouts version
```

### 3. The blue-green Rollout

Work in a fresh namespace — the kustomize podlab envs stay untouched today:

```sh
kubectl create namespace rollouts-lab
```

Now write the day's core artifact. Requirements:

- A **Rollout** named `podlab` in `rollouts-lab`: 3 replicas, image `podlab:v1`, container port 8080, env `VERSION=1.0.0` and `COLOR=blue`, readiness probe on `/healthz`.
- `strategy.blueGreen` with `activeService: podlab-active`, `previewService: podlab-preview`, `autoPromotionEnabled: false`, `scaleDownDelaySeconds: 60`.
- Two **Services** (`podlab-active`, `podlab-preview`), both port 80 → targetPort 8080, both with selector `app: podlab` (the controller injects the hash selector on top — don't add it yourself).
- Two **Ingresses**: host `active.localhost` → `podlab-active`, host `preview.localhost` → `podlab-preview` (class `nginx`, like Day 5).

Write it yourself first; the complete reference is in [`rollout-bluegreen.yaml`](rollout-bluegreen.yaml).

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: podlab
  namespace: rollouts-lab
spec:
  replicas: 3
  selector:
    matchLabels:
      app: podlab
  template:
    metadata:
      labels:
        app: podlab
    spec:
      containers:
        - name: podlab
          image: podlab:v1
          ports:
            - containerPort: 8080
          env:
            - name: VERSION
              value: "1.0.0"
            - name: COLOR
              value: "blue"
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
  strategy:
    blueGreen:
      activeService: podlab-active
      previewService: podlab-preview
      autoPromotionEnabled: false
      scaleDownDelaySeconds: 60
---
apiVersion: v1
kind: Service
metadata:
  name: podlab-active
  namespace: rollouts-lab
spec:
  selector:
    app: podlab
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: podlab-preview
  namespace: rollouts-lab
spec:
  selector:
    app: podlab
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: podlab-active
  namespace: rollouts-lab
spec:
  ingressClassName: nginx
  rules:
    - host: active.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: podlab-active
                port:
                  number: 80
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: podlab-preview
  namespace: rollouts-lab
spec:
  ingressClassName: nginx
  rules:
    - host: preview.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: podlab-preview
                port:
                  number: 80
```

</details>

Apply and inspect:

```sh
kubectl apply -f rollout-bluegreen.yaml
kubectl argo rollouts get rollout podlab -n rollouts-lab
```

Note the output: one revision, `Status: ✔ Healthy`, the ReplicaSet tagged both `stable` and `active`. Look at what the controller did to the Services:

```sh
kubectl get svc podlab-active -n rollouts-lab -o jsonpath='{.spec.selector}{"\n"}'
```

It added `rollouts-pod-template-hash: <hash>` — that hash is the flip switch. Both URLs serve blue right now (preview points at stable when no update is in progress):

```sh
curl -s http://active.localhost:8080/  | python3 -m json.tool | grep -E 'version|color'
curl -s http://preview.localhost:8080/ | python3 -m json.tool | grep -E 'version|color'
```

### 4. Deploy v2 — the parallel-universes moment

In one terminal, start the live watch:

```sh
kubectl argo rollouts get rollout podlab -n rollouts-lab -w
```

In another, edit `rollout-bluegreen.yaml`: `VERSION: "2.0.0"`, `COLOR: "green"` (same image — env-only change, exactly like a config-driven release), then:

```sh
kubectl apply -f rollout-bluegreen.yaml
```

Watch the `-w` view: a `revision:2` ReplicaSet scales to 3, gets tagged `preview`, and the Rollout goes **`॥ Paused`** with message `BlueGreenPause`. Six pods are running. Now the moment this day exists for:

```sh
curl -s http://active.localhost:8080/  | python3 -m json.tool | grep -E 'version|color'   # 1.0.0 / blue
curl -s http://preview.localhost:8080/ | python3 -m json.tool | grep -E 'version|color'   # 2.0.0 / green
```

Two complete versions of production, side by side, on different URLs. This is where you'd run smoke tests, point QA at preview, or replay traffic. Production hasn't changed.

Open the dashboard for the visual version (Ctrl-C when done):

```sh
kubectl argo rollouts dashboard
# → http://localhost:3100/rollouts — select namespace rollouts-lab
```

### 5. Promote

```sh
kubectl argo rollouts promote podlab -n rollouts-lab
curl -s http://active.localhost:8080/ | python3 -m json.tool | grep -E 'version|color'   # 2.0.0 / green — instantly
```

In the `-w` view: rev-2 is now `stable, active`; rev-1 is `delay:60s` — your rollback window — then scales to 0. The traffic flip was a Service selector edit; zero pods started or stopped to make it happen.

### 6. Abort drill — production never sees v3

Edit again: `VERSION: "3.0.0"`, `COLOR: "red"`. Apply. Wait for the pause, confirm preview serves red, then pretend smoke tests failed:

```sh
kubectl argo rollouts abort podlab -n rollouts-lab
curl -s http://active.localhost:8080/ | python3 -m json.tool | grep color   # still green
```

The rev-3 ReplicaSet scales to 0 and the Rollout shows **`✖ Degraded`** — deliberate: your *desired* spec (git, in real life) still says v3, so the controller flags the mismatch rather than pretending all is well. Two ways out:

```sh
# a) "ship it after all": retry the update
kubectl argo rollouts retry rollout podlab -n rollouts-lab
# ...wait for pause, then abort again for the next step

# b) "v3 is dead": roll the spec back to the stable revision
kubectl argo rollouts undo podlab -n rollouts-lab
kubectl argo rollouts status podlab -n rollouts-lab    # Healthy
```

Use option (b). In GitOps reality, `undo` is a stopgap — the real fix is `git revert`, and ArgoCD syncs the Rollout back; otherwise ArgoCD will re-assert v3 on the next sync. The Rollout is the GitOps object; `promote`/`abort` are the human gate *on top of* the desired state, not a replacement for it.

## Verify ✅

- [ ] `kubectl get application argo-rollouts -n argocd` → `Synced` / `Healthy`
- [ ] `kubectl argo rollouts version` → prints client version without error
- [ ] `kubectl get rollout podlab -n rollouts-lab` → `DESIRED 3`, `READY 3`
- [ ] During step 4 (before promote): `curl -s active.localhost:8080/ | grep -o '"color":"[a-z]*"'` → `"color":"blue"` while the same curl against `preview.localhost:8080` → `"color":"green"`
- [ ] After promote: `curl -s active.localhost:8080/` → `"version":"2.0.0"`, and ~60 s later `kubectl get rs -n rollouts-lab` shows the rev-1 ReplicaSet at `0`
- [ ] After the abort drill + undo: `kubectl argo rollouts status podlab -n rollouts-lab` → `Healthy`, active still serves green

## Interview corner 💬

**"When would you pick blue-green over a rolling update, and what does it cost?"**
Rolling update gives me zero-downtime but no traffic control and no verification gate — users hit the new version as soon as a readiness probe passes. Blue-green gives me a full copy of the new version receiving *no* production traffic, a place to run real smoke tests, an instant switchover (a Service selector flip), and an instant rollback window while the old ReplicaSet is kept warm. The cost is 2× capacity during every release, which is why I'd use `previewReplicaCount` to shrink the preview, or canary instead when capacity is tight.

**"Blue-green vs canary — how do you choose?"**
Blue-green when the failure mode is *functional*: I want to verify the release end-to-end (smoke tests, QA, schema checks) before any user touches it, and an all-at-once flip is acceptable. Canary when the failure mode is *statistical*: the bug only shows under real traffic (latency, error rates, memory under load), so I shift a small percentage of real users and judge with metrics. Many teams do both: preview verification, then a canary ramp.

**"Your blue-green promotion flipped and users report errors. What happens next?"**
That's exactly what `scaleDownDelaySeconds` is for: the old ReplicaSet is still running, so `kubectl argo rollouts abort`/`undo` flips the selector back in seconds — no pods need to start. If I've passed the delay window, rollback means re-scaling the old version, which is back to rolling-update speed. So I size the delay to my time-to-detect, which is why the metrics work from Phase 5 matters.

## Stretch goals

- Set `previewReplicaCount: 1` and redo a release — note green runs at 1 pod until promotion, then scales to 3 *before* the flip.
- Add a `prePromotionAnalysis` block referencing tomorrow's AnalysisTemplate once Day 39 is done — blue-green with an automated gate.
- Move the Rollout into `~/Code/k8s-gitops` as a proper ArgoCD app and do a release purely via git commits; watch ArgoCD's health status while the Rollout is paused.
- In k9s, watch `:rs -n rollouts-lab` during a promote — narrate to yourself which ReplicaSet does what and when.

## Cleanup

Keep everything: **Argo Rollouts (controller + ArgoCD app) stays** — Day 39 builds directly on it and the capstone uses it. The `rollouts-lab` namespace and its Rollout also stay for tomorrow.

If RAM is tight, you can shrink the lab between days:

```sh
kubectl argo rollouts get rollout podlab -n rollouts-lab   # make sure it's Healthy first
kubectl patch rollout podlab -n rollouts-lab --type merge -p '{"spec":{"replicas":1}}'
```

(Scale it back to 3 at the start of Day 39.)
