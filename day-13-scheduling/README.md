# Day 13 — Scheduling: Telling the Scheduler Where Pods Go

> **Time:** ~3 h · **Builds on:** Days 2, 9, 12

## Objectives

- Explain the scheduler's filter → score pipeline and read its decisions from events
- Pin pods to nodes with nodeSelector and node affinity (required vs preferred)
- Reserve nodes with taints and grant access with tolerations, including NoExecute eviction
- Spread replicas across nodes with podAntiAffinity and topologySpreadConstraints

## Concepts

### How the scheduler thinks

Every pod without a `nodeName` lands in a queue. For each one, kube-scheduler runs two phases:

```
all nodes ──▶ FILTER ──▶ feasible nodes ──▶ SCORE ──▶ winner ──▶ bind
              "can it run here at all?"      "where is it best?"
              - enough free requests?        - least loaded
              - tolerates the taints?        - spread across zones
              - matches required affinity?   - image already present
              - volume reachable?            - preferred affinity
```

If **zero nodes survive filtering, the pod stays `Pending`** and the scheduler writes an event explaining exactly which filters killed which nodes. That event text is the single most useful diagnostic in scheduling — today you'll deliberately produce one and read it.

Everything below is just a way to influence one of the two phases.

### The mechanisms, weakest to strongest

| Mechanism | Phase | Direction | Typical use |
|---|---|---|---|
| `nodeName` | bypasses scheduler | pod → node | almost never (static pods use it implicitly) |
| `nodeSelector` | filter | pod *chooses* nodes | "needs SSD", simple and blunt |
| node affinity | filter (+score) | pod chooses nodes | same, but with operators and "preferred" |
| taints + tolerations | filter | node *repels* pods | "nobody runs here unless invited" |
| pod (anti-)affinity | filter (+score) | pod chooses *relative to other pods* | spread replicas, co-locate with a cache |
| topologySpreadConstraints | filter (+score) | even distribution | the modern spread tool |

**`nodeName`** skips scheduling entirely — the kubelet on that node just gets the pod. No filters run, which means it can land on a node without resources and get stuck. Know it exists; don't use it.

**`nodeSelector`** is an exact label match: pod runs only on nodes with all listed labels. **Node affinity** is its grown-up sibling: operators (`In`, `NotIn`, `Exists`, `Gt`, `Lt`), OR-able term lists, and crucially two strengths — `requiredDuringSchedulingIgnoredDuringExecution` (a filter: no match, no schedule) and `preferredDuringSchedulingIgnoredDuringExecution` (a score: try, but schedule anyway if impossible). The `IgnoredDuringExecution` suffix means already-running pods aren't evicted when labels change later.

**Taints invert the relationship.** Labels+affinity let *pods* opt in to nodes; taints let *nodes* push pods away unless the pod carries a matching toleration. Three effects: `NoSchedule` (filter), `PreferNoSchedule` (soft), `NoExecute` (filter **and evict already-running pods** — this is how nodes drain pods when they go unhealthy; the `node.kubernetes.io/not-ready` toleration with `tolerationSeconds: 300` you see on every pod is exactly this machinery). Your control plane stays clean because kubeadm taints it — you saw the effect on Day 12. Important asymmetry: a toleration *allows* a pod on a tainted node; it doesn't *attract* it. Dedicated-node setups need taint (keep others out) **plus** nodeSelector/affinity (keep your pods in).

**Pod affinity/anti-affinity** schedules relative to *other pods*: "run me where pods matching X are (not) running", with "where" defined by a `topologyKey` — a node label whose value defines the bucket (per-node: `kubernetes.io/hostname`; per-zone in clouds: `topology.kubernetes.io/zone`). Anti-affinity on hostname = classic HA spread. Its failure mode: with `required`, more replicas than buckets means eternal `Pending`.

**`topologySpreadConstraints`** is the modern, more expressive spread: instead of binary "never two together" you say `maxSkew: 1` — the difference between the fullest and emptiest bucket may be at most 1 — and choose `whenUnsatisfiable: DoNotSchedule` (hard) or `ScheduleAnyway` (soft). It scales to "spread 50 replicas over 3 zones" where anti-affinity can't.

### What kind gives you to play with

```sh
kubectl get nodes --show-labels
```

Every node has `kubernetes.io/hostname` (= node name), arch/OS labels, and the control-plane additionally has `node-role.kubernetes.io/control-plane` (plus our `ingress-ready=true` from the Day 1 config). Nothing distinguishes the two workers — you'll add labels yourself, which is exactly how real clusters mark hardware (`disktype=ssd`, `gpu=a100`, …).

## Lab

Work in a scratch namespace; you'll label/taint real nodes, and **cleanup at the end matters**.

```sh
kubectl create namespace sched
```

### 1. nodeSelector: pin podlab to "the SSD node"

```sh
kubectl label node course-worker disktype=ssd
```

