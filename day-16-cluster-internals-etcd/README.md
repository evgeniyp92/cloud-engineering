# Day 16 — Cluster Internals and the etcd Backup/Restore Drill

> **Time:** ~3.5 h · **Builds on:** Days 1, 15

## Objectives

- Narrate exactly what each control-plane component does when you `kubectl apply` a Deployment
- Find and modify static pod manifests, and explain mirror pods
- Take an etcd snapshot with the right cert flags and restore it — watching cluster state roll back
- Tour the kubeadm PKI and check certificate expiration

## Concepts

### Who does what: the apply story

The control plane is five processes with one strict rule: **only the API server talks to etcd.** Everyone else — controllers, scheduler, kubelets, you — talks to the API server, mostly via *watches* (long-lived "tell me when this changes" connections). Nothing calls anything else directly; all coordination happens through state in the API. That's the architecture in one sentence.

Trace `kubectl apply -f deployment.yaml` (3 replicas) end to end:

```
you ──▶ kube-apiserver ── authn/authz/admission ──▶ etcd (Deployment stored)
              ▲ │
   watches────┘ └─watch events
1. kube-controller-manager (deployment controller): "a Deployment exists but no
   matching ReplicaSet" → creates ReplicaSet            → apiserver → etcd
2. kube-controller-manager (replicaset controller): "RS wants 3, has 0"
   → creates 3 Pods (spec.nodeName empty)               → apiserver → etcd
3. kube-scheduler: "pods with no node" → filter/score (Day 13) → binds each pod
4. kubelet on each chosen node: "a pod is bound to me" → pulls image, asks
   containerd (via CRI) to start containers, runs probes → reports status up
5. kube-proxy + endpoint controllers: pod IPs become Service endpoints
```

Note what "controller-manager" means: a bag of **reconcile loops**, each endlessly comparing desired state (spec) to observed state (status) and nudging reality. Nothing imperative ever happens; you write a wish into etcd and five independent loops conspire to make it true. This is also your debugging map — Pending pod with no events? Scheduler. Deployment that never makes a ReplicaSet? Controller-manager. Pod bound but not starting? That node's kubelet. (Full table in CKA corner.)

### Static pods: the bootstrap trick

Riddle: the scheduler is a pod. Who scheduled the scheduler?

Answer: nobody. The kubelet, besides watching the API server, also watches a local directory — `/etc/kubernetes/manifests` — and runs any pod manifest dropped there, no API server needed. These are **static pods**. kubeadm bootstraps a cluster by writing four manifests into that directory on the control-plane node: etcd, kube-apiserver, kube-controller-manager, kube-scheduler. The kubelet starts them from files; *then* a cluster exists.

For each static pod, the kubelet creates a read-only **mirror pod** in the API so you can see it (`kubectl get pods -n kube-system` — the ones suffixed with the node name). Deleting the mirror does nothing; the file is the source of truth. Editing or moving the file is how you reconfigure or restart a control-plane component — which is exactly what today's restore drill does.

### etcd: the only state there is

Everything `kubectl get` shows you is a key under `/registry/` in etcd. Lose etcd, lose the cluster — pods keep running (kubelets are autonomous in the short term) but nothing can be changed, and on restart nothing comes back. **An etcd snapshot is therefore a full cluster-state backup**, and `etcdctl snapshot save` / restore is a canonical CKA task.

etcd serves TLS with client-cert auth, so every etcdctl call needs three flags pointing into `/etc/kubernetes/pki/etcd/`: `--cacert ca.crt`, `--cert server.crt`, `--key server.key` (plus `--endpoints=https://127.0.0.1:2379`). Restore is the subtle half: you don't restore "into" a running etcd — you materialize the snapshot into a **new data directory** with `etcdutl snapshot restore`, then point etcd's static-pod manifest at it and let the kubelet restart etcd. The API server reconnects, and the cluster's entire state is whatever the snapshot held. Anything created after the snapshot has never existed, as far as the cluster is concerned.

### How the components find each other

