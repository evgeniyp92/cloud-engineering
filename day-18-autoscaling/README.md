# Day 18 — Autoscaling

> **Time:** ~3 h · **Builds on:** Days 8, 15

## Objectives

- Explain the three scaling axes and where HPA, VPA, and cluster autoscaling each apply
- Predict HPA behavior from the formula `desired = ceil(current × currentMetric / targetMetric)`
- Drive podlab from 1 to 6 replicas with real CPU load, then tune scale-down with the `behavior` field
- Read HPA conditions to debug "why isn't it scaling"

## Concepts

### Three axes

When load grows you can scale on three independent axes:

| Axis | Mechanism | Changes |
|---|---|---|
| **Horizontal** | HPA (today's core) | replica count — more pods |
| **Vertical** | VPA | requests/limits — bigger pods |
| **Cluster** | Cluster Autoscaler / Karpenter | node count — more room |

They compose: HPA adds pods → pods don't fit → cluster autoscaler adds nodes. Horizontal is the workhorse because stateless replicas (Days 3–5: Deployments behind Services) make "more pods" trivially safe.

### How the HPA actually works

The HorizontalPodAutoscaler is a control loop in kube-controller-manager (reconcile loops — Day 16). Every ~15s it:

1. asks the **metrics API** for current usage of the pods its target selects — served by **metrics-server**, which you installed on Day 8 and reinstalled on Day 15 (no metrics-server → no HPA, full stop);
2. averages usage across pods and computes:

```
desiredReplicas = ceil( currentReplicas × currentMetric / targetMetric )
```

3. clamps to [minReplicas, maxReplicas], applies any `behavior` rules, and patches the target's `scale` subresource (an RBAC-visible subresource — Day 14 strikes again).

Run the numbers once by hand and HPA stops being magic. Target 50% CPU, 2 replicas currently averaging 90%:
`ceil(2 × 90/50) = ceil(3.6) = 4`. At 4 replicas the same total load averages ~45% → `ceil(4 × 45/50) = 4` → stable. The formula self-corrects toward the target; a tolerance band (~±10%) stops it twitching over noise.

**Percentage of what?** CPU "utilization" targets are a percentage **of the pod's requests** — not limits, not node capacity. A pod requesting `100m` and using `120m` is at 120%. Which is why **HPA percentage targets hard-require requests on every container**: no request, no denominator, and the HPA reports `FailedGetResourceMetric` and does nothing. Forgetting requests is the #1 broken-HPA cause in the wild; you set them deliberately today.

**Asymmetric by design.** Scale-up acts on the instantaneous picture (load is hurting users *now*). Scale-down uses a **stabilization window** (default 300s): the controller computes desired replicas continuously but only applies the *highest* recommendation from the window. Translation: replicas fall ~5 minutes after load stops, on purpose — flapping traffic would otherwise create flapping pods, and killing pods you'll need again in 20 seconds is worse than briefly over-provisioning. When the default doesn't fit, the `autoscaling/v2` **`behavior`** field tunes both directions: per-direction stabilization windows plus rate-limit policies ("at most N pods / N% per period").

`autoscaling/v2` also takes **multiple metrics** (HPA computes desired replicas per metric and takes the max), memory targets (use cautiously — memory rarely falls when load does, so scale-down stalls), and custom/external metrics via adapters (requests-per-second from Prometheus — the production favorite; you'll have Prometheus by Day 30). What HPA **cannot** do: scale to zero (min is 1 — an idle service still costs a pod; event-driven scale-to-zero is [KEDA](https://keda.sh)'s niche, which wraps HPA with event-source scalers) and it can't help pods that need to be *bigger* rather than *more numerous*.

### VPA, in concept

The Vertical Pod Autoscaler watches actual usage and adjusts requests/limits. Modes: `Off` (recommend only — the genuinely useful one: it's how you *learn* correct requests instead of guessing), `Initial` (apply at pod creation only), `Auto`/`Recreate` (apply by evicting and recreating pods — disruptive, since requests are immutable on running pods). **Never combine VPA-on-CPU with HPA-on-CPU**: VPA changes the denominator of HPA's percentage math and the two controllers chase each other. Installation is a stretch goal; on Day 47 you'll meet Goldilocks, a dashboard that runs VPA in recommend-mode fleet-wide.

### Cluster autoscaling, in concept (kind can't demo it)

**Cluster Autoscaler** is the classic: it watches for Pending pods that failed scheduling on resources, and resizes pre-defined node groups (ASGs etc.) up; it scales down by finding nodes whose pods all fit elsewhere, draining them. It thinks in fixed instance shapes per group. **Karpenter** (AWS-born, now CNCF) skips node groups: it looks at the Pending pods' aggregate requirements and provisions best-fit instances directly ("these pods need 7 CPU and a GPU → one g5.xlarge"), then actively consolidates — bin-packing pods onto fewer, cheaper nodes and deleting the rest. Faster, denser, better with spot capacity; the operational trade is a more dynamic, heterogeneous fleet. Either way the chain is the same: **HPA makes pods, autoscaler makes room** — requests (Day 8) are the currency of the entire conversation.

## Lab

### 1. A scalable target

Namespace + deployment + service. Requests are the load-bearing detail: `cpu: 100m` request, `200m` limit (Burstable QoS — Day 8), so one CPU-burning pod reads as 200% of request and the math has room to move.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: autoscale
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podlab
  namespace: autoscale
spec:
  replicas: 1
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
        resources:
          requests:
            cpu: 100m
            memory: 64Mi
          limits:
            cpu: 200m
            memory: 128Mi
---
apiVersion: v1
kind: Service
metadata:
  name: podlab
  namespace: autoscale
spec:
  selector:
    app: podlab
  ports:
  - port: 80
    targetPort: 8080
```

```sh
kubectl apply -f podlab-scalable.yaml
kubectl top pod -n autoscale     # idle baseline: ~1m CPU (also proves metrics-server works)
```

### 2. The HPA — core object

Write `hpa.yaml` yourself: `autoscaling/v2`, target the `podlab` Deployment, min 1, max 6, target **50% average CPU utilization**.

<details><summary>Solution</summary>

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: podlab
  namespace: autoscale
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: podlab
  minReplicas: 1
  maxReplicas: 6
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50
```

</details>

```sh
kubectl apply -f hpa.yaml
kubectl get hpa -n autoscale
# NAME     REFERENCE           TARGETS       MINPODS  MAXPODS  REPLICAS
# podlab   Deployment/podlab   cpu: 1%/50%   1        6        1
```

(`<unknown>/50%` for the first ~30s is normal — metrics-server hasn't scraped the pod yet. `<unknown>` *forever* means missing requests or broken metrics-server: see CKA corner.)

### 3. Light the fire

podlab's `/load?seconds=n` burns a full core in-process (capped by the 200m limit). Each curl blocks until the burn ends, so a sequential loop = one continuous burner. Run two loaders to make the math vivid:

```sh
kubectl run loader1 -n autoscale --image=curlimages/curl --restart=Never -- \
  sh -c 'while true; do curl -s "http://podlab/load?seconds=30" > /dev/null; done'
kubectl run loader2 -n autoscale --image=curlimages/curl --restart=Never -- \
  sh -c 'while true; do curl -s "http://podlab/load?seconds=30" > /dev/null; done'
```

Two terminals (or k9s in one — `:hpa` is a live view):

```sh
kubectl get hpa -n autoscale -w
kubectl get pods -n autoscale -w
```

Narrate what you see against the formula. One pod, two burners, throttled at its 200m limit → `cpu: 200%/50%` → `ceil(1 × 200/50) = 4` replicas almost immediately; the Service spreads the next `/load` calls across new pods, utilization stays above target, and within a couple of minutes you ride up to **6/6** (clamped by max — the formula may *want* more). Events tell the story too:

```sh
kubectl describe hpa podlab -n autoscale | tail -15
# SuccessfulRescale: New size: 4; reason: cpu resource utilization above target ...
```

### 4. Watch the slow ride down

```sh
kubectl delete pod loader1 loader2 -n autoscale
kubectl get hpa -n autoscale -w
```

TARGETS falls to ~1% within a minute — but REPLICAS stays 6 for ~5 minutes, then steps down (possibly via an intermediate step, e.g. 6→2→1, with `ScaleDownStabilized` events in describe). This is the 300s stabilization window doing its job: the HPA remembers the *peak* recommendation of the last 5 minutes and refuses to act on a dip that might be a blip. Time it. Feel the slowness. It's a feature.

### 5. Tune it with `behavior`

For a lab (or a dev environment) 5 minutes is tedious. Add to the HPA spec and re-apply:

```yaml
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 30
      policies:
      - type: Pods
        value: 2
        periodSeconds: 30
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
      - type: Percent
        value: 100
        periodSeconds: 15
```

Reading: scale-down waits only 30s, then sheds at most 2 pods per 30s (a controlled glide, not a cliff — 6→4→2→1 at 30s steps); scale-up acts instantly, at most doubling per 15s. Re-run the loaders, ride to 6, kill them, and watch the descent take ~90 seconds instead of 5+ minutes:

```sh
kubectl run loader1 -n autoscale --image=curlimages/curl --restart=Never -- \
  sh -c 'while true; do curl -s "http://podlab/load?seconds=30" > /dev/null; done'
# wait for 6 replicas, then:
kubectl delete pod loader1 -n autoscale
kubectl get hpa -n autoscale -w   # 6 → 4 → 2 → 1, ~30s apart
```

Production defaults are usually fine; reach for `behavior` when product traffic is spiky (longer windows) or batch-like (faster down). A multi-metric variant is one more list item under `metrics:` — e.g. memory `averageUtilization: 80` — and the HPA scales on whichever demands more replicas. (Remember the memory caveat from Concepts before copying that into production.)

## Verify ✅

- [ ] `kubectl get hpa podlab -n autoscale` shows a numeric target like `cpu: 1%/50%` (not `<unknown>`) within a minute of creation
- [ ] Under load: `kubectl get hpa -n autoscale -w` walks REPLICAS up to `6`, and `kubectl describe hpa podlab -n autoscale` shows `SuccessfulRescale` events with the reason `cpu resource utilization (percentage of request) above target`
- [ ] `kubectl top pods -n autoscale` during load shows podlab pods pinned near `200m` (the limit)
- [ ] After stopping load with default behavior: replicas hold for ~5 min before dropping (you timed it)
- [ ] With the tuned `behavior`: descent from 6 begins within ~30s and steps down 2 at a time
- [ ] `kubectl get hpa podlab -n autoscale -o jsonpath='{.status.conditions[?(@.type=="AbleToScale")].status}'` → `True`

## CKA corner 🎓

HPA is a one-liner on the exam — know it cold:

```sh
kubectl autoscale deployment NAME --min=2 --max=8 --cpu-percent=70 -n NS
```

(`kubectl autoscale` also works on statefulsets/replicasets. It generates the v2 object on modern clusters; check with `kubectl get hpa NAME -o yaml`.) The other exam skill is *reading* a broken HPA — that lives in `kubectl describe hpa`, in the **Conditions**:

| Condition | Healthy | When it's not |
|---|---|---|
| `AbleToScale` | `True` | controller can't update the scale subresource, or a rate limit/stabilization is holding |
| `ScalingActive` | `True` | `False` + `FailedGetResourceMetric` = metrics missing → no metrics-server, or **no requests on the pods** |
| `ScalingLimited` | `False` | `True` = formula wants more/fewer than min/max allows — often the sign max is too low |

**Drill 1 (2 min).** Imperatively autoscale deployment `podlab` in `autoscale` between 1 and 4 replicas at 80% CPU, under the name it'll get by default. Then delete it and restore your v2 YAML.

<details><summary>Solution</summary>

```sh
kubectl delete hpa podlab -n autoscale          # can't have two HPAs fighting over one target
kubectl autoscale deployment podlab --min=1 --max=4 --cpu-percent=80 -n autoscale
kubectl get hpa -n autoscale                    # name defaults to the workload's: podlab
kubectl delete hpa podlab -n autoscale && kubectl apply -f hpa.yaml
```
</details>

**Drill 2 (5 min).** An HPA shows `TARGETS: <unknown>/50%` and never scales. Name the two most likely causes and the one command that distinguishes them.

<details><summary>Solution</summary>

```sh
kubectl describe hpa NAME -n NS    # read ScalingActive's reason
```
`FailedGetResourceMetric ... missing request for cpu` → a container in the target pods lacks `resources.requests.cpu` (every container needs it). `unable to get metrics ... metrics API not available` → metrics-server is missing/broken (`kubectl top pods` failing confirms; check `kubectl get pods -n kube-system | grep metrics-server`). Same symptom, opposite fixes — always read the condition's reason.
</details>

**Drill 3 (3 min).** Current state: 3 replicas averaging 30% CPU; target 60%; min 2, max 10. What does the controller do? Same question at 3 replicas averaging 240%.

<details><summary>Solution</summary>

`ceil(3 × 30/60) = ceil(1.5) = 2` → scale to 2 (min allows it) — after the scale-down stabilization window. `ceil(3 × 240/60) = 12` → clamped to 10, immediately; `ScalingLimited: True`.
</details>

## Stretch goals

- **Load through the front door:** add an Ingress `podlab.localhost` and drive load from your Mac — `for i in $(seq 1 4); do curl -s "http://podlab.localhost:8080/load?seconds=60" & done` — to watch host-originated traffic do the same thing through ingress-nginx.
- **VPA in recommend mode:** install VPA (`git clone https://github.com/kubernetes/autoscaler.git && cd autoscaler/vertical-pod-autoscaler && ./hack/vpa-up.sh`), create a VPA with `updateMode: "Off"` targeting podlab, run load, and read `kubectl describe vpa` recommendations — then compare with the requests you guessed. Preview of Day 47's Goldilocks.
- **Starve the cluster:** raise the HPA max to 30 and requests to `cpu: 500m`, load it, and watch pods go Pending once the workers fill — the exact signal a cluster autoscaler would act on. You can't add kind nodes on the fly, which is precisely the lesson; read the Pending events (Day 13) and scale back down.

## Cleanup

```sh
kubectl delete namespace autoscale
```

Nothing from today persists. Still on the cluster and **staying**: the `guestbook` namespace (with its NetworkPolicies), Cilium, ingress-nginx, metrics-server. Tomorrow Helm enters the picture and the YAML-by-hand era starts to close.
