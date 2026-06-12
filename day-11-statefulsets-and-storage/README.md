# Day 11 — StatefulSets and Storage

> **Time:** ~3 h · **Builds on:** Days 4, 7, 10

## Objectives

- Explain the PV/PVC/StorageClass model and how dynamic provisioning works on kind
- Deploy Postgres as a StatefulSet with a headless Service and a volumeClaimTemplate
- Run the guestbook API against it and prove data survives pod deletion
- Demonstrate alive ≠ ready: `/healthz` 200 while `/readyz` returns 503 with the DB down

## Concepts

### Storage is a request, not a disk

Pods are ephemeral. Anything written to a container filesystem dies with the pod. Kubernetes separates *what an app needs* from *what the cluster has*:

| Object | Who creates it | What it says |
|---|---|---|
| **PersistentVolume (PV)** | Admin or provisioner | "Here is 1Gi of actual storage" (a disk, NFS export, host dir) |
| **PersistentVolumeClaim (PVC)** | You, next to your app | "I need 1Gi, ReadWriteOnce" — a *request* |
| **StorageClass** | Admin | "Claims of this class are fulfilled by *this provisioner* with *these parameters*" |

A PVC **binds** to exactly one PV. With **static provisioning** an admin pre-creates PVs and claims fish for a match (capacity, access mode, class). With **dynamic provisioning** — the normal mode everywhere — the claim names a StorageClass, and the class's provisioner creates a fitting PV on the fly. That's why in real clusters you almost never write a PV by hand: the claim is the API, the class is the implementation.

kind ships a StorageClass called `standard` backed by the **rancher local-path provisioner**: it creates a directory under `/var/local-path-provisioner` *on the node* and serves it as a PV. Two consequences worth internalizing:

1. **`volumeBindingMode: WaitForFirstConsumer`** — the PV isn't created until a pod actually uses the PVC, because the provisioner must know *which node* the pod landed on before it can carve out a host directory there. Until then the PVC sits in `Pending` — that's normal, not broken.
2. The data lives on one node. If that node dies, the data is gone. Fine for a lab; in clouds the provisioner creates EBS/PD volumes that outlive nodes.

**Access modes** describe how many nodes can mount a volume: `ReadWriteOnce` (one node — most block storage), `ReadOnlyMany`, `ReadWriteMany` (needs a shared filesystem like NFS/EFS), `ReadWriteOncePod` (one pod, enforced). **Reclaim policy** says what happens to the PV when the claim is deleted: `Delete` (provisioned storage is destroyed — the default for dynamic) or `Retain` (PV sticks around `Released`, data preserved, admin cleans up manually).

### Why a Deployment fails Postgres

Run Postgres in a Deployment and three things break:

- **Identity**: ReplicaSet pods get random names (`db-7f9c4-x2vqr`). After a restart it's a *different* pod. Databases that form clusters need stable names.
- **Storage**: a Deployment has one pod template, so every replica would mount the *same* PVC. Two Postgres processes on one data dir corrupts it. ReadWriteOnce won't even allow it across nodes.
- **Order**: rolling updates replace pods in arbitrary overlap. Stateful systems usually need one-at-a-time, ordered operations.

A **StatefulSet** gives you exactly these guarantees:

```
Deployment:   web-7f9c4d8b6-x2vqr   web-7f9c4d8b6-p8jw3      (random, interchangeable)
StatefulSet:  guestbook-db-0        guestbook-db-1           (ordinal, stable)
                  │                      │
              PVC: data-guestbook-db-0   data-guestbook-db-1  (one claim PER replica)
```

- **Stable identity**: pods are named `<sts>-0`, `<sts>-1`, … A deleted pod is recreated with the *same name* and reattached to the *same PVC*.
- **Per-replica storage**: `volumeClaimTemplates` stamps out one PVC per ordinal. Deleting the StatefulSet does **not** delete the PVCs — deliberate, so your data survives an accidental `kubectl delete sts`.
- **Ordered rollout**: pods start 0→N-1 (each waiting for the previous to be Ready) and terminate N-1→0. Updates roll highest-ordinal-first.

### Headless Services and per-pod DNS

