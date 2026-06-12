# Day 46 — CRDs and the Operator Pattern

> **Time:** ~3 h · **Builds on:** Days 11, 16, 26

## Objectives

- Write a CustomResourceDefinition by hand — schema validation, printer columns, status subresource — and explain every field
- Articulate why a CRD without a controller is inert, and why the controller IS the behavior
- Run a production-grade operator (CloudNativePG), trigger a Postgres failover, and watch it self-heal in seconds
- Decide when to consume an operator vs write one — and defend "usually: consume"

## Concepts

### Kubernetes extends itself with itself

You've been using extensions for weeks without ceremony: `Application` (ArgoCD), `Rollout`, `SealedSecret`, `Certificate`, `ClusterPolicy`, `ServiceMonitor` — none of those ship with Kubernetes. Each arrived as a **CustomResourceDefinition**: a YAML document that tells the apiserver "serve a new resource type."

The crucial insight is that a custom resource is a **full citizen**, not a bolt-on:

| Built-in (`Deployment`) | Custom (`ShortURL`) |
|---|---|
| Served by the apiserver | Served by the apiserver |
| Stored in etcd | Stored in etcd |
| `kubectl get/describe/edit/explain` | `kubectl get/describe/edit/explain` |
| RBAC rules per verb/resource | RBAC rules per verb/resource |
| Watchable (`-w`, informers) | Watchable (`-w`, informers) |
| Validated by a compiled-in schema | Validated by *your* OpenAPI schema |

No plugin system, no sidecar API — the same etcd, the same auth chain, the same watch machinery. That uniformity is why the ecosystem could grow ArgoCD, cert-manager, and Kyverno without forking Kubernetes.

### Anatomy of a CRD

The parts you must be able to read (and will write today):

- **group/versions/kind** — `shorturls.lab.example.com/v1alpha1`, kind `ShortURL`. The group namespaces your API so it can't collide with anyone else's; the CRD's own name *must* be `<plural>.<group>`.
- **OpenAPI schema** — `openAPIV3Schema` under each version. The apiserver enforces it at admission: required fields, regex patterns, min/max. Garbage gets a 4xx *before* anything touches etcd — your data type gets the same gate Deployments get.
- **served / storage** — each version flags whether the apiserver answers for it (`served`) and exactly one version is what's written to etcd (`storage`). This is how APIs evolve: serve `v1alpha1` and `v1` simultaneously, store `v1`, convert on read. (You met the consumer side on Day 23: pluto warns when you use a served-but-deprecated version.)
- **subresources** — `status: {}` splits `/status` into its own endpoint. Writes to the main resource can't touch `.status` and vice versa, and RBAC can grant a controller status-write without spec-write. `scale: {...}` is the other one — it's what makes `kubectl scale` and HPA work on custom types like Rollouts.
- **additionalPrinterColumns** — JSONPath-driven columns for `kubectl get`. Cosmetic, but it's why `kubectl get certificates` shows READY/SECRET/AGE instead of just names.

### Controllers: the loop that makes YAML do things

A CRD defines *nouns*. Nothing acts on them. The actor is a **controller**: a process (usually a Deployment in-cluster) running a **reconcile loop**:

```
            ┌────────────────────────────────────────┐
            │  watch my resource type (+ owned types)│
            ▼                                        │
   observe actual state ──► compare with spec ──► act to converge ──┘
                            (desired state)      (create/update/delete)
```

This is not a pattern invented for extensions — it's how *all* of Kubernetes works. The deployment-controller watches Deployments and reconciles ReplicaSets; the replicaset-controller watches ReplicaSets and reconciles Pods; the scheduler is a controller that reconciles `spec.nodeName`. Built-ins just happen to be compiled into `kube-controller-manager` (you toured it on Day 16). When you write a controller for your CRD, you're joining the same game with the same rules: level-triggered (reconcile from observed state, never from "what event did I miss"), idempotent, and driven by **watches** — long-lived streams from the apiserver, not polling.

### Operator = CRDs + controller + domain knowledge

An **operator** is the pattern applied to running real software: encode what a human expert would do — provision, configure replication, elect a primary, fail over, take backups, coordinate version upgrades — as reconcile logic against high-level CRs. CloudNativePG's `Cluster` resource says "3 Postgres instances, 1Gi storage"; the operator knows everything else, including what to do at 3am when the primary dies.