Worth knowing for debugging: the non-apiserver components each hold a **kubeconfig** — same format as yours — in `/etc/kubernetes/` on the control-plane node: `controller-manager.conf`, `scheduler.conf`, `kubelet.conf`, plus `admin.conf` (the cluster-admin credentials kind copied to your laptop on Day 1). Each embeds a client certificate whose CN is the component's identity (`system:kube-scheduler`, …), authorized by pre-baked RBAC bindings — Day 14's machinery, eating its own dog food. When a control-plane component logs `Unauthorized` or `connection refused`, the diagnosis is the same as for any client: wrong/expired cert in its kubeconfig, or the apiserver address in it is unreachable.

### PKI: the certificates holding it together

Every arrow in the diagram above is mTLS. kubeadm generates a CA (actually three: cluster, etcd, front-proxy) and a dozen leaf certs under `/etc/kubernetes/pki/`: apiserver's serving cert, its client certs for kubelets and etcd, etcd's serving/peer certs, the controller-manager's and scheduler's kubeconfig credentials, your `kubernetes-admin` cert. kubeadm leaf certs live **1 year**; the CAs 10. Expired apiserver/kubelet certs are a classic "cluster suddenly dead" incident — `kubeadm certs check-expiration` is the 10-second audit, `kubeadm certs renew all` the fix (a cluster upgrade also renews them, which is why regularly-upgraded clusters never notice).

## Lab

kind makes all of this unusually inspectable: each "node" is a Docker container, so `docker exec` is your SSH.

### 1. Static pod safari

```sh
docker exec course-control-plane ls /etc/kubernetes/manifests
# etcd.yaml  kube-apiserver.yaml  kube-controller-manager.yaml  kube-scheduler.yaml

docker exec course-control-plane cat /etc/kubernetes/manifests/etcd.yaml
```

Read `etcd.yaml` properly — you'll edit it later. Find: the `--data-dir=/var/lib/etcd` arg, the `--cert-file`/`--key-file`/`--trusted-ca-file` args, and the `etcd-data` volume (`hostPath: /var/lib/etcd`). Then prove the file-is-truth property:

```sh
kubectl get pod -n kube-system kube-scheduler-course-control-plane   # the mirror pod
kubectl delete pod -n kube-system kube-scheduler-course-control-plane
kubectl get pod -n kube-system kube-scheduler-course-control-plane   # back instantly — you only deleted the mirror
```

Now restart the scheduler for real by moving its manifest:

```sh
docker exec course-control-plane mv /etc/kubernetes/manifests/kube-scheduler.yaml /tmp/
kubectl get pods -n kube-system | grep scheduler        # gone within seconds
kubectl run orphan --image=busybox -- sleep 300
kubectl get pod orphan                                  # Pending — nobody schedules!
docker exec course-control-plane mv /tmp/kube-scheduler.yaml /etc/kubernetes/manifests/
kubectl get pod orphan -w                               # scheduler returns, pod runs
kubectl delete pod orphan
```

You just diagnosed-by-construction the "pods stay Pending with no events" failure mode.

### 2. PKI tour

```sh
docker exec course-control-plane ls /etc/kubernetes/pki /etc/kubernetes/pki/etcd
docker exec course-control-plane kubeadm certs check-expiration
```

Read the table: every cert, its expiry (~1 year out), and which CA signed it. Peek inside one:

```sh
docker exec course-control-plane sh -c \
  'openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -subject -ext subjectAltName'
```

The SANs include `kubernetes.default.svc`, the service VIP, and `localhost` — every name a client might dial the apiserver by must be in this list (the source of the classic "x509: certificate is valid for X, not Y" error).

### 3. etcd snapshot save

> ⚠️ Steps 3–5 are the **destructive drill**. Done in order they're safe — the snapshot is taken *before* anything else, and the only casualty is a marker deployment created *after* it. If you'd rather rehearse on disposable hardware first: `kind create cluster --name scratch`, run the whole drill there with names adjusted (`scratch-control-plane`), then `kind delete cluster --name scratch`. Recommended if etcd makes you nervous — that's the point of practicing.

etcdctl ships inside the etcd image, and the pod already mounts the certs and the data dir, so exec is the easy path. Save the snapshot **into** `/var/lib/etcd` — it's hostPath-backed, so it survives pod restarts and is reachable via docker cp:

