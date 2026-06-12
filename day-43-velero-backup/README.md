# Day 43 — Velero: Back It Up, Destroy It, Get It Back

> **Time:** ~3.5 h · **Builds on:** Days 11 (guestbook + Postgres), 16 (etcd snapshots), 28 (sealed-secrets key backup)

## Objectives

- Explain precisely what an etcd snapshot protects and what it cannot — and where Velero fits in the gap.
- Deploy MinIO as an in-cluster S3 target and install Velero with File System Backup (the no-CSI-snapshot path kind forces on you).
- Back up the `guestbook` namespace — **including the Postgres data** — delete the entire namespace, and restore it with the data intact.
- Schedule recurring backups and articulate what Velero does *not* protect.

## Concepts

### Two different things people mean by "backup"

On Day 16 you snapshotted etcd. That captures every **API object** — Deployments, Services, Secrets, PVC *objects* — the cluster's brain. It does **not** capture a single byte of what's *inside* a PersistentVolume. Restore etcd after losing a disk and you get a PVC object pointing at data that no longer exists: the skeleton, not the organs. The inverse failure also matters: a volume-level disk backup without the objects gives you data no workload can mount.

| | etcd snapshot | Velero |
|---|---|---|
| API objects | all of them, whole cluster only | yes — filterable by namespace/label/kind |
| PV data | ❌ | ✅ (CSI snapshots or File System Backup) |
| Granular restore ("just this namespace") | ❌ all-or-nothing | ✅ |
| Restore into a *different* cluster | painful to impossible | ✅ — it's also a migration tool |
| Who runs it | platform admin, control-plane level | platform *and* app teams, API level |

They're complements: etcd snapshots are disaster recovery for the **control plane**; Velero is disaster recovery for **workloads and their data**. The migration point deserves emphasis — backup to object storage + restore into another cluster is how teams move namespaces between clusters, clouds, and (relevant to you) how you'd survive recreating the kind cluster.

### Velero's architecture

```
 velero CLI ──► Backup/Restore CRDs ──► velero server (deployment, ns velero)
                                            │
                     ┌──────────────────────┼─────────────────────┐
                     ▼                      ▼                     ▼
            queries the API           object-store plugin    node-agent DaemonSet
            (which objects match)     (aws/gcp/azure…)       (File System Backup)
                     │                      │                     │
                     └────────► BackupStorageLocation ◄───────────┘
                                (S3 bucket: MinIO today)
```

- **Server**: watches `Backup`/`Restore`/`Schedule` CRDs, collects matching objects as JSON, tars them into the object store.
- **BackupStorageLocation (BSL)**: "where backups live" — any S3-compatible store. Today: MinIO, in-cluster. Production: real S3/GCS, in another region/account (a backup in the failure domain it protects against is a prayer, not a backup).
- **Volume data, two ways**: (a) **CSI snapshots** — ask the storage driver for a block-level snapshot; fast, but kind's `local-path` provisioner can't do it. (b) **File System Backup (FSB)** — a `node-agent` DaemonSet reads the volume's files *from the node* and uploads them (kopia under the hood; this subsystem was historically called "restic", and you'll still see that name in older docs — the restic→kopia transition also renamed install flags, hence the flag-check below). FSB works on any volume, at the cost of being file-level and slower.

### Consistency: the honest part

FSB copies files **while the application runs**. For Postgres that yields *crash consistency*: equivalent to yanking the power cord. Postgres is built to recover from exactly that (WAL replay), so today's restore will work — but "usually recovers, like after a power cut" is the real guarantee, and you should be able to say so precisely. The upgrade is *application consistency* via **backup hooks**: Velero can exec into the pod **pre**-backup (e.g. `pg_dump` to a sidecar-visible file, or an `fsfreeze`) and **post**-backup to thaw. The honest hierarchy for databases in K8s:

1. crash-consistent volume copy (today's default) — fine for labs, risky for prod
2. hooks + `pg_dump`/WAL archiving — solid
3. a database operator (CloudNativePG, etc.) doing continuous archiving + PITR — the production answer; Velero then covers the *rest* of the namespace

## Lab

### 1. Deploy MinIO — the backup target

A backup needs somewhere to live that isn't the thing being backed up. Apply the provided manifest ([`minio.yaml`](minio.yaml) — Namespace, PVC, Deployment, Service, console Ingress; deliberately simple):

```sh
kubectl apply -f minio.yaml
kubectl get pods -n minio -w        # Running, 1/1
```

> Day 42 alumni note: the manifest carries a `team` label and a pinned image tag. Try mentally diffing what your Kyverno policies would have rejected/mutated — then check `kubectl get policyreport -n minio` after a few minutes.

Create the `velero` bucket with a one-shot `mc` (MinIO client) pod:

```sh
kubectl run mc -n minio --rm -i --restart=Never --image=minio/mc --command -- \
  /bin/sh -c "mc alias set local http://minio:9000 minioadmin minioadmin && mc mb local/velero && mc ls local"
```

Browse the console at http://minio.localhost:8080 (minioadmin/minioadmin) — empty bucket `velero` exists. Keep this tab; watching backup objects appear is half the fun.

### 2. Install Velero

```sh
brew install velero
```

Velero authenticates to MinIO with AWS-style credentials. Create `credentials-velero`:

```ini
[default]
aws_access_key_id = minioadmin
aws_secret_access_key = minioadmin
```

Install — every flag here is doing real work, read them before running (flag names current as of Velero 1.13+; the docs at https://velero.io/docs/ are the authority if your version disagrees — these renamed during the restic→kopia transition):

```sh
velero install \
  --provider aws \
  --plugins velero/velero-plugin-for-aws \
  --bucket velero \
  --secret-file ./credentials-velero \
  --backup-location-config region=minio,s3ForcePathStyle="true",s3Url=http://minio.minio:9000 \
  --use-node-agent \
  --default-volumes-to-fs-backup \
  --use-volume-snapshots=false
```

| Flag | Why |
|---|---|
| `--provider aws` + aws plugin | MinIO speaks the S3 API; "aws" here means "S3 protocol", not Amazon |
| `s3ForcePathStyle`, `s3Url` | path-style addressing at an explicit endpoint — required for any non-AWS S3 |
| `--use-node-agent` | deploy the FSB DaemonSet |
| `--default-volumes-to-fs-backup` | back up **every** pod volume via FSB unless opted out — the right default when CSI snapshots don't exist |
| `--use-volume-snapshots=false` | kind's local-path storage has no snapshot support; don't pretend |

```sh
kubectl get pods -n velero          # velero + node-agent on each worker
velero backup-location get          # PHASE: Available  ← if not, fix this before proceeding
```

### 3. Seed data worth saving

Make the guestbook hold something you'd grieve:

```sh
curl -s -X POST http://guestbook.localhost:8080/entries -H 'Content-Type: application/json' -d '{"message":"survive-me-1"}'
curl -s -X POST http://guestbook.localhost:8080/entries -H 'Content-Type: application/json' -d '{"message":"survive-me-2"}'
curl -s -X POST http://guestbook.localhost:8080/entries -H 'Content-Type: application/json' -d '{"message":"if you can read this, Velero worked"}'
curl -s http://guestbook.localhost:8080/entries | python3 -m json.tool
```

Those rows live in Postgres, on a PVC, on a node — *not* in any API object. That's the whole point of today.

### 4. Back up the namespace

```sh
velero backup create guestbook-bk --include-namespaces guestbook
velero backup describe guestbook-bk          # wait for Phase: Completed
velero backup describe guestbook-bk --details | grep -A10 "kopia"
```

In `--details`, find the **Pod Volume Backups** — that's the node-agent shipping the Postgres PVC contents. Then look in the MinIO console: `velero/backups/guestbook-bk/` (object manifests, logs) and a `kopia/` repository (the volume data). Your namespace now exists as files in an object store.

### 5. Disaster

Take a breath and type it:

```sh
kubectl delete namespace guestbook
kubectl get all -n guestbook                              # No resources / NotFound
curl -m3 http://guestbook.localhost:8080/entries          # 503 from nginx
```

StatefulSet, Service, Secrets, NetworkPolicies, PVC — gone. With kind's default StorageClass the PV and its data are reclaimed too. This is the realistic blast radius of a fat-fingered `delete ns` or a bad `prune: true` (Day 27 foreshadowed this).

### 6. Restore

```sh
velero restore create guestbook-restore --from-backup guestbook-bk
velero restore describe guestbook-restore        # → Phase: Completed
kubectl get pods -n guestbook -w                 # postgres-0 Init:0/1 → Running, then the API pods
```

Watch the order of reconstruction: namespace → PVC → StatefulSet → pods. The Postgres pod runs an **init container Velero injected** (`restore-wait`) that blocks until the node-agent finishes pouring the file-system backup back into the fresh PVC. When everything's Ready — the moment this day exists for:

```sh
curl -s http://guestbook.localhost:8080/entries | python3 -m json.tool
```

`"survive-me-1"` is back. Not the Deployment — the **rows**. Objects came from the backup manifests; data came through kopia from MinIO into a brand-new PV. Say the distinction out loud: *etcd-style object backup alone could not have done this.*

### 7. Schedules — backups nobody has to remember

```sh
velero schedule create guestbook-daily --schedule="0 3 * * *" --include-namespaces guestbook --ttl 72h
velero schedule get
```

Cron syntax (Day 12), 3-day retention, each run produces `guestbook-daily-<timestamp>`. Trigger one off-schedule now rather than waiting for 03:00:

```sh
velero backup create --from-schedule guestbook-daily
velero backup get
```

Production discipline: schedules + TTL + an **alert on backup failure** (Velero exports Prometheus metrics — `velero_backup_last_status`; a backup system that fails silently is worse than none, because you've stopped worrying).

### 8. What Velero does NOT protect — write this list down

- **The cluster itself.** No API server, no restore. Cluster bootstrap (kind config, Cilium, ArgoCD) must be reproducible from git — which, after Phase 4, yours is.
- **etcd / control-plane state** — that's Day 16's snapshot territory, complementary not redundant.
- **CRD ordering gotchas.** Restoring a namespace whose resources depend on CRDs (Rollouts, Certificates…) requires the CRDs/controllers to exist first; restore order and `--include-cluster-resources` need thought.
- **Encryption/sealing keys.** Restoring a SealedSecret into a cluster whose sealed-secrets key changed gives you ciphertext nobody can open. Your Day 28 key backup is its own, separate lifeline — Velero backing up the *encrypted* objects doesn't replace it.
- **Anything between backups.** RPO = your schedule interval. Last night's backup loses today's entries; databases that can't tolerate that need WAL archiving, not more Velero.

### 9. Migration teaser: restore into a different namespace

The restore target doesn't have to be the original — this is the migration primitive:

```sh
velero restore create guestbook-clone --from-backup guestbook-bk --namespace-mappings guestbook:guestbook-clone
kubectl get pods -n guestbook-clone
kubectl port-forward -n guestbook-clone svc/guestbook 8082:80 &
curl -s localhost:8082/entries | python3 -m json.tool | grep message ; kill %1
```

Same data, parallel universe. Across *clusters*, the move is identical: point the second cluster's Velero at the same bucket (read-only BSL), `velero restore create`. That sentence is a migration strategy interviewers ask about.

```sh
kubectl delete namespace guestbook-clone     # clone proved its point
```

## Verify ✅

- [ ] `velero backup-location get` → `PHASE Available`
- [ ] `velero backup get` → `guestbook-bk` `STATUS Completed`; MinIO console shows `backups/guestbook-bk/` and a `kopia/` prefix in the bucket
- [ ] `velero backup describe guestbook-bk --details` lists Pod Volume Backups including the Postgres data volume
- [ ] After the delete: `kubectl get ns guestbook` → `NotFound`; after restore: all guestbook pods `Running`
- [ ] `curl -s http://guestbook.localhost:8080/entries` → contains `"survive-me-1"`, `"survive-me-2"`, and the third message
- [ ] `velero schedule get` → `guestbook-daily`, `SCHEDULE 0 3 * * *`, and a `--from-schedule` backup completed

## Interview corner 💬

**"What's the DR story for your cluster?"**
Layered, by failure domain. Workloads + data: Velero on a schedule to object storage *outside* the cluster, with File System Backup (or CSI snapshots where the driver supports them) for PV data, TTL-based retention, and alerts on failed backups. Control plane: etcd snapshots — separate concern, separate tool. Cluster definition: everything is GitOps, so the cluster itself is rebuildable from the repo, and DR for stateless services is "recreate and sync". Keys and credentials that *unlock* backups — sealed-secrets keys, KMS access — are escrowed independently, because a backup you can't decrypt is decoration. And it's only DR if it's been *restored* recently: we drill the restore path, not just the backup path.

**"etcd snapshot vs Velero — when do you use which?"**
etcd snapshots capture the entire API state atomically — the right tool when the control plane itself dies or etcd corrupts, but it's all-or-nothing, same-cluster, and contains zero volume data. Velero works at the API level: selective by namespace/label, includes PV data, restores into different namespaces or different clusters entirely — so it covers "team deleted their namespace", "migrate this app to the new cluster", and "ransomware'd PVs", none of which etcd snapshots address. Production runs both; what changes is which one you reach for during which incident.

**"How do you back up Postgres running in Kubernetes — properly?"**
Honest layers. A Velero file-system backup of the data directory is crash-consistent — Postgres will usually WAL-replay its way back, but that's a power-cord guarantee, not transactional. Better: Velero pre-backup hooks running `pg_dump` (application-consistent logical dump) or fsfreeze around the copy. The production answer is an operator like CloudNativePG doing continuous WAL archiving to object storage with point-in-time recovery — RPO of seconds instead of "since last backup" — and Velero still backing up the namespace's *objects* around it. The pattern: the database's own consistency machinery does data; Velero does Kubernetes.

## Stretch goals

- Add an application-consistency **hook**: annotate the Postgres pod template with `pre.hook.backup.velero.io/command: '["pg_dump", "-U", "guestbook", "-f", "/var/lib/postgresql/data/dump.sql", "guestbook"]'` (adjust user/db to your Day 11 setup), re-backup, and find the dump file in the restored volume.
- Exclude a volume from FSB with the `backup.velero.io/backup-volumes-excludes` pod annotation — the opt-out half of `--default-volumes-to-fs-backup`.
- Restore *selectively*: `velero restore create --from-backup guestbook-bk --include-resources persistentvolumeclaims,secrets` and reason about what's usable.
- Break the BSL on purpose (scale MinIO to 0, run a backup) — watch the failure mode and where it's reported; this is what your backup alert must catch.
- Compare: `velero backup create guestbook-nodata --include-namespaces guestbook --snapshot-volumes=false ...` sized against `guestbook-bk` in MinIO — objects are kilobytes; data is the backup.

## Cleanup

Backups live in MinIO's PVC — they survive anything except deleting that PVC.

**Recommended: keep Velero + MinIO** through Day 44 and the capstone (a working backup during a troubleshooting gauntlet is also just… wise). The schedule is the only thing that acts on its own; remove it if the daily churn bothers you:

```sh
velero schedule delete guestbook-daily --confirm
```

If you must reclaim resources instead: `velero uninstall` and `kubectl delete ns minio` (this deletes the backups with the PVC — knowingly). Keep `credentials-velero` out of git either way.
