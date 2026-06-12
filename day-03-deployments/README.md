# Day 03 — Deployments

> **Time:** ~3 h · **Builds on:** Days 1, 2

## Objectives

- Explain the Deployment → ReplicaSet → Pod ownership chain and read it from `kubectl get rs` output.
- Perform a rolling update and *watch* it: a curl loop shows old and new versions answering simultaneously, then only new.
- Tune `maxSurge`/`maxUnavailable`, record change causes, and roll back to a specific revision.
- Choose between `RollingUpdate` and `Recreate`, and use scaling and paused rollouts deliberately.

## Concepts

### Why pods aren't enough

Yesterday's pods had a fatal flaw: delete one (or drain its node) and it's gone forever. Nothing recreates it. Kubernetes' answer is the **controller pattern**: a control loop that endlessly compares *desired* state ("3 replicas of podlab:v1") with *observed* state and acts on the difference. You declare; the controller reconciles. This loop is the single most important idea in Kubernetes — Days 27+ (GitOps) are the same loop applied to whole clusters.

### Three layers, two controllers

```
Deployment  "podlab"                ← you edit THIS, and only this
 └── ReplicaSet "podlab-7d4b9c..."  ← one per pod-template version
      └── Pods "podlab-7d4b9c...-xxxxx"
```

A **ReplicaSet** does one dumb thing well: keep N copies of a pod template alive. A **Deployment** manages ReplicaSets *over time*: every change to the pod template creates a *new* ReplicaSet, and the deployment controller shifts replicas from old RS to new RS step by step. That's all a rolling update is — two ReplicaSets with a sliding replica count:

```
time →
old RS:  3 ─ 3 ─ 2 ─ 1 ─ 0
new RS:  0 ─ 1 ─ 2 ─ 3 ─ 3
```

Old ReplicaSets are kept at 0 replicas (up to `revisionHistoryLimit`, default 10) — they *are* the rollback mechanism: `rollout undo` just scales an old RS back up. You never create ReplicaSets by hand; you also never edit pods owned by one (the RS will fight you and win — try it in the stretch goals).

Ownership is literal: every pod carries an `ownerReferences` entry pointing at its RS, and the RS at its Deployment. This is also why deleting a Deployment cascades down.

### Rollout mechanics: maxSurge and maxUnavailable

`RollingUpdate` strategy has two knobs, absolute numbers or percentages of `replicas`:

| Knob | Meaning | Default |
|---|---|---|
| `maxSurge` | how many pods *above* desired count may exist during rollout | 25% |
| `maxUnavailable` | how many desired pods may be *missing* during rollout | 25% |

`maxSurge: 1, maxUnavailable: 0` = "always full capacity, add one new before killing one old" — the zero-downtime setting (needs headroom for +1 pod). `maxUnavailable: 1, maxSurge: 0` = "never exceed capacity" — for when nodes are full or licenses are per-instance. **Recreate** kills *all* old pods before starting any new — guaranteed downtime, but correct when two versions must never overlap (singleton consumers, schema-incompatible app versions, RWO volumes that only one pod may mount).

### Revisions and change-cause

Each template change bumps the revision counter. `kubectl rollout history` lists revisions, but `CHANGE-CAUSE` is empty unless you fill it. The old `--record` flag is deprecated; the current practice is annotating yourself:

```sh
kubectl annotate deployment podlab kubernetes.io/change-cause="VERSION 2.0.0" --overwrite
```

Do it on every change today. In real life Git history plays this role (Day 27), but on the CKA and in ad-hoc ops, change-cause is what saves you.

### Readiness gates the rollout

The controller only continues shifting replicas when new pods report **Ready**. Today podlab has no readiness probe, so "Ready" merely means "container started" — the rollout can outrun actual usability. Day 10 closes that hole; today just notice where readiness plugs into the machine.

## Lab

### 1. Write the Deployment

Requirements — `deploy.yaml`, by hand (you know pod templates from Day 2; a Deployment wraps one):

- Deployment `podlab`, 3 replicas
- selector matches label `app: podlab` (selector and template labels MUST match — apply rejects it otherwise)
- pod template: container `podlab`, image `podlab:v1`, port 8080, env `VERSION=1.0.0`, plus the three Downward API envs from Day 2