**When to write one:** you operate the same complex stateful thing many times, the runbook is genuinely automatable, and no good operator exists. **When not to (most of the time):** a controller is a distributed-systems program — caches, requeues, conflict retries, upgrade paths for stored versions. For Postgres, Kafka, Elasticsearch, certificates, DNS — mature operators exist with years of failure-mode hardening you don't have. Most teams should be expert *consumers* of operators and authors of, at most, small glue controllers. If you ever do write one: **kubebuilder** is the standard Go scaffold (controller-runtime underneath), **operator-sdk** wraps kubebuilder with lifecycle/OLM packaging, and **metacontroller** lets you write just the reconcile function as a webhook in any language while it handles the watch machinery — a good prototyping middle ground.

## Lab

### Part 1 — write a CRD by hand

#### 1. The CRD

Write `shorturl-crd.yaml` yourself first. Requirements:

- Group `lab.example.com`, version `v1alpha1` (served + storage), kind `ShortURL`, plural `shorturls`, namespaced, short name `su`
- Schema: `spec.targetURL` (string, required, must match `^https?://.+`), `spec.alias` (string, required, 2–30 chars, lowercase slug pattern), `status.ready` (boolean)
- `status` subresource enabled
- Printer columns: Target, Alias, Ready, Age

<details><summary>Solution</summary>

See [`shorturl-crd.yaml`](shorturl-crd.yaml) in this folder — fully commented. Key excerpt:

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: shorturls.lab.example.com
spec:
  group: lab.example.com
  scope: Namespaced
  names: {kind: ShortURL, singular: shorturl, plural: shorturls, shortNames: [su]}
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources: {status: {}}
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              required: ["targetURL", "alias"]
              properties:
                targetURL: {type: string, pattern: '^https?://.+'}
                alias: {type: string, minLength: 2, maxLength: 30, pattern: '^[a-z0-9-]+$'}
            status:
              type: object
              properties:
                ready: {type: boolean}
      additionalPrinterColumns:
        - {name: Target, type: string, jsonPath: .spec.targetURL}
        - {name: Alias, type: string, jsonPath: .spec.alias}
        - {name: Ready, type: boolean, jsonPath: .status.ready}
        - {name: Age, type: date, jsonPath: .metadata.creationTimestamp}
```

</details>

```sh
kubectl apply -f shorturl-crd.yaml
kubectl get crd shorturls.lab.example.com
kubectl api-resources | grep shorturl
```

Your type now appears next to `pods` and `deployments` in the API discovery output. Same API machinery, your noun.

#### 2. Your type has documentation

```sh
kubectl explain shorturl
kubectl explain shorturl.spec
kubectl explain shorturl.spec.targetURL
```

`kubectl explain` is reading **your** OpenAPI schema back from the apiserver — the `description` fields you wrote are now built-in docs for anyone on the cluster.

#### 3. Create instances — and watch validation work

```sh
kubectl create ns shorturl-lab
cat <<'EOF' | kubectl apply -f -
apiVersion: lab.example.com/v1alpha1
kind: ShortURL
metadata:
  name: course
  namespace: shorturl-lab
spec:
  targetURL: https://github.com/your-user/k8s-gitops
  alias: course
EOF
kubectl get shorturls -n shorturl-lab     # printer columns in action
kubectl get su -n shorturl-lab            # short name works too
```

Now feed it garbage:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: lab.example.com/v1alpha1
kind: ShortURL
metadata:
  name: bad
  namespace: shorturl-lab
spec:
  targetURL: not-a-url
  alias: "X"
EOF
```

Rejected by the apiserver with field-level errors — `targetURL` fails the pattern, `alias` fails pattern and minLength. Nothing reached etcd. Your schema is doing admission-time validation exactly like a built-in.

#### 4. The lesson: nothing happens

```sh
kubectl get shorturls -n shorturl-lab
```