A normal Service gives you one virtual IP load-balancing across pods — useless when you must reach *pod 0 specifically* (e.g. the primary in a replicated DB). A **headless Service** (`clusterIP: None`) creates no VIP; instead DNS returns the pod IPs directly, and — combined with the StatefulSet's `serviceName` — each pod gets its own stable record:

```
guestbook-db-0.guestbook-db.guestbook.svc.cluster.local
└── pod ──┘    └─ service ─┘ └─ ns ──┘
```

For a 1-replica database the headless service name alone (`guestbook-db`) resolves to the single pod, which is exactly what the guestbook's default `DATABASE_URL` expects.

Today you build the stack this course leans on for weeks: Postgres (StatefulSet) + guestbook API (Deployment) in a `guestbook` namespace. **It stays on the cluster** — Day 15 wraps NetworkPolicies around it and Day 43 backs it up with Velero.

## Lab

### 1. Inspect the storage machinery kind gave you

```sh
kubectl get storageclass
kubectl get sc standard -o yaml
```

Note three fields: `provisioner: rancher.io/local-path`, `reclaimPolicy: Delete`, `volumeBindingMode: WaitForFirstConsumer`. The provisioner itself is a pod:

```sh
kubectl get pods -n local-path-storage
```

### 2. Namespace and Secret

```sh
kubectl create namespace guestbook
kubectl create secret generic guestbook-db \
  -n guestbook \
  --from-literal=POSTGRES_PASSWORD=supersecret \
  --from-literal=DATABASE_URL='postgres://guestbook:supersecret@guestbook-db:5432/guestbook?sslmode=disable'
```

The host in `DATABASE_URL` is `guestbook-db` — the headless Service you're about to create.

### 3. Postgres as a StatefulSet — the core object

Write `postgres.yaml` yourself first. Requirements:

- A **headless Service** named `guestbook-db` (`clusterIP: None`, port 5432, selector `app: guestbook-db`)
- A **StatefulSet** `guestbook-db`, 1 replica, `serviceName: guestbook-db`, image `postgres:16`
- Env: `POSTGRES_USER=guestbook`, `POSTGRES_DB=guestbook`, `POSTGRES_PASSWORD` from the Secret
- Set `PGDATA=/var/lib/postgresql/data/pgdata` (Postgres wants an empty dir; a subdirectory of the mount avoids fighting anything the volume root contains)
- `volumeClaimTemplates`: one claim named `data`, 1Gi, `ReadWriteOnce`, mounted at `/var/lib/postgresql/data`
- Readiness probe: `exec` running `pg_isready -U guestbook -d guestbook`

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Service
metadata:
  name: guestbook-db
  namespace: guestbook
spec:
  clusterIP: None
  selector:
    app: guestbook-db
  ports:
  - port: 5432
    name: postgres
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: guestbook-db
  namespace: guestbook
spec:
  serviceName: guestbook-db
  replicas: 1
  selector:
    matchLabels:
      app: guestbook-db
  template:
    metadata:
      labels:
        app: guestbook-db
    spec:
      containers:
      - name: postgres
        image: postgres:16
        ports:
        - containerPort: 5432
          name: postgres
        env:
        - name: POSTGRES_USER
          value: guestbook
        - name: POSTGRES_DB
          value: guestbook
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: guestbook-db
              key: POSTGRES_PASSWORD
        - name: PGDATA
          value: /var/lib/postgresql/data/pgdata
        readinessProbe:
          exec:
            command: ["pg_isready", "-U", "guestbook", "-d", "guestbook"]
          initialDelaySeconds: 5
          periodSeconds: 5
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi
```

</details>

```sh
kubectl apply -f postgres.yaml
kubectl get pods,pvc,pv -n guestbook -w
```

Watch the sequence: PVC `Pending` → pod scheduled → provisioner creates the PV → PVC `Bound` → pod `Running` → readiness goes `1/1`. Note the PVC name: `data-guestbook-db-0` — `<template>-<sts>-<ordinal>`.

### 4. The guestbook API

This is boilerplate by now — `api.yaml` inline:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: guestbook
  namespace: guestbook
spec:
  replicas: 2
  selector:
    matchLabels:
      app: guestbook
  template:
    metadata:
      labels:
        app: guestbook
    spec:
      containers:
      - name: guestbook
        image: guestbook:v1
        ports:
        - containerPort: 8080
        env:
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              name: guestbook-db
              key: DATABASE_URL
        readinessProbe:
          httpGet: {path: /readyz, port: 8080}
          periodSeconds: 5
        livenessProbe:
          httpGet: {path: /healthz, port: 8080}
          periodSeconds: 10
---
apiVersion: v1
kind: Service
metadata:
  name: guestbook
  namespace: guestbook
spec:
  selector:
    app: guestbook
  ports:
  - port: 80
    targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: guestbook
  namespace: guestbook
spec:
  ingressClassName: nginx
  rules:
  - host: guestbook.localhost
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: guestbook
            port:
              number: 80
```