<details><summary>Solution</summary>

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podlab
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
            - name: POD_IP
              valueFrom: { fieldRef: { fieldPath: status.podIP } }
            - name: NODE_NAME
              valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
            - name: POD_NAMESPACE
              valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
```

</details>

```sh
kubectl apply -f deploy.yaml
kubectl annotate deployment podlab kubernetes.io/change-cause="initial deploy, VERSION 1.0.0"
kubectl get deploy,rs,pods -l app=podlab
```

Read the three layers in that output: pod names embed the RS hash, the RS name embeds the deployment name. Confirm ownership:

```sh
kubectl get pods -l app=podlab -o jsonpath='{.items[0].metadata.ownerReferences[0].kind}{" "}{.items[0].metadata.ownerReferences[0].name}{"\n"}'
```

Kill a pod and watch the RS replace it within seconds — this is the entire reason Deployments exist:

```sh
kubectl delete pod $(kubectl get pods -l app=podlab -o jsonpath='{.items[0].metadata.name}')
kubectl get pods -l app=podlab     # a brand-new name appears, AGE seconds
```

### 2. A service + a watching loop

The rollout demo needs a stable endpoint that load-balances across replicas. Day 4 explains Services properly; today, boilerplate:

```sh
kubectl expose deployment podlab --port=80 --target-port=8080
```

Why not watch through `kubectl port-forward svc/podlab`? Because port-forward pins **one** backing pod at connect time — you'd see one version, then a dead tunnel when that pod is replaced. To see load-balancing for real, curl from *inside* the cluster. Terminal 2:

```sh
kubectl run watcher --rm -it --image=curlimages/curl --restart=Never -- \
  sh -c 'while true; do curl -s --max-time 2 podlab/ | sed -E "s/.*\"version\":\"([^\"]*)\".*/\1/"; sleep 0.3; done'