The `Ready` column is empty. It will stay empty forever. No redirect exists, no pods were created, nothing watches this type. **A CRD without a controller is a database table.** Everything you've ever seen a CR "do" — a SealedSecret decrypting, a Certificate getting issued, a Rollout shifting traffic — was a controller noticing a watch event and acting. The CRD is the schema; the controller is the behavior. (Stretch goal: fake being the controller yourself.)

### Part 2 — a real operator: CloudNativePG

#### 5. Install the operator via ArgoCD

CloudNativePG (a CNCF project) runs production Postgres: HA, failover, backups, PITR. Add it to your platform the way you add everything now — an Application in `~/Code/k8s-gitops/argocd/apps/cnpg.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cloudnative-pg
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"
spec:
  project: default
  source:
    repoURL: https://cloudnative-pg.github.io/charts
    chart: cloudnative-pg
    targetRevision: "*"           # pin to the current version after first sync
    helm:
      values: ""
  destination:
    server: https://kubernetes.default.svc
    namespace: cnpg-system
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions: ["CreateNamespace=true", "ServerSideApply=true"]
```

(`ServerSideApply=true` matters: CNPG's CRDs are too large for the client-side-apply annotation. Manifest alternative if you prefer: `kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.1.yaml` — check their docs for the current release.)

```sh
cd ~/Code/k8s-gitops && git add . && git commit -m "add cloudnative-pg operator" && git push
kubectl get pods -n cnpg-system -w     # one operator deployment
kubectl get crds | grep cnpg           # clusters, backups, poolers, scheduledbackups...
```

#### 6. Ask for a database — declaratively

```sh
kubectl create ns cnpg-lab
cat <<'EOF' | kubectl apply -f -
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-lab
  namespace: cnpg-lab
spec:
  instances: 3
  storage:
    size: 1Gi
EOF
```

Eleven lines. Now watch an expert DBA work:

```sh
kubectl get pods -n cnpg-lab -w
```

The operator runs an init job, bootstraps `pg-lab-1`, then joins `-2` and `-3` as **streaming replicas** — sequenced, health-checked, in the right order. Inspect what it built:

```sh
kubectl get cluster pg-lab -n cnpg-lab          # printer columns: instances, ready, primary
kubectl get svc -n cnpg-lab                     # pg-lab-rw (primary), pg-lab-ro (replicas), pg-lab-r (any)
kubectl get secrets -n cnpg-lab                 # generated app credentials, TLS certs
kubectl get cluster pg-lab -n cnpg-lab -o jsonpath='{.status.currentPrimary}{"\n"}'
```

The `-rw`/`-ro`/`-r` service split is the operator encoding domain knowledge: apps write to the primary, reports read from replicas, and *the operator* keeps those services pointed at the right pods.

#### 7. The failover drill

This is the moment that retroactively justifies operators. Kill the primary:

```sh
PRIMARY=$(kubectl get cluster pg-lab -n cnpg-lab -o jsonpath='{.status.currentPrimary}')
kubectl delete pod $PRIMARY -n cnpg-lab &
kubectl get cluster pg-lab -n cnpg-lab -w
```

Within seconds: the operator detects the failure, **promotes a replica to primary**, repoints `pg-lab-rw`, and rebuilds the old primary as a new replica. Check:

```sh
kubectl get cluster pg-lab -n cnpg-lab -o jsonpath='{.status.currentPrimary}{"\n"}'   # a different pod
```

Optional but nice — the krew plugin (krew itself is tomorrow's first lab if you don't have it):

```sh
kubectl krew install cnpg
kubectl cnpg status pg-lab -n cnpg-lab     # full topology, replication lag, who's primary
```

Now the honest comparison with your Day 11 StatefulSet. That Postgres had stable identity and surviving storage — and that's all. One replica; a second one would have been an empty, unrelated database. No replication, no failover (a dead node = an outage until the pod reschedules *with its data, on local-path storage: maybe never*), no backups, no PITR, no coordinated upgrades. Your Day 11 build was the right way to *learn* StatefulSets; production Postgres belongs to an operator that carries the runbook in code.

#### 8. No magic — look under the hood

```sh
kubectl get deploy,sa,clusterrole -n cnpg-system | head
kubectl get validatingwebhookconfigurations | grep cnpg
kubectl get crd clusters.postgresql.cnpg.io -o yaml | head -40
```

The operator is: a Deployment, RBAC, CRDs, and admission webhooks. The same primitives you've written by hand all course — composed into a reconcile loop with a DBA's judgment. That's the whole trick.

## Verify ✅

- [ ] `kubectl api-resources | grep shorturls` → `shorturls  su  lab.example.com/v1alpha1  true  ShortURL`
- [ ] `kubectl explain shorturl.spec.targetURL` → prints your description text
- [ ] Applying the invalid ShortURL → error mentioning `spec.targetURL` pattern (and nothing created)
- [ ] `kubectl get su -n shorturl-lab` → columns TARGET / ALIAS / READY, with READY empty
- [ ] `kubectl get cluster pg-lab -n cnpg-lab` → `Cluster in healthy state`, 3/3 instances
- [ ] After deleting the primary pod: `.status.currentPrimary` changed, cluster back to healthy within ~1 min
- [ ] `kubectl get svc -n cnpg-lab` → `pg-lab-rw`, `pg-lab-ro`, `pg-lab-r` all present

## Interview corner 💬

**"What's an operator, really?"**

> Strong answer avoids buzzwords: "One or more CRDs plus a controller plus encoded operational knowledge. The CRD adds a domain-level API type to the apiserver — stored in etcd, RBAC'd, watchable like any built-in. The controller runs a reconcile loop: watch the resource, diff desired vs actual, act to converge — the identical pattern Deployments and ReplicaSets use internally. What makes it an *operator* rather than just a controller is the domain expertise: CloudNativePG doesn't just create three pods, it manages replication, primary election, failover, and backups — a DBA's runbook as software."

**"When would you NOT write an operator?"**

> Strong answer: "Almost always. I'd write one only if we operate the same complex stateful system repeatedly, the runbook is truly mechanizable, and no mature operator exists. A controller is a distributed-systems program — informer caches, idempotency, conflict retries, CRD version migration — and mature operators embed years of failure handling. For one-off needs I reach for simpler tools first: Helm for templating, Kyverno generate rules for stamped resources, a CronJob for periodic logic. Consume operators; write them as a last resort."

**"How does a controller know when to act — does it poll the API?"**

> Strong answer: "It uses **watches**: long-lived streaming connections where the apiserver pushes change events, backed by etcd's watch feature. Client libraries wrap this in informers — a local cache plus event handlers — so reconciles read from cache and don't hammer the apiserver. Crucially controllers are *level-triggered*: an event only queues a key, and the reconcile re-reads current state and converges from there. Miss an event (restart, network blip)? The periodic resync and the next reconcile still arrive at the right answer. Edge-triggered logic ('react to exactly what changed') is how you build controllers that drift."

## Stretch goals

- **Be the controller yourself**: `kubectl patch shorturl course -n shorturl-lab --subresource=status --type=merge -p '{"status":{"ready":true}}'` — note that a normal `kubectl edit` of status silently no-ops because of the subresource split. Then actually reconcile: create a ConfigMap of alias→URL pairs and an nginx pod that serves redirects from it. You just hand-executed one reconcile pass — a controller is this, in a loop.
- Add a `v1` version to the CRD with an extra optional field; serve both, storage `v1`; see `kubectl get shorturl course -o yaml` return v1. Version conversion without a webhook works when schemas are compatible.
- Connect guestbook to the operator-managed DB: read `pg-lab-app` secret, point a guestbook copy's `DATABASE_URL` at `pg-lab-rw.cnpg-lab`, then run the failover drill **while POSTing entries** and count dropped requests.
- Skim the kubebuilder quickstart (book.kubebuilder.io) — scaffold a project locally and find where your reconcile function would go; even without writing logic, the shape is informative.

## Cleanup

- `kubectl delete ns shorturl-lab && kubectl delete crd shorturls.lab.example.com` — the hand-made CRD was a teaching prop.
- `kubectl delete ns cnpg-lab` — the Cluster CR and its PVCs go with it (watch the operator tear it down gracefully).
- **Keep the cloudnative-pg Application** in `k8s-gitops` — it's part of your platform now and Day 48 will rebuild it from git automatically. (If RAM is tight, you may delete the app from `argocd/apps/` instead — your call; just know what your bootstrap will and won't include.)
