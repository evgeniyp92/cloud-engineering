# Day 10 — Probes & Graceful Shutdown

> **Time:** ~3.5 h · **Builds on:** Days 3, 4, 8

## Objectives

- Configure startup, readiness, and liveness probes and articulate each one's distinct job and failure consequence.
- Show a live-but-unready pod leaving EndpointSlices with zero failed client requests — and *not* restarting.
- Show a liveness failure causing a kubelet restart, on your schedule.
- Build the zero-downtime rolling-update recipe (probes + preStop + SIGTERM handling) and prove it with a request loop that records no errors.

## Concepts

### Three probes, three different questions

The kubelet can keep asking your container questions. Same mechanisms (HTTP GET expecting 2xx/3xx, TCP connect, exec command, gRPC), but the *consequence* of failure differs — and confusing them causes outages:

| Probe | Question | On failure | Wrong answer costs you |
|---|---|---|---|
| **startup** | "Are you done booting?" | keep waiting (other probes disabled); kill after `failureThreshold` | slow apps murdered at boot |
| **readiness** | "Can you take traffic *right now*?" | **remove from Service endpoints** — no restart | traffic to a pod that can't serve it |
| **liveness** | "Are you alive at all?" | **kill & restart the container** | restart loops that fix nothing |

The two rules that prevent most probe-related incidents:

1. **Readiness is recoverable; liveness is capital punishment.** A pod that's briefly overwhelmed, or waiting on a dependency, should go *unready* — not be executed. Liveness is only for states a restart actually cures: deadlocks, wedged event loops. Pointing liveness at a handler that checks a database is the classic self-inflicted outage: DB blips → every pod restarts simultaneously → real outage.
2. **They run independently, forever, in parallel.** Liveness does not wait for readiness. A slow-booting app fails liveness before it's up — that's what startupProbe exists for: it gates the other two until first success.

### Tuning fields

Every probe takes the same knobs:

```
initialDelaySeconds: 0    # wait before first probe
periodSeconds:       10   # how often
timeoutSeconds:      1    # per-attempt timeout
failureThreshold:    3    # consecutive failures to trip
successThreshold:    1    # consecutive successes to untrip (readiness only may be >1)
```

Time-to-consequence ≈ `period × failureThreshold` (+ up to one period). Readiness should trip *fast* (small period, low threshold — seconds matter when a pod goes bad); liveness should trip *slow* (generous threshold — restarts are expensive and cascade). `startupProbe: failureThreshold: 30, periodSeconds: 2` = "up to 60 s to boot" without loosening the steady-state probes.

### Shutdown: the endpoint-propagation race

What happens when a pod is deleted (rollout, scale-down, drain):

```
t=0  pod marked Terminating
      ├─▶ kubelet sends SIGTERM to the container        (path A)
      └─▶ endpoint controllers remove pod from slices,
          kube-proxy on EVERY node updates iptables      (path B)
t=grace  kubelet sends SIGKILL if still alive
```

Paths A and B run **in parallel — nothing orders them**. For a few hundred milliseconds (or seconds, on busy clusters), nodes still route *new* connections to a pod that already got SIGTERM. If the app dies instantly on SIGTERM, those requests get connection-refused. That's the race behind "we see a small error blip on every deploy".

The standard fix is embarrassingly simple — **wait before dying**:

- a `preStop` hook that sleeps a few seconds: SIGTERM is *delayed* until the hook finishes, so the pod keeps serving while path B propagates;
- then the app handles SIGTERM properly: stop accepting, finish in-flight requests, exit (podlab does — you'll watch it log it);
- `terminationGracePeriodSeconds` (default 30) must cover `preStop + drain time`, because SIGKILL at the deadline is unconditional.

Since the pod is already out of (most) endpoints by the time SIGTERM arrives, "draining" is usually just finishing stragglers. Note `preStop` traditionally meant `exec: sh -c "sleep 5"` — useless in a distroless image with no shell. Kubernetes now has a native sleep action: `preStop: {sleep: {seconds: 5}}` — no shell required.

### The zero-downtime checklist (Days 3 + 10 combined)

1. ≥2 replicas; rolling update with `maxUnavailable: 0` (capacity never dips)
2. **readiness probe** — new pods get traffic only when truly ready; rollout waits for it
3. **preStop sleep** — old pods leave endpoints *before* they stop serving
4. app handles **SIGTERM** with in-flight draining
5. `terminationGracePeriodSeconds` > preStop + worst-case drain
6. (liveness tuned conservatively, startup probe if boot is slow)

Miss any one and a rolling deploy can drop requests. Today you assemble all of it and prove zero errors under continuous load.

## Lab

### 1. Probe the deployment

podlab's `/healthz` returns 200, and 503 after `POST /healthz/toggle` — a probe target you can break on command. Requirements — update `deploy.yaml` (keep Day 8's resources, Day 9's labels):

- `replicas: 2`
- readiness: HTTP GET `/healthz` port 8080, `periodSeconds: 2`, `failureThreshold: 2` (trips in ~4 s)
- liveness: same endpoint, `periodSeconds: 5`, `failureThreshold: 6` (trips in ~30 s — deliberately slower than readiness)
- startup: same endpoint, `periodSeconds: 2`, `failureThreshold: 15`

<details><summary>Solution (container excerpt)</summary>

```yaml
          startupProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 2
            failureThreshold: 15
          readinessProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 2
            failureThreshold: 2
          livenessProbe:
            httpGet: {path: /healthz, port: 8080}
            periodSeconds: 5
            failureThreshold: 6
```

</details>

```sh
kubectl apply -f deploy.yaml && kubectl rollout status deployment/podlab
kubectl describe pod -l app=podlab | grep -E 'Liveness|Readiness|Startup' | head -3
```

### 2. Start the truth-tellers

Terminal 2 — a relentless client (in-cluster, against the Service, so endpoints actually matter — Day 3 explained why port-forward can't show this):

```sh
kubectl run loop --rm -it --image=curlimages/curl --restart=Never -- \
  sh -c 'while true; do curl -s -o /dev/null --max-time 2 -w "%{http_code} " podlab/; sleep 0.2; done'
```

A stream of `200 200 200 …`. Any non-200 from here on is a lost request — keep this running through the whole lab. Terminal 3:

```sh
stern podlab --include 'health|draining|shutdown'
```

### 3. Readiness failure: leave the pool, don't die

Pick ONE pod and port-forward **to the pod, not the service** (the toggle must hit a specific victim):

```sh
VICTIM=$(kubectl get pods -l app=podlab -o jsonpath='{.items[0].metadata.name}')
kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide   # note: 2 addresses
kubectl port-forward pod/$VICTIM 8089:8080 &
curl -s -X POST localhost:8089/healthz/toggle
```

Within ~4 s, watch three things at once:

```sh
kubectl get pods -l app=podlab            # VICTIM: READY 0/1 — but STATUS Running, RESTARTS 0
kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide   # one address GONE
kubectl describe pod $VICTIM | tail -5    # Warning  Unhealthy ... Readiness probe failed: HTTP 503
```

And the most important non-event: **terminal 2 never printed anything but 200**. The service stopped routing to the sick pod the moment readiness tripped; the healthy replica absorbed everything. The pod was *not* restarted — readiness never restarts. Now heal it **before liveness's 30 s budget runs out**:

```sh
curl -s -X POST localhost:8089/healthz/toggle
kubectl get pods -l app=podlab                  # READY 1/1 again, RESTARTS still 0
kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide   # both addresses back
```

Unready ≠ dead. The pod sat there Running the whole time — exactly what you want during a dependency blip.

### 4. Liveness failure: the kubelet's restart

Same toggle — but this time let it ride past liveness's `5 s × 6` budget:

```sh
curl -s -X POST localhost:8089/healthz/toggle
kubectl get pods -l app=podlab --watch
```

Timeline you'll observe: ~4 s → `READY 0/1` (readiness, again first); ~30–35 s → `RESTARTS 1` as the kubelet kills and recreates the container. podlab restarts healthy (the toggle was in-memory), startup probe passes, readiness passes, back to `READY 1/1`, back into the endpoints. Terminal 2: still unbroken 200s — readiness had *already* pulled the pod before the restart, which is exactly why readiness must trip faster than liveness. Forensics:

```sh
kubectl describe pod $VICTIM | grep -B2 -A6 'Liveness probe failed'
# Warning  Unhealthy  Liveness probe failed: HTTP probe failed with statuscode: 503
# Normal   Killing    Container podlab failed liveness probe, will be restarted
kill %1   # the port-forward died with the container anyway
```

### 5. Graceful shutdown: preStop + SIGTERM

Add to the container spec (sibling of `image:`) and set the grace period (sibling of `containers:`):

```yaml
          lifecycle:
            preStop:
              sleep:
                seconds: 5
```

```yaml
      terminationGracePeriodSeconds: 30
```

(`exec`+`sh -c sleep` is the pattern you'll see in older manifests; podlab is distroless — no shell — so the native sleep action is also the only one that works here.)

```sh
kubectl apply -f deploy.yaml
```

Watch terminal 3 (stern) during the rollout this apply just triggered: each old pod, ~5 s *after* termination begins (the preStop sleep), logs:

```
"signal received, draining connections"
"shutdown complete"
```

That gap between "Terminating" appearing in `kubectl get pods` and the SIGTERM log line *is* the preStop hook buying time for endpoint propagation.

### 6. The final exam: zero-downtime rolling restart

Make sure the strategy is `maxSurge: 1, maxUnavailable: 0` (Day 3), terminal 2's loop is running, then restart everything and count errors:

```sh
kubectl rollout restart deployment/podlab
kubectl rollout status deployment/podlab
```

Terminal 2: an unbroken wall of `200` while every single pod was replaced. Stop the loop (Ctrl-C) and read its output one last time — any `000` (timeout) or `5xx` means a checklist item is missing. Then break it on purpose to feel the difference: remove the readiness probe *and* the preStop block, apply, `rollout restart` again — the loop now shows `000`s/gaps during the rollout. Restore both, apply, re-verify. Now you've *seen* what each safeguard buys.

## Verify ✅

- [ ] Step 3: `kubectl get pod $VICTIM` showed `READY 0/1` with `RESTARTS 0`, and the EndpointSlice dropped to one address while the curl loop stayed all-200
- [ ] Step 3 recovery: both addresses back in `kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide`
- [ ] Step 4: `kubectl get pod $VICTIM -o jsonpath='{.status.containerStatuses[0].restartCount}'` ≥ 1, and describe shows `failed liveness probe, will be restarted`
- [ ] Step 5: stern showed `signal received, draining connections` then `shutdown complete` for each replaced pod
- [ ] Step 6: a full `rollout restart` with the curl loop printing **zero** non-200 codes
- [ ] `kubectl get deploy podlab -o jsonpath='{.spec.template.spec.containers[0].lifecycle.preStop}'` → the sleep action

## CKA corner 🎓

Exam notes:

- Probe YAML must flow from your fingers: `livenessProbe.httpGet.{path,port}`, `readinessProbe`, plus `initialDelaySeconds/periodSeconds/failureThreshold`. `kubectl explain pod.spec.containers.livenessProbe.httpGet` if stuck.
- Probes are **per-container**, under `containers[]` — a chronically misplaced block.
- Symptom mapping for troubleshooting questions: high `RESTARTS` + `Unhealthy` events = liveness; `READY 0/1` + Running + no restarts = readiness; pod killed during boot = startup/initialDelay too tight.
- exec probes: `exec.command: ["cat", "/tmp/healthy"]` — the exam loves this with busybox.

**Drill 1 (4 min):** Pod `flaky` (busybox): creates `/tmp/healthy`, sleeps 30, deletes it, sleeps 600. Liveness: exec `cat /tmp/healthy`, period 5 s, failureThreshold 1. Predict, then verify, the restart behavior.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata: {name: flaky}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sh", "-c", "touch /tmp/healthy; sleep 30; rm /tmp/healthy; sleep 600"]
      livenessProbe:
        exec:
          command: ["cat", "/tmp/healthy"]
        periodSeconds: 5
        failureThreshold: 1
```

Prediction: healthy ~30 s, then the first failed probe (threshold 1) restarts it; the restart recreates the file → cycle repeats ≈ every 35–40 s. `kubectl get pod flaky --watch` confirms RESTARTS climbing. `kubectl delete pod flaky`.

</details>

**Drill 2 (3 min):** Deployment `slowboot` (nginx) must tolerate 90 s of boot time without restarts, while liveness in steady state checks `/` every 10 s with threshold 3. Add the probes; prove the startup probe gates liveness by reading `describe` events.

<details><summary>Solution</summary>

```yaml
          startupProbe:
            httpGet: {path: /, port: 80}
            periodSeconds: 5
            failureThreshold: 18      # 18 × 5 = 90 s budget
          livenessProbe:
            httpGet: {path: /, port: 80}
            periodSeconds: 10
            failureThreshold: 3
```

`kubectl create deployment slowboot --image=nginx --dry-run=client -o yaml`, add probes, apply. Events show startup probe activity first; no liveness events until startup succeeds. `kubectl delete deploy slowboot`.

</details>

## Stretch goals

- Measure the race you just defended against: remove only `preStop`, run the loop with `-w "%{http_code}\n"` piped to a file during a restart, and count non-200s; restore preStop and show the count at 0. Quantified preStop value.
- Add `minReadySeconds: 10` to the Deployment and watch `rollout status` slow down — what extra guarantee does it buy over the readiness probe alone?
- Try `successThreshold: 3` on readiness — a flapping pod must now prove itself 3 times before rejoining.
- gRPC probes exist (`grpc: {port: 9090}`) — read `kubectl explain pod.spec.containers.livenessProbe.grpc` for when you meet a gRPC service.

## Cleanup

```sh
kubectl delete pod loop flaky --ignore-not-found
```

**Keep everything**: the `podlab` Deployment — now with recommended labels, resources, all three probes, and graceful shutdown — plus its Service, ingress-nginx, and metrics-server. This is the production-shaped baseline Phase 2 (Day 11: StatefulSets & guestbook) builds on. Phase 1 complete.
