# Day 09 — Namespaces & Labels

> **Time:** ~3 h · **Builds on:** Days 4, 8

## Objectives

- Use namespaces as scoping + multi-tenancy boundaries, and know what *isn't* namespaced.
- Wield labels and selectors (equality and set-based) fluently, and know when an annotation is the right tool instead.
- Adopt the `app.kubernetes.io/*` recommended labels for podlab and keep them for the rest of the course.
- Enforce a namespace budget with ResourceQuota and default sizing with LimitRange — and read their error messages.

## Concepts

### Namespaces: virtual clusters, sort of

A namespace partitions one physical cluster into named scopes. Within a namespace, resource names must be unique; across namespaces they're independent — every team can have a Deployment called `api`. Four things actually attach to the namespace boundary:

1. **Names & DNS** — `svc.ns.svc.cluster.local`; the short-name convenience stops at the boundary.
2. **RBAC** (Day 13) — "team-a can do anything *in namespace team-a*" is the standard tenancy grant.
3. **ResourceQuota / LimitRange** — budgets and defaults are namespace-scoped objects.
4. **Policy attachment points** — NetworkPolicies (Day 15), Pod Security admission labels.

And one thing that pointedly does *not*: **the network**. Pods in different namespaces reach each other freely by default — namespaces are an organizational boundary, not a firewall. People assume otherwise constantly; Day 15 fixes it with NetworkPolicies.

Not everything lives in a namespace. Nodes, PersistentVolumes, StorageClasses, ClusterRoles, the namespaces themselves — anything that's physically or logically cluster-wide. The authoritative list is one command away (`kubectl api-resources --namespaced=false`), and "is X namespaced?" is a real interview/CKA question.

Four namespaces exist from birth: `default` (where you've been squatting all week), `kube-system` (control-plane components — look, don't touch), `kube-public` (readable by everyone, rarely used), `kube-node-lease` (node heartbeats).

### Labels vs annotations

Both are string key/value metadata; the difference is *who reads them*:

| | Labels | Annotations |
|---|---|---|
| Read by | **selectors** — machinery picks objects by them | humans and tools, by exact key |
| Size/charset | strict, ≤63-char values | up to 256 KiB total, anything |
| Examples | `app=podlab`, `env=prod`, `tier=backend` | `kubernetes.io/change-cause`, ingress-nginx `rewrite-target`, a deploy timestamp |

The test: *will anything ever need to SELECT objects by this?* Yes → label. No → annotation. You've been living this split all week without naming it: Services/Deployments select pods by **label**; Day 3's change-cause and Day 5's rewrite config rode in **annotations**.

Selectors come in two grammars. Equality: `env=prod`, `env!=prod`. Set-based: `env in (dev,staging)`, `tier notin (cache)`, `release` (key exists), `!release` (key absent). Deployments' `matchLabels`/`matchExpressions` are the same grammar in YAML. Separately, **field selectors** (`--field-selector status.phase=Running`) filter by a small set of object *fields* — not labels, not general-purpose, but invaluable for things like "all pods on node X".

### Recommended labels

The `app.kubernetes.io/*` label set is the ecosystem's shared vocabulary — Helm writes it, dashboards group by it, cost tools bill by it:

| Key | Example |
|---|---|
| `app.kubernetes.io/name` | `podlab` |
| `app.kubernetes.io/instance` | `podlab-main` (this deployment of it) |
| `app.kubernetes.io/version` | `2.0.0` |
| `app.kubernetes.io/component` | `api` |
| `app.kubernetes.io/part-of` | `course` |
| `app.kubernetes.io/managed-by` | `kubectl` (later: `Helm`, `argocd`) |

Adopt them today; every later phase (Helm Day 24, ArgoCD Day 27, Prometheus Day 31) assumes this shape.

### Quota and defaults: the tenancy contract

**ResourceQuota** caps a namespace's totals — object counts (`pods: 10`) and resource sums (`requests.cpu`, `limits.memory`). Two behaviors to internalize: violations are rejected **at creation time** by the API server (HTTP 403 Forbidden, with arithmetic in the message), and — the gotcha — once a compute quota exists, **every pod must declare** the quoted resources or it's rejected outright. That's what **LimitRange** is for: it injects per-container default requests/limits (and can clamp min/max) so ordinary pods keep working under quota. Quota = the budget; LimitRange = the default line items. Real platform teams stamp both into every tenant namespace at creation.

### Organizing real clusters

Two dominant layouts: **namespace-per-env** (`myapp-dev`, `myapp-prod` in one cluster — cheap, easy promotion, but shared blast radius: one control plane, one node pool, noisy neighbors) vs **cluster-per-env** (prod isolated at the infrastructure level — the common end-state for companies past a certain size, at the cost of fleet management, which is what GitOps phases of this course are about). Common hybrid: cluster-per-env for prod/nonprod, namespace-per-team inside each.