Boilerplate deployment, `podlab.yaml` — you'll mutate this one file all day:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podlab
  namespace: sched
spec:
  replicas: 2
  selector:
    matchLabels:
      app: podlab
  template:
    metadata:
      labels:
        app: podlab
    spec:
      nodeSelector:
        disktype: ssd
      containers:
      - name: podlab
        image: podlab:v1
        ports:
        - containerPort: 8080
```

```sh
kubectl apply -f podlab.yaml
kubectl get pods -n sched -o wide
```

Both replicas on `course-worker`. Now break it: change the selector to `disktype: nvme`, apply, and read why the new pods can't go anywhere:

```sh
kubectl get pods -n sched   # new pods Pending (old ones keep running — rolling update waits)
kubectl describe pod -n sched -l app=podlab | grep -A4 Events: | tail -5
```

> `0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) didn't match Pod's node affinity/selector.`

Read it like the scheduler: 3 candidates, every one eliminated, each with its reason. Revert to `disktype: ssd` and apply.

### 2. Taints: reserve worker2 for batch work

```sh
kubectl taint node course-worker2 dedicated=batch:NoSchedule
kubectl delete -f podlab.yaml
```

Remove the `nodeSelector` from `podlab.yaml`, set `replicas: 4`, apply. All four pods land on `course-worker` — worker2 repels them, control-plane repels them, only one node is left. Now invite podlab in — add to the pod spec:

```yaml
      tolerations:
      - key: dedicated
        operator: Equal
        value: batch
        effect: NoSchedule
```

Apply and watch placement:

```sh
kubectl get pods -n sched -o wide -w
```

New pods now use *both* workers (toleration permits, the spread score prefers the empty node). Next, **NoExecute eviction**: upgrade the taint and watch running pods without the toleration get thrown off. First remove the toleration from podlab.yaml and apply (pods all drift to `course-worker` again? not necessarily — NoSchedule doesn't evict! Pods already on worker2 *stay*; only new scheduling is blocked. Verify that with `kubectl get pods -n sched -o wide`). Then:

```sh
kubectl taint node course-worker2 dedicated=batch:NoSchedule-   # remove old taint
kubectl taint node course-worker2 dedicated=batch:NoExecute
kubectl get pods -n sched -o wide -w
```

Every podlab pod on worker2 is **evicted immediately** and rescheduled onto `course-worker` — that's the difference: NoSchedule gates admission, NoExecute gates *presence*. Remove the taint before moving on:

```sh
kubectl taint node course-worker2 dedicated=batch:NoExecute-
```

### 3. Node affinity: required vs preferred

Replace `nodeSelector` with affinity in `podlab.yaml` (replicas back to 2). Requirements: **require** `disktype In [ssd, nvme]`, and **prefer** (weight 50) `disktype=nvme`.

<details><summary>Solution</summary>

```yaml
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: disktype
                operator: In
                values: ["ssd", "nvme"]
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 50
            preference:
              matchExpressions:
              - key: disktype
                operator: In
                values: ["nvme"]
```

</details>

Apply: both pods on `course-worker` (the only node passing the *required* term; the nvme *preference* finds no taker and is ignored — that's the point of preferred). Label `course-worker2 disktype=nvme`, delete the pods, and watch them prefer worker2.

```sh
kubectl label node course-worker2 disktype=nvme
kubectl delete pods -n sched -l app=podlab
kubectl get pods -n sched -o wide
```

### 4. Anti-affinity: spread, then overflow

The HA pattern: never two replicas on one node. Replace the whole `affinity` block:

```yaml
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
          - labelSelector:
              matchLabels:
                app: podlab
            topologyKey: kubernetes.io/hostname
```

Apply with `replicas: 2` → one pod per worker. Now:

```sh
kubectl scale deployment podlab -n sched --replicas=3
kubectl get pods -n sched
kubectl describe pod -n sched $(kubectl get pod -n sched --field-selector=status.phase=Pending -o name | head -1 | cut -d/ -f2) | tail -4
```

The third pod is `Pending` forever:

> `0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) didn't match pod anti-affinity rules.`

Only two schedulable buckets exist and both already hold a podlab. This *will* happen to you in production the day a node is drained. The fix when spread is a preference, not a law — switch to soft anti-affinity:

```yaml
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchLabels:
                  app: podlab
              topologyKey: kubernetes.io/hostname
```

Apply — the rollout replaces all pods, the third one doubles up on a worker, everything `Running`.

### 5. topologySpreadConstraints: the modern way

Delete the `affinity` block; add to the pod spec instead:

```yaml
      topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app: podlab
```

Apply with replicas 4: you get 2+2 across the workers — anti-affinity could never express "at most one apart". Scale to 5 → 3+2 still satisfies `maxSkew: 1`. Flip `whenUnsatisfiable: ScheduleAnyway` to make it advisory. In k9s, the `:pods` view with the NODE column (or `o` to sort wide) makes each step's placement obvious at a glance.