```sh
kubectl -n kube-system exec etcd-course-control-plane -- etcdctl \
  --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save /var/lib/etcd/snap-day16.db

kubectl -n kube-system exec etcd-course-control-plane -- etcdutl \
  snapshot status /var/lib/etcd/snap-day16.db -w table

docker cp course-control-plane:/var/lib/etcd/snap-day16.db ./snap-day16.db   # off-node copy, like a real backup
```

### 4. Create evidence that will be destroyed

```sh
kubectl create deployment marker --image=busybox -- sleep 3600
kubectl get deployment marker     # exists, 1/1 — and is NOT in the snapshot
```

### 5. Restore — and watch the marker vanish

**Order matters: restore to a new dir first, repoint the manifest second.**

```sh
# 5a. materialize the snapshot into a NEW data dir (inside the etcd container,
#     /var/lib/etcd is the hostPath — so this lands on the node at /var/lib/etcd/restored)
kubectl -n kube-system exec etcd-course-control-plane -- etcdutl \
  snapshot restore /var/lib/etcd/snap-day16.db --data-dir /var/lib/etcd/restored

# 5b. repoint the static pod's hostPath at the restored dir.
#     Only ONE line in etcd.yaml matches "path: /var/lib/etcd" — the data volume.
docker exec course-control-plane sed -i \
  's|path: /var/lib/etcd$|path: /var/lib/etcd/restored|' \
  /etc/kubernetes/manifests/etcd.yaml
```

The kubelet sees the manifest change and recreates etcd on the restored data (mountPath and `--data-dir` still say `/var/lib/etcd` *inside* the container; only the hostPath behind it changed). The API server drops its connection and reconnects; give it ~30–60s, and if kubectl is cranky, bounce the apiserver the static-pod way:

```sh
docker exec course-control-plane sh -c \
  'mv /etc/kubernetes/manifests/kube-apiserver.yaml /tmp/ && sleep 5 && mv /tmp/kube-apiserver.yaml /etc/kubernetes/manifests/'
```

Now the reveal:

```sh
kubectl get deployment marker
# Error from server (NotFound): deployments.apps "marker" not found
kubectl get pods -n guestbook    # but guestbook is intact — it predates the snapshot
```

The cluster has been rolled back in time. The marker deployment doesn't "get deleted" — in the restored state **it never existed**. Everything from before the snapshot (guestbook, Cilium, policies) is exactly as it was. Pause and appreciate how strange and how useful this is.

If you instead see `etcdutl: not found` in 5a, your etcd image predates the split binary — use the deprecated-but-working `etcdctl snapshot restore` with the same arguments.

## Verify ✅

- [ ] `docker exec course-control-plane ls /etc/kubernetes/manifests` lists exactly the four control-plane manifests
- [ ] With the scheduler manifest moved away: new pods stick at `Pending` and `kubectl get events --field-selector involvedObject.name=orphan` shows **no** Scheduled event; after restoring the manifest, the pod runs
- [ ] `docker exec course-control-plane kubeadm certs check-expiration` shows all certs with ~1y residual validity, none expired
- [ ] `etcdutl snapshot status` on your snapshot prints a table with a non-zero key count
- [ ] `ls -lh snap-day16.db` — the snapshot exists *off the node* (several MB)
- [ ] After restore: `kubectl get deployment marker` → `NotFound`, while `kubectl get pods -n guestbook` shows the full Day 15 stack and `kubectl get nodes` shows 3 Ready nodes

## CKA corner 🎓

This day *is* the CKA corner. The etcd drill above mirrors the exam task almost exactly — differences to expect: you'll be SSH'd into the node itself (no `docker exec`), etcdctl/etcdutl are preinstalled on the host, and the snapshot path is dictated (e.g. `/var/lib/backup/etcd-snapshot.db`). The four cert/endpoint flags are usually *given* in the question — your job is mechanical accuracy under time pressure.