## Lab

### 1. Explore the namespace landscape

```sh
kubectl get namespaces
kubectl api-resources --namespaced=false | head -20
kubectl api-resources --namespaced=true | grep -cE '.'    # plenty of both
kubectl get pods -A -o wide | head                        # -A = --all-namespaces
```

### 2. Create team-a, learn the -n reflex

```sh
kubectl create namespace team-a
kubectl run pinger -n team-a --image=curlimages/curl --command -- sleep 3600
kubectl get pods            # NOT there — default ns
kubectl get pods -n team-a  # there
kubens team-a               # Day 1's tool: switch default ns
kubectl get pods            # there, no -n
kubens default
```

Cross-namespace DNS — from `team-a`, reach the `podlab` Service living in `default`:

```sh
kubectl exec -n team-a pinger -- curl -s --max-time 3 podlab/healthz; echo            # FAILS: short name is ns-local
kubectl exec -n team-a pinger -- curl -s podlab.default/healthz; echo                 # works
kubectl exec -n team-a pinger -- curl -s podlab.default.svc.cluster.local/; echo      # works, FQDN
```

That failure→success pair is the whole cross-namespace DNS lesson: the search path expands short names *within your own namespace* only.

### 3. Labels and selectors workout

Give podlab the recommended labels. Update `deploy.yaml`: in **both** `metadata.labels` and `spec.template.metadata.labels` add:

```yaml
    app.kubernetes.io/name: podlab
    app.kubernetes.io/instance: podlab-main
    app.kubernetes.io/version: "2.0.0"
    app.kubernetes.io/component: api
    app.kubernetes.io/part-of: course
    app.kubernetes.io/managed-by: kubectl
```

Keep the existing `app: podlab` everywhere and **don't touch `spec.selector`** — it's immutable on Deployments, and changing it orphans pods (apply would force you to delete/recreate; leaving it is also realistic: selector minimal, labels rich). Apply, then drill:

```sh
kubectl apply -f deploy.yaml && kubectl rollout status deployment/podlab
kubectl get pods --show-labels
kubectl get pods -l app.kubernetes.io/name=podlab
kubectl get pods -l 'app.kubernetes.io/version in (2.0.0,2.1.0)'
kubectl get pods -l 'app,!debug'                       # has app, lacks debug
kubectl get all -A -l app.kubernetes.io/part-of=course # one query, the whole system — the payoff
kubectl get pods --field-selector spec.nodeName=course-worker
kubectl get events --field-selector type=Warning | tail -5
```

Imperative label/annotate (for live experiments — declarative wins for real changes):

```sh
kubectl label pod pinger -n team-a team=a owner=you
kubectl label pod pinger -n team-a owner-                # remove
kubectl annotate deployment podlab course/last-touched="$(date)" --overwrite
```

### 4. ResourceQuota: give team-a a budget

Requirements — write `quota.yaml` yourself: a ResourceQuota `team-a-quota` in `team-a` capping `pods: 4`, `requests.cpu: 500m`, `requests.memory: 256Mi`, `limits.cpu: "1"`, `limits.memory: 512Mi`.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-a-quota
  namespace: team-a
spec:
  hard:
    pods: "4"
    requests.cpu: 500m
    requests.memory: 256Mi
    limits.cpu: "1"
    limits.memory: 512Mi
```

</details>

```sh
kubectl apply -f quota.yaml
kubectl describe quota team-a-quota -n team-a    # Used vs Hard table
```

Now trip it twice. First, the gotcha — a quota on cpu/memory means undeclared pods are refused:

```sh
kubectl run noresources -n team-a --image=busybox --command -- sleep 3600
# Error from server (Forbidden): ... failed quota: team-a-quota: must specify limits.cpu ...
```

Read that error fully — it names the quota and the missing fields. Second, exceed the budget:

```sh
kubectl run heavy -n team-a --image=busybox --command --overrides='{"spec":{"containers":[{"name":"heavy","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"cpu":"400m","memory":"200Mi"},"limits":{"cpu":"800m","memory":"400Mi"}}}]}}' -- sleep 3600
kubectl run heavy2 -n team-a --image=busybox --command --overrides='{"spec":{"containers":[{"name":"heavy2","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"cpu":"400m","memory":"200Mi"},"limits":{"cpu":"800m","memory":"400Mi"}}}]}}' -- sleep 3600
# Forbidden: exceeded quota: team-a-quota, requested: requests.cpu=400m, used: requests.cpu=400m, limited: requests.cpu=500m
```

The arithmetic is right there in the message: requested + used > limited. Note `pinger` (created *before* the quota) survives — quota gates admission, it never evicts.

### 5. LimitRange: defaults so quota doesn't hurt

Requirements — `limitrange.yaml` in `team-a`: type `Container`, default requests cpu `50m`/memory `32Mi`, default limits cpu `100m`/memory `64Mi`, max memory `256Mi`.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: LimitRange
metadata:
  name: team-a-defaults
  namespace: team-a
spec:
  limits:
    - type: Container
      defaultRequest:
        cpu: 50m
        memory: 32Mi
      default:
        cpu: 100m
        memory: 64Mi
      max:
        memory: 256Mi
```