```

A steady column of `1.0.0`, served round-robin-ish by all 3 pods. Leave it running. Terminal 3, the log view:

```sh
stern podlab --include version    # all replicas, color-coded — Day 1's promise paying off
```

### 3. Rolling update — watch both versions live

Terminal 1: set the rollout knobs explicitly first. Edit `deploy.yaml`, adding under `spec:`:

```yaml
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
```

Also slow pod startup slightly so the rollout is humanly visible — add `minReadySeconds: 5` under `spec:`. Apply, then change the version:

```sh
kubectl apply -f deploy.yaml
kubectl set env deployment/podlab VERSION=2.0.0
kubectl annotate deployment podlab kubernetes.io/change-cause="VERSION 2.0.0" --overwrite
kubectl rollout status deployment/podlab
```

Now look at terminal 2: the column flips `1.0.0 / 2.0.0 / 1.0.0 / 2.0.0 …` — **both versions serving at once** — then settles to pure `2.0.0`. That interleaving is the heart of today; if you missed it, roll back and forth again. Meanwhile:

```sh
kubectl get rs -l app=podlab    # old RS scaled to 0, new RS at 3 — both still exist
```

### 4. History and rollback

```sh
kubectl rollout history deployment/podlab
kubectl rollout history deployment/podlab --revision=2   # full template of a revision
```

Ship a "bad" release and undo it:

```sh
kubectl set env deployment/podlab VERSION=2.1.0-broken
kubectl annotate deployment podlab kubernetes.io/change-cause="VERSION 2.1.0-broken" --overwrite
kubectl rollout undo deployment/podlab                    # back to previous (2.0.0)
kubectl rollout undo deployment/podlab --to-revision=1    # explicitly back to 1.0.0
kubectl rollout history deployment/podlab
```

Note in the history: rolling back doesn't go "backwards" — revision 1's template is re-released as a **new** highest revision, and the watcher column flips accordingly. Roll forward to `2.0.0` again before continuing (`kubectl set env deployment/podlab VERSION=2.0.0`).

### 5. Scaling and pausing

```sh
kubectl scale deployment podlab --replicas=5
kubectl get pods -l app=podlab -o wide     # spread across both workers
kubectl scale deployment podlab --replicas=3
```

Scaling changes the *current* RS's replica count — no new revision, check `rollout history`. Pausing lets you batch several template edits into one rollout:

```sh
kubectl rollout pause deployment/podlab
kubectl set env deployment/podlab COLOR=blue
kubectl set env deployment/podlab VERSION=2.2.0
kubectl get rs -l app=podlab               # nothing happened — paused
kubectl rollout resume deployment/podlab   # ONE rollout for both changes
kubectl rollout status deployment/podlab
```

### 6. Recreate strategy

In `deploy.yaml`, replace the whole `strategy:` block with `strategy: {type: Recreate}` (and delete the `rollingUpdate` knobs — they're invalid with Recreate). Apply, then change `VERSION` to `3.0.0` and watch terminal 2: the curl loop **errors/hangs for a few seconds** — all pods die before any replacement starts. That gap is the cost; version-overlap safety is the payoff. Restore `RollingUpdate` and `VERSION=2.0.0` afterwards, and stop the watcher (Ctrl-C) and stern.

## Verify ✅

- [ ] `kubectl get deploy podlab` → `READY 3/3`
- [ ] `kubectl get rs -l app=podlab` → ≥2 ReplicaSets; exactly one with `DESIRED 3`, the rest `0`
- [ ] During step 3 the watcher printed interleaved `1.0.0` and `2.0.0` lines, then only `2.0.0`
- [ ] `kubectl rollout history deployment/podlab` → ≥4 revisions, each with a non-empty `CHANGE-CAUSE`
- [ ] `kubectl get pods -l app=podlab -o jsonpath='{.items[*].metadata.ownerReferences[0].kind}'` → `ReplicaSet ReplicaSet ReplicaSet`
- [ ] During step 6 (Recreate) the watcher showed a visible gap with no responses

## CKA corner 🎓

Exam notes:

- `kubectl create deployment x --image=img --replicas=3 --dry-run=client -o yaml` then edit — never write deployments from scratch in the exam.
- Know the imperative quartet: `kubectl set image deploy/x ctr=img:v2`, `kubectl scale`, `kubectl rollout undo`, `kubectl rollout status`. They solve most deployment questions in one line.
- `kubectl set image` needs the **container name** — get it with `kubectl get deploy x -o jsonpath='{.spec.template.spec.containers[*].name}'`.

**Drill 1 (3 min):** Create deployment `web`, image `nginx:1.25`, 4 replicas. Update to `nginx:1.27`, confirm rollout completed, then roll back and prove the image is `nginx:1.25` again — imperative commands only.

<details><summary>Solution</summary>

```sh
kubectl create deployment web --image=nginx:1.25 --replicas=4
kubectl set image deployment/web nginx=nginx:1.27
kubectl rollout status deployment/web
kubectl rollout undo deployment/web
kubectl get deployment web -o jsonpath='{.spec.template.spec.containers[0].image}'   # nginx:1.25
kubectl delete deployment web
```

</details>

**Drill 2 (4 min):** Deployment `careful` (nginx, 5 replicas) must never drop below 5 ready pods and never exceed 7 total during updates. Set the strategy, prove it by updating the image while running `kubectl get pods --watch` in another terminal.

<details><summary>Solution</summary>

`maxUnavailable: 0` (never below 5), `maxSurge: 2` (never above 7):

```sh
kubectl create deployment careful --image=nginx:1.25 --replicas=5
kubectl patch deployment careful -p '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":2,"maxUnavailable":0}}}}'
kubectl set image deployment/careful nginx=nginx:1.27
# watch shows pod count peaking at 7, Running+Ready never below 5
kubectl delete deployment careful
```

</details>

## Stretch goals

- Fight the controller: `kubectl edit` a *pod*'s label `app: podlab` to `app: rogue`. The RS instantly creates a replacement (the edited pod fell out of the selector) — you now have an orphaned pod. Delete it.
- Set `revisionHistoryLimit: 2`, make 4 changes, and confirm old ReplicaSets get garbage-collected.
- `kubectl rollout restart deployment/podlab` — rollout with zero template change (it stamps a restart annotation). Why is this better than deleting pods? (Hint: it respects maxUnavailable.)
- In k9s: `:deploy`, select podlab, watch a rollout live; `:rs` shows the replica handoff.

## Cleanup

```sh
kubectl delete pod watcher --ignore-not-found     # if the --rm didn't reap it
```

**Keep:** the `podlab` Deployment (3 replicas, `VERSION=2.0.0`, RollingUpdate) **and** the `podlab` Service — Day 4 starts from exactly this state.