**Drill 1 — snapshot save, exam phrasing (5 min).** "Save a snapshot of the etcd instance at `https://127.0.0.1:2379` to `/var/lib/backup/etcd-snapshot.db`. The CA cert, client cert and key are in `/etc/kubernetes/pki/etcd/`."

<details><summary>Solution</summary>

```sh
ETCDCTL_API=3 etcdctl \
  --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/server.crt \
  --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save /var/lib/backup/etcd-snapshot.db
```
(`ETCDCTL_API=3` is redundant on modern etcd but harmless — cheap insurance. Verify with `etcdutl snapshot status /var/lib/backup/etcd-snapshot.db`.)
</details>

**Drill 2 — restore, exam phrasing (8 min).** "Restore the cluster from the snapshot at `/var/lib/backup/etcd-snapshot-previous.db`." List every step, in order, including how etcd picks up the new data dir on a kubeadm cluster.

<details><summary>Solution</summary>

```sh
# 1. restore into a NEW directory (never the live one):
etcdutl snapshot restore /var/lib/backup/etcd-snapshot-previous.db \
  --data-dir /var/lib/etcd-restored
# 2. point the static pod at it:
vi /etc/kubernetes/manifests/etcd.yaml
#    → volumes: hostPath: path: /var/lib/etcd  →  /var/lib/etcd-restored
# 3. wait; kubelet restarts etcd automatically (watch: crictl ps | grep etcd)
# 4. verify: kubectl get nodes / get pods -A reflect the snapshot-time state
```
Mistakes that cost points: restoring over the live data dir; editing `--data-dir` but not the hostPath (or vice versa — on the exam edit the **hostPath**, the container path can stay); forgetting that no `kubectl` works while etcd/apiserver bounce — patience, not panic.
</details>

**Drill 3 — symptom → component (5 min).** Cover the right column and diagnose:

| Symptom | Broken component |
|---|---|
| New pods stay `Pending`; `kubectl describe pod` shows **no events at all** | kube-scheduler |
| Deployment created, but **no ReplicaSet/pods** ever appear | kube-controller-manager |
| `kubectl` → `connection refused` on :6443; *existing* pods keep serving | kube-apiserver (or etcd under it) |
| One node `NotReady`; its pods eventually evicted/rescheduled | that node's kubelet |
| Pods Running, but a Service VIP doesn't route on one node | kube-proxy on that node |
| Pods can reach IPs but **names don't resolve** | CoreDNS (or a NetworkPolicy you wrote on Day 15…) |
| Pod stuck `ContainerCreating`, events mention CNI/sandbox | CNI plugin (cilium) on that node |
| apiserver up, but everything mysteriously read-only/erroring on writes | etcd (disk full is the classic) |

The reasoning pattern: *which loop failed to advance the story from step N to N+1?* Replay the apply-trace from Concepts and find the missing transition.

## Stretch goals

- Browse etcd directly: `kubectl -n kube-system exec etcd-course-control-plane -- etcdctl --endpoints=https://127.0.0.1:2379 --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key get /registry/deployments/guestbook/guestbook -w fields | head -30` — your Deployment, as etcd sees it. List all keys: `get / --prefix --keys-only | head -50`.
- Write your own static pod: drop a minimal nginx pod manifest into `/etc/kubernetes/manifests/` on `course-worker` (`docker exec -i course-worker tee ...`), watch the mirror pod `nginx-course-worker` appear; try to `kubectl delete` it, fail, then remove the file.
- Add `--v=5` to the scheduler manifest and skim its logs scoring nodes for a new pod, then revert.

## Cleanup

```sh
kubectl delete deployment marker --ignore-not-found   # no-op expected — it was rolled back away
rm -f snap-day16.db
# the PRE-restore data dir and on-node snapshot are now dead weight:
docker exec course-control-plane rm -rf /var/lib/etcd/member /var/lib/etcd/snap-day16.db
```

Leave the etcd manifest pointing at `/var/lib/etcd/restored` — it *is* the live data dir now and works fine. (Purists may move the data back and re-edit the manifest; nothing in the course needs it.) If you made a `scratch` cluster: `kind delete cluster --name scratch`. The guestbook namespace, Cilium, and policies are untouched and **stay**.
