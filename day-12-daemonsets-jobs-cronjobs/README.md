# Day 12 — DaemonSets, Jobs, and CronJobs

> **Time:** ~2.5 h · **Builds on:** Days 3, 11

## Objectives

- Run exactly one pod per node with a DaemonSet and control which nodes via tolerations
- Drive batch work with Jobs: completions, parallelism, and failure backoff
- Schedule recurring work with CronJobs and reason about concurrency and missed runs
- Create Jobs and CronJobs imperatively at exam speed

## Concepts

Deployments answer "keep N copies running somewhere". Today's three controllers answer different questions.

### DaemonSet: one pod per node

Some software isn't *an app*, it's *node infrastructure*: log shippers reading `/var/log` on every node, monitoring agents exporting node metrics, the CNI plugin itself, storage drivers. The replica count isn't a number you choose — it's "however many nodes exist, now and later". A **DaemonSet** runs exactly one pod on every (eligible) node; add a node and a pod appears, drain a node and its pod goes with it.

You'll meet real ones soon: Cilium runs as a DaemonSet on Day 15, and node-exporter + Promtail do on Day 30. Today you build a toy one to see the mechanics.

Two scheduling details matter:

- **Taints decide eligibility.** Your control-plane node carries `node-role.kubernetes.io/control-plane:NoSchedule`, so a plain DaemonSet lands only on the two workers. Agents that *must* run everywhere (CNI, kube-proxy) ship with broad tolerations — check `kubectl get ds -n kube-system kube-proxy -o yaml` and you'll find them.
- **`updateStrategy`**: `RollingUpdate` (default, replaces pods node-by-node, tunable with `maxUnavailable`) vs `OnDelete` (new pods only when you delete old ones — used when an agent restart is disruptive and you want to control the timing per node).

### Job: run to completion

A Deployment restarts containers forever; a **Job** runs pods until a *success count* is reached, then stops. The dials:

| Field | Meaning |
|---|---|
| `completions` | how many successful pod runs the Job needs in total |
| `parallelism` | how many pods may run at once |
| `backoffLimit` | how many *failures* before the Job gives up (default 6) |
| `activeDeadlineSeconds` | wall-clock kill switch for the whole Job, retries included |
| `restartPolicy` | `Never` or `OnFailure` — `Always` is illegal in a Job |

`completions: 5, parallelism: 2` gives you a work queue: 2 pods at a time until 5 succeed.

`restartPolicy` is subtle and exam-relevant. With **`OnFailure`** the *kubelet restarts the container in the same pod* — you see `RESTARTS` climb, and the pod's previous logs vanish unless you use `kubectl logs --previous`. With **`Never`** each failure produces a *new pod*, so failed pods pile up in `Error` state with their logs intact — far easier to debug, at the cost of pod litter. Either way, failures count against `backoffLimit`, and retries are spaced with exponential backoff (10s, 20s, 40s… capped at 6m) — that growing delay is what you'll watch in the lab.

### CronJob: Jobs on a schedule

A **CronJob** is a Job factory with a five-field cron schedule (`min hour day-of-month month day-of-week`; `*/5 * * * *` = every 5 minutes). The interesting fields are about *what happens when reality misbehaves*:

- **`concurrencyPolicy`** — what if the previous run is still going when the next is due? `Allow` (default: run both), `Forbid` (skip the new one), `Replace` (kill the old, start the new). A backup job wants `Forbid`; a cache-refresh wants `Replace`.
- **`startingDeadlineSeconds`** — how late a run may start and still count. If the controller was down across a scheduled time and the deadline passed, the run is skipped. After ~100 consecutive missed schedules with no deadline set, the CronJob stops scheduling entirely and logs an error — set a deadline on anything important.
- **`successfulJobsHistoryLimit` / `failedJobsHistoryLimit`** (defaults 3 / 1) — how many finished Jobs to keep around for inspection.
- **`suspend: true`** — pause scheduling without deleting anything. The standard "stop the bleeding" move during incidents.

```
CronJob ──creates──▶ Job ──creates──▶ Pod(s)
 (schedule)           (completions/backoff)   (the actual work)
```

Debugging always walks that chain: cronjob → `kubectl get jobs` → `kubectl logs job/<name>`.

## Lab

### 1. DaemonSet: a toy node agent

Work in a fresh namespace:

```sh
kubectl create namespace batch
```

Write `node-agent.yaml`: a DaemonSet `node-agent` in `batch`. Requirements:

- image `busybox`, mounts the node's `/var/log/pods` (hostPath, readOnly) at `/host/log/pods`
- command: a loop that every 30s prints the hostname and how many pod-log directories it sees (a fake log shipper "tailing" the node)
- no toleration yet