```sh
kubectl apply -f api.yaml
kubectl get pods -n guestbook -w   # wait for 2/2 api pods Ready
```

### 5. Use it

```sh
curl -s -X POST http://guestbook.localhost:8080/entries \
  -H 'Content-Type: application/json' -d '{"message":"hello from day 11"}'
curl -s -X POST http://guestbook.localhost:8080/entries \
  -H 'Content-Type: application/json' -d '{"message":"statefulsets are neat"}'
curl -s http://guestbook.localhost:8080/entries
```

### 6. The whole point: kill the database

```sh
kubectl delete pod guestbook-db-0 -n guestbook
kubectl get pods -n guestbook -w
```

The replacement pod is named `guestbook-db-0` again — not a random suffix — and `kubectl describe pod guestbook-db-0 -n guestbook` shows it mounting the **same** `data-guestbook-db-0` claim. Once it's Ready:

```sh
curl -s http://guestbook.localhost:8080/entries
```

Your entries are still there. Stable name + stable claim = data survives the pod. (In k9s: watch the pod cycle on the `:pods` view filtered to the namespace, then `:pvc` to see the claim never blinked.)

### 7. Alive ≠ ready

Take the database away entirely and watch the API react *correctly*:

```sh
kubectl scale statefulset guestbook-db -n guestbook --replicas=0
kubectl get pods -n guestbook -w        # api pods drop to 0/1 READY — but 0 RESTARTS
kubectl get endpoints guestbook -n guestbook   # ENDPOINTS: <none>
curl -si http://guestbook.localhost:8080/entries   # 503 from nginx — no backends
```

The Service won't route to a not-ready pod, so probe a pod IP directly to see both answers at once:

```sh
API_IP=$(kubectl get pod -n guestbook -l app=guestbook -o jsonpath='{.items[0].status.podIP}')
kubectl run probe-check -n guestbook --image=curlimages/curl --rm -it --restart=Never -- \
  sh -c "echo healthz:; curl -si http://$API_IP:8080/healthz | head -1; echo readyz:; curl -si http://$API_IP:8080/readyz | head -1"
```

`healthz: 200`, `readyz: 503`. Liveness says "process is fine, don't restart me"; readiness says "don't send me traffic". Restarting the API would not fix a missing database — this is exactly why the two probes exist separately (Day 10 theory, now in anger). Bring it back:

```sh
kubectl scale statefulset guestbook-db -n guestbook --replicas=1
```

The PVC was never deleted, so the same data returns, the API goes Ready, entries reappear.

## Verify ✅

- [ ] `kubectl get sc standard -o jsonpath='{.provisioner} {.volumeBindingMode}'` → `rancher.io/local-path WaitForFirstConsumer`
- [ ] `kubectl get pvc -n guestbook` → `data-guestbook-db-0  Bound  ... 1Gi`
- [ ] `kubectl get pods -n guestbook` → `guestbook-db-0 1/1`, two `guestbook-...` pods `1/1`
- [ ] `curl -s http://guestbook.localhost:8080/entries` returns your POSTed messages
- [ ] After `kubectl delete pod guestbook-db-0 -n guestbook`: pod comes back with the **same name** and `curl .../entries` still returns the messages
- [ ] With the sts scaled to 0: `kubectl get endpoints guestbook -n guestbook` shows `<none>` and api pods show `0/1` with **0 restarts**
- [ ] `kubectl run dns-test -n guestbook --image=busybox --rm -it --restart=Never -- nslookup guestbook-db-0.guestbook-db.guestbook.svc.cluster.local` resolves to the pod IP