</details>

```sh
kubectl apply -f limitrange.yaml
kubectl run noresources -n team-a --image=busybox --command -- sleep 3600   # now ADMITTED
kubectl get pod noresources -n team-a -o jsonpath='{.spec.containers[0].resources}'; echo
```

The resources block was **injected at admission** — the pod spec you submitted had none; the one stored has the LimitRange defaults, satisfying the quota. Check `kubectl describe quota -n team-a` again: `Used` ticked up by exactly the defaults. Also confirm the clamp: try a pod requesting `memory: 512Mi` limits and watch the LimitRange reject it (`maximum memory usage per Container is 256Mi`).

## Verify ✅

- [ ] `kubectl exec -n team-a pinger -- curl -s podlab.default/healthz` → `{"status":"ok"}`, while plain `podlab/healthz` from team-a fails
- [ ] `kubectl get all -l app.kubernetes.io/part-of=course` → podlab deployment, RS, pods, service
- [ ] `kubectl run noresources …` (with quota, before LimitRange) → error containing `must specify`
- [ ] Step 4's second heavy pod → error containing `exceeded quota: team-a-quota`
- [ ] `kubectl get pod noresources -n team-a -o jsonpath='{.spec.containers[0].resources.limits.cpu}'` → `100m` (injected, not authored)
- [ ] `kubectl api-resources --namespaced=false | grep -w nodes` → present

## CKA corner 🎓

Exam notes:

- Every kubectl verb takes `-n`; `--all-namespaces`/`-A` for reads. Wrong-namespace blindness is the #1 exam time sink — when something "doesn't exist", check the namespace before anything else.
- `kubectl config set-context --current --namespace=X` is the exam-legal kubens.
- `kubectl describe quota -n X` and `kubectl describe limitrange -n X` give you the full Used/Hard and defaults tables — faster than reading YAML.

**Drill 1 (3 min):** Namespace `drill-q` with quota: max 2 pods, `requests.memory` 100Mi. Prove both limits by creating pods until each rejection, capturing the two distinct error types.

<details><summary>Solution</summary>

```sh
kubectl create ns drill-q
kubectl create quota drill-quota -n drill-q --hard=pods=2,requests.memory=100Mi
kubectl run p1 -n drill-q --image=busybox --overrides='{"spec":{"containers":[{"name":"p1","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"memory":"60Mi"}}}]}}' --command -- sleep 3600
kubectl run p2 -n drill-q --image=busybox --overrides='{"spec":{"containers":[{"name":"p2","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"memory":"60Mi"}}}]}}' --command -- sleep 3600
# rejection 1: exceeded quota ... requests.memory
# shrink p2 to 30Mi → admitted; then:
kubectl run p3 -n drill-q --image=busybox --overrides='{"spec":{"containers":[{"name":"p3","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"memory":"5Mi"}}}]}}' --command -- sleep 3600
# rejection 2: exceeded quota ... pods
kubectl delete ns drill-q
```

(`kubectl create quota` exists — one less YAML file.)

</details>

**Drill 2 (2 min):** In namespace `drill-q2`, ensure any container created without resources gets exactly cpu request 100m / limit 200m. Verify with a bare `kubectl run`.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: LimitRange
metadata: {name: lr, namespace: drill-q2}
spec:
  limits:
    - type: Container
      defaultRequest: {cpu: 100m}
      default: {cpu: 200m}
```

```sh
kubectl create ns drill-q2 && kubectl apply -f lr.yaml
kubectl run t -n drill-q2 --image=busybox --command -- sleep 600
kubectl get pod t -n drill-q2 -o jsonpath='{.spec.containers[0].resources}'; echo
kubectl delete ns drill-q2
```

</details>

## Stretch goals

- Delete a namespace with stuff in it (`kubectl delete ns drill-q`) and watch cascade deletion; then read about namespace finalizers and the infamous "namespace stuck Terminating" failure mode.
- Quota object counts beyond pods: `count/deployments.apps`, `services.loadbalancers` — quota a namespace to zero LoadBalancers (a real cost control).
- `kubectl get pods -A --sort-by=.metadata.creationTimestamp` + field selectors — build your one-liner toolkit.
- Compare `kubectl describe node course-worker`'s Allocated resources before/after team-a's pods — namespace budgets vs node capacity are different layers.

## Cleanup

```sh
kubectl delete ns team-a     # takes pinger, quota, and limitrange with it
```

**Keep:** the `podlab` Deployment/Service in `default`, now wearing recommended labels — they stay for the whole course. Keep metrics-server.