<details><summary>Solution</summary>

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-agent
  namespace: batch
spec:
  selector:
    matchLabels:
      app: node-agent
  template:
    metadata:
      labels:
        app: node-agent
    spec:
      containers:
      - name: agent
        image: busybox
        command:
        - sh
        - -c
        - 'while true; do echo "$(date) $(hostname): shipping logs for $(ls /host/log/pods | wc -l) pods"; sleep 30; done'
        volumeMounts:
        - name: pod-logs
          mountPath: /host/log/pods
          readOnly: true
      volumes:
      - name: pod-logs
        hostPath:
          path: /var/log/pods
```

</details>

```sh
kubectl apply -f node-agent.yaml
kubectl get pods -n batch -o wide
```

**Two** pods — one per worker, none on `course-control-plane`. Why? Read the taint:

```sh
kubectl describe node course-control-plane | grep -A2 Taints
```

`node-role.kubernetes.io/control-plane:NoSchedule`. Now make the agent run there too — add to the pod template spec:

```yaml
      tolerations:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
```

```sh
kubectl apply -f node-agent.yaml
kubectl get pods -n batch -o wide    # now three, one per node
stern -n batch node-agent            # all three agents reporting
```

Check the update strategy and compare with a system DaemonSet:

```sh
kubectl get ds node-agent -n batch -o jsonpath='{.spec.updateStrategy}'; echo
kubectl get ds kube-proxy -n kube-system -o jsonpath='{.spec.template.spec.tolerations}' | python3 -m json.tool
```

### 2. Job: parallel work

Write `pi-batch.yaml`: a Job `pi-batch` in `batch`, **5 completions, parallelism 2**, `restartPolicy: Never`, image `busybox`, each pod sleeps 10s then echoes a result.

<details><summary>Solution</summary>

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: pi-batch
  namespace: batch
spec:
  completions: 5
  parallelism: 2
  backoffLimit: 4
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: worker
        image: busybox
        command: ["sh", "-c", "echo working...; sleep 10; echo done on $(hostname)"]
```

</details>

```sh
kubectl apply -f pi-batch.yaml
kubectl get pods -n batch -w
```

Watch the rhythm: 2 pods running, one finishes, a third starts — never more than 2 in flight — until 5 show `Completed`. Then:

```sh
kubectl get job pi-batch -n batch    # COMPLETIONS 5/5
kubectl logs -n batch job/pi-batch   # logs from one of the pods
```

### 3. Job: watch failure backoff

Boilerplate, inline — `doomed.yaml`:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: doomed
  namespace: batch
spec:
  backoffLimit: 3
  activeDeadlineSeconds: 300
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: fail
        image: busybox
        command: ["sh", "-c", "echo attempting...; exit 1"]
```

```sh
kubectl apply -f doomed.yaml
kubectl get pods -n batch -w
```

New `Error` pods appear with **growing gaps** (≈10s, 20s, 40s) — exponential backoff in action, and each failed pod sticks around because `restartPolicy: Never`. After 4 failed pods (initial + 3 retries):

```sh
kubectl describe job doomed -n batch | grep -A5 Conditions
```

`Failed ... BackoffLimitExceeded`. Now edit the command to `exit 0` and re-apply — rejected! Job templates are immutable; delete and recreate is the workflow. Try the same Job with `restartPolicy: OnFailure` if you're curious: one pod, climbing `RESTARTS`, `CrashLoopBackOff` look — same backoff, different packaging.

### 4. CronJob: a poor man's healthcheck for guestbook

Every minute, curl the guestbook API from Day 11 (cross-namespace DNS: service `guestbook` in namespace `guestbook` = `guestbook.guestbook.svc`). Write `guestbook-check.yaml`: CronJob `guestbook-check` in `batch`, schedule `* * * * *`, `concurrencyPolicy: Forbid`, `startingDeadlineSeconds: 30`, history limits 3/3, image `curlimages/curl`, command curl `-fsS http://guestbook.guestbook.svc/entries`.

<details><summary>Solution</summary>

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: guestbook-check
  namespace: batch
spec:
  schedule: "* * * * *"
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 30
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 1
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: check
            image: curlimages/curl
            command: ["curl", "-fsS", "http://guestbook.guestbook.svc/entries"]
```

</details>

```sh
kubectl apply -f guestbook-check.yaml
kubectl get cronjob,jobs -n batch -w   # within a minute: a job, then Completed
kubectl logs -n batch -l job-name --tail=5 --prefix 2>/dev/null || \
  kubectl logs -n batch job/$(kubectl get jobs -n batch -o jsonpath='{.items[-1:].metadata.name}')