## Verify ✅

- [ ] Step 1: `kubectl get pods -n sched -o wide` → both pods on `course-worker`; after the `nvme` typo, a Pending pod whose describe-event names **both** filter reasons (taint + selector)
- [ ] Step 2: with the NoSchedule taint and no toleration, `kubectl get pods -n sched -o wide` shows zero pods on `course-worker2`; with the toleration, both workers used
- [ ] Step 2: switching to `NoExecute` visibly evicts worker2's pods (watch `-w` output) and `kubectl describe node course-worker2 | grep Taints` confirms the taint before you remove it
- [ ] Step 4: at replicas=3 with required anti-affinity, exactly one pod `Pending` with `didn't match pod anti-affinity rules` in its events; with preferred, 3/3 Running
- [ ] Step 5: replicas=4 → `kubectl get pods -n sched -o wide` shows a 2/2 split across workers
- [ ] Cleanup done: `kubectl describe node course-worker2 | grep Taints` → `<none>`, and neither worker has a `disktype` label (`kubectl get nodes -L disktype`)

## CKA corner 🎓

Taints/tolerations and affinity appear on nearly every exam. Burn in the imperative taint syntax — including the trailing-dash removal:

```sh
kubectl taint node N key=value:NoSchedule     # add
kubectl taint node N key=value:NoSchedule-    # remove (the dash!)
kubectl taint node N key:NoExecute-           # remove, value-agnostic
```

And remember the question pattern: "pods aren't scheduling on node X" → check `kubectl describe node X` Taints section *first*.

**Drill 1 (4 min).** Taint node `course-worker2` with `env=prod:NoSchedule`. Then create a pod `prod-pod` (image nginx) that tolerates it AND is guaranteed to run on `course-worker2`.

<details><summary>Solution</summary>

```sh
kubectl taint node course-worker2 env=prod:NoSchedule
kubectl run prod-pod --image=nginx --dry-run=client -o yaml > p.yaml
```
Edit `p.yaml`, add under `spec:`:
```yaml
  tolerations:
  - key: env
    operator: Equal
    value: prod
    effect: NoSchedule
  nodeSelector:
    kubernetes.io/hostname: course-worker2
```
`kubectl apply -f p.yaml` — toleration alone isn't enough to *target* the node; you need the selector too. Clean up: delete pod, `kubectl taint node course-worker2 env=prod:NoSchedule-`.
</details>

**Drill 2 (4 min).** A pod must run only on nodes labeled `size=large` **or** `size=xlarge`, using node affinity (not nodeSelector). Write the affinity block from memory.

<details><summary>Solution</summary>

```yaml
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: size
            operator: In
            values: ["large", "xlarge"]
```
The mouthful field names are free points if you've typed them enough times. `kubectl explain pod.spec.affinity.nodeAffinity --recursive` is your in-exam crib sheet.
</details>

**Drill 3 (3 min).** A pod is `Pending`. List, in order, the three commands you run and the three most likely scheduling causes.

<details><summary>Solution</summary>

```sh
kubectl describe pod P | tail            # 1. the event says it all
kubectl describe nodes | grep -A6 Taints # 2. if taints implicated
kubectl top nodes                        # 3. if "Insufficient cpu/memory"
```
Causes: (a) insufficient resources for the requests, (b) untolerated taints, (c) unsatisfiable selector/affinity (including anti-affinity with too few topology buckets). The describe-event names which.
</details>

## Stretch goals

- Co-location: deploy a `redis` pod, then give podlab `podAffinity` (required, hostname) toward `app: redis` — all replicas pile onto redis's node. The cache-locality pattern, and a reminder that affinity concentrates risk.
- Set `nodeName: course-worker2` directly on a pod with absurd resource requests (e.g. 100 CPU) — it schedules anyway (no scheduler involved) and the *kubelet* rejects it: `OutOfcpu`. Now you've seen why bypassing the scheduler is a footgun.
- Look at how a DaemonSet (Day 12's, or kube-proxy) achieves per-node placement in its pod spec: a generated `nodeAffinity` on `metadata.name` per pod, not nodeName. `kubectl get pod -n kube-system <kube-proxy-pod> -o yaml | grep -A8 affinity`.

## Cleanup

The namespace goes; the **node changes must be reverted** or later days will schedule strangely:

```sh
kubectl delete namespace sched
kubectl label node course-worker disktype-
kubectl label node course-worker2 disktype-
kubectl taint node course-worker2 dedicated=batch:NoSchedule- 2>/dev/null
kubectl taint node course-worker2 dedicated=batch:NoExecute- 2>/dev/null
kubectl describe nodes | grep -E "^Name:|Taints:"   # only control-plane keeps its taint
```

The `guestbook` namespace remains untouched, as always.