## CKA corner 🎓

Storage questions are mechanical points if you know the binding rules: a PVC binds only to a PV with **the same storageClassName**, **a compatible access mode**, and **capacity ≥ the request**. No match → PVC stays `Pending` forever (or until a PV appears).

**Drill 1 — static binding (5 min).** Create a PV `pv-manual` (2Gi, `ReadWriteOnce`, `hostPath: /tmp/pv-manual`, `storageClassName: manual`) and a PVC `claim-manual` in `default` requesting 1Gi of class `manual`. Verify they bind, and explain why the PVC shows capacity **2Gi**, not 1Gi.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: pv-manual
spec:
  capacity:
    storage: 2Gi
  accessModes: ["ReadWriteOnce"]
  storageClassName: manual
  hostPath:
    path: /tmp/pv-manual
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: claim-manual
  namespace: default
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: manual
  resources:
    requests:
      storage: 1Gi
```

`kubectl get pvc claim-manual` → `Bound`. Capacity shows 2Gi because binding is whole-PV: the claim gets the entire volume even if it asked for less. Clean up: delete the PVC, then the PV.
</details>

**Drill 2 — diagnose a Pending PVC (3 min).** Create a PVC requesting class `standard` on this cluster and observe it stays `Pending` with event `WaitForFirstConsumer`. What two different root causes produce a Pending PVC, and how do you tell them apart?

<details><summary>Solution</summary>

`kubectl describe pvc <name>` — read the Events. (a) `waiting for first consumer to be created before binding`: not an error, the class is `WaitForFirstConsumer`; create a pod that mounts it. (b) `no persistent volumes available for this claim` / provisioner errors: a real matching problem — wrong class name, no PV with enough capacity, or access-mode mismatch. The event text is the answer; always `describe` the PVC first.
</details>

**Drill 3 — expand a PVC (4 min).** Grow `data-guestbook-db-0` from 1Gi to 2Gi. What field on the StorageClass gates this, and what happens here?

<details><summary>Solution</summary>

Expansion requires `allowVolumeExpansion: true` on the StorageClass. Check: `kubectl get sc standard -o jsonpath='{.allowVolumeExpansion}'` — empty/false on kind's local-path class, so `kubectl edit pvc data-guestbook-db-0 -n guestbook` (raise `spec.resources.requests.storage` to `2Gi`) is **rejected by the API**. On the exam the class usually allows it: edit the PVC (never the PV), then watch `kubectl get pvc -w` — status conditions show resize progress; some volume types need a pod restart to finish filesystem expansion. Remember: you can only grow, never shrink.
</details>

## Stretch goals

- Scale the StatefulSet to 2 and watch ordered startup: `guestbook-db-1` only starts after `-0` is Ready, and gets its own fresh `data-guestbook-db-1` PVC (an empty second Postgres — useless without replication, which is the point: per-replica storage isn't shared storage). Scale back to 1; note the PVC `data-guestbook-db-1` *remains* — delete it by hand.
- `docker exec -it course-worker sh` (or `course-worker2`, wherever the pod ran) and `ls /var/local-path-provisioner/` — find your Postgres data dir on the node, see the actual files.
- Delete the StatefulSet (`kubectl delete sts guestbook-db -n guestbook`), confirm the PVC survives, re-apply `postgres.yaml`, confirm data is back. (Do re-apply — the stack must stay.)

## Cleanup

- Delete Drill 1's `pv-manual` / `claim-manual` and any Pending drill PVCs.
- **KEEP everything in the `guestbook` namespace** — Secret, StatefulSet, PVC, Deployment, Service, Ingress. Day 12's CronJob curls it, Day 15 wraps NetworkPolicies around it, Day 43 backs it up with Velero. Save `postgres.yaml` and `api.yaml` in this folder — **you will redeploy them from scratch on Day 15**.