```

You should see the JSON entries from Day 11 — your healthcheck proves the whole stack end-to-end. Break it on purpose: `kubectl scale sts guestbook-db -n guestbook --replicas=0`, wait a minute, and the next job **fails** (`curl -f` turns the 503 into exit code 22). Scale the db back to 1. Old jobs roll off thanks to the history limits.

Now suspend it:

```sh
kubectl patch cronjob guestbook-check -n batch -p '{"spec":{"suspend":true}}'
kubectl get cronjob -n batch    # SUSPEND: True — no new jobs from now on
```

In k9s: `:cronjobs` lets you trigger (`t`) and suspend (`ctrl-s` on recent versions) without typing any of this — genuinely the fastest way to poke at cron schedules.

## Verify ✅

- [ ] `kubectl get ds node-agent -n batch` → `DESIRED 3, READY 3` (after the toleration)
- [ ] `kubectl get pods -n batch -l app=node-agent -o wide` → one pod on each of the 3 nodes
- [ ] `kubectl get job pi-batch -n batch` → `COMPLETIONS 5/5`; during the run, never more than 2 pods `Running` at once
- [ ] `kubectl get job doomed -n batch -o jsonpath='{.status.conditions[?(@.type=="Failed")].reason}'` → `BackoffLimitExceeded`, with 4 `Error` pods
- [ ] `kubectl get cronjob guestbook-check -n batch` shows `LAST SCHEDULE` under a minute old (before suspending), and a completed job whose logs contain guestbook JSON
- [ ] After the patch: `kubectl get cronjob guestbook-check -n batch -o jsonpath='{.spec.suspend}'` → `true`

## CKA corner 🎓

Jobs/CronJobs are pure speed points — the imperative commands write the YAML for you.

```sh
kubectl create job NAME --image=IMG -- cmd args            # one-shot job
kubectl create cronjob NAME --image=IMG --schedule="*/5 * * * *" -- cmd
kubectl create job NAME --from=cronjob/CJNAME              # trigger a cronjob NOW
```

That last one is a known exam task ("manually trigger the cronjob...") and also your real-world move for "re-run last night's backup right now". Need `completions`/`parallelism`? Generate and edit: `kubectl create job ... --dry-run=client -o yaml > j.yaml`.

**Drill 1 (3 min).** Create a Job `busybox-date` that runs `date` in busybox, then verify it completed and read its output. Imperative only.

<details><summary>Solution</summary>

```sh
kubectl create job busybox-date --image=busybox -- date
kubectl wait --for=condition=complete job/busybox-date --timeout=60s
kubectl logs job/busybox-date
```
</details>

**Drill 2 (5 min).** Create a CronJob `ping-dns` running `nslookup kubernetes.default` in busybox every 2 minutes; it must never run concurrently and must give up if it can't start within 20s of schedule. Then trigger one run immediately without waiting.

<details><summary>Solution</summary>

```sh
kubectl create cronjob ping-dns --image=busybox --schedule="*/2 * * * *" \
  --dry-run=client -o yaml -- nslookup kubernetes.default > cj.yaml
# edit cj.yaml: add under spec:
#   concurrencyPolicy: Forbid
#   startingDeadlineSeconds: 20
kubectl apply -f cj.yaml
kubectl create job ping-now --from=cronjob/ping-dns
kubectl logs job/ping-now
```
</details>

**Drill 3 (4 min).** A Job must run a flaky task: tolerate up to 5 failures, run at most 3 pods in parallel, need 6 successes, and be killed wholesale after 2 minutes. Write the four spec fields from memory.

<details><summary>Solution</summary>

```yaml
spec:
  backoffLimit: 5
  parallelism: 3
  completions: 6
  activeDeadlineSeconds: 120
```
Remember: `activeDeadlineSeconds` trumps `backoffLimit` — whichever hits first fails the Job.
</details>

## Stretch goals

- Set `updateStrategy: {type: OnDelete}` on `node-agent`, change the echo message, apply — nothing rolls. Delete one pod, see only that node get the new version. The manual-canary pattern for risky agents.
- Indexed Jobs: add `completionMode: Indexed` to a 5-completion Job and have each pod echo `$JOB_COMPLETION_INDEX` — the building block for sharded batch work.
- Add `ttlSecondsAfterFinished: 60` to a Job and watch it self-delete a minute after completing — the fix for Job litter that history limits give CronJobs but bare Jobs lack.

## Cleanup

```sh
kubectl delete namespace batch
```

Everything today was disposable. The `guestbook` namespace from Day 11 **stays untouched** (you only scaled its StatefulSet down and back up — make sure it's back at 1 replica and Ready before you leave).
