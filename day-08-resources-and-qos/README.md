# Day 08 — Resources & QoS

> **Time:** ~3 h · **Builds on:** Days 2, 3

## Objectives

- Explain what requests do (scheduling) vs what limits do (enforcement), and why CPU and memory behave differently at the limit.
- Place pods into all three QoS classes on purpose and predict eviction order from them.
- Install metrics-server on kind and read `kubectl top`.
- Reproduce CPU throttling and an OOMKill on demand, and read both from the API.

## Concepts

### Requests are promises to the scheduler; limits are walls

Two numbers per resource, per container, doing unrelated jobs:

- **`requests`** — used **only at scheduling time**. The scheduler does bookkeeping math: a node "fits" if the sum of requests of pods on it + this pod's requests ≤ the node's allocatable. It is a *reservation on paper*, not a measurement — a pod requesting 1 CPU and using 5m still blocks that 1 CPU from other pods' math. Requests also set proportional CPU weight under contention.
- **`limits`** — enforced **at runtime by the kernel** (cgroups), never consulted by the scheduler. Cross the wall and the kernel reacts.

```
            scheduling           runtime
requests ──▶ "where does it fit?"     (cpu weight under contention)
limits   ──▶ (ignored)            ──▶ cgroup enforcement
```

The sum of *limits* on a node may exceed the node — that's **overcommit**, and it's normal: requests = what you're guaranteed; limits = how far you may burst.

### CPU is compressible; memory is not

What "hitting the limit" means differs completely by resource, and this asymmetry drives every sizing decision:

| | CPU | Memory |
|---|---|---|
| Nature | compressible — can be taken away and given back | incompressible — allocated bytes can't be "slowed down" |
| At the limit | **throttling**: the cgroup gets its quota per 100 ms period and then waits; app gets slow, latency spikes | **OOMKill**: the kernel kills the container (exit code 137, reason `OOMKilled`); kubelet restarts it per restartPolicy |
| Failure mode | invisible latency | visible crash |

Hence the widely used rule of thumb: **always set memory limits** (and usually limit = request, so the wall equals the promise), but **think twice about CPU limits** — many teams set CPU requests only, accepting throttle-free bursting; others keep CPU limits for predictability. Know the trade-off, and know that mysterious p99 latency with low average CPU often turns out to be throttling.

`cpu: 100m` = 100 millicores = 0.1 CPU. `memory: 64Mi` = mebibytes (`Mi`, binary) — note `64M` (decimal) is legal and different; a classic typo is `64m` which means 0.064 *bytes*-ish nonsense the API happily accepts.

### QoS classes: who dies first

From requests/limits, Kubernetes derives a per-pod **QoS class** — its life-insurance tier when a *node* runs out of memory (node-pressure eviction; the kubelet picks victims considering QoS and how far pods are over their requests):

| Class | Rule | Eviction priority |
|---|---|---|
| `Guaranteed` | every container has limits = requests for **both** cpu & memory | last to die |
| `Burstable` | at least one request or limit set, but not Guaranteed | middle |
| `BestEffort` | no requests, no limits anywhere | first to die |

Everything you've deployed so far (no resources block!) has been BestEffort — fine for a course cluster, malpractice in production. From Day 9's LimitRange onward, defaults will backstop you.

### metrics-server: making `kubectl top` work

The kubelet measures actual usage, but nothing serves it through the API until **metrics-server** aggregates kubelet stats and exposes the Metrics API (`kubectl top`, and — critically — the HPA on Day 18). On kind, kubelets use self-signed certs, so metrics-server needs `--kubelet-insecure-tls` or it crash-loops with TLS errors — a flag for labs only, never production. Remember the divide: `kubectl top` = *measured usage* (metrics-server); `kubectl describe node` = *requested* bookkeeping (scheduler math). They disagree all the time, and reading both is how you spot waste.

## Lab

### 1. Install metrics-server

Via helm (first helm use in the course — note the repo/install pattern, it recurs weekly):

```sh
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/
helm repo update
helm upgrade --install metrics-server metrics-server/metrics-server \
  --namespace kube-system \
  --set 'args={--kubelet-insecure-tls}'
kubectl -n kube-system rollout status deployment/metrics-server
```

(Manifest alternative: `kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml`, then patch the same arg in: `kubectl -n kube-system patch deployment metrics-server --type=json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'`.)

Give it ~60 s to scrape, then:

```sh
kubectl top nodes
kubectl top pods -A --sort-by=memory | head
```

In k9s, pod/node views now show live CPU/MEM columns (`:pulses` for a cluster dashboard).

### 2. One pod per QoS class

Requirements — write `qos.yaml` with three busybox pods (`command: ["sleep", "3600"]`):

- `qos-guaranteed`: cpu request=limit `100m`, memory request=limit `64Mi`
- `qos-burstable`: requests cpu `50m` / memory `32Mi`, limits cpu `200m` / memory `128Mi`
- `qos-besteffort`: no resources block at all

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata: {name: qos-guaranteed}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sleep", "3600"]
      resources:
        requests: {cpu: 100m, memory: 64Mi}
        limits:   {cpu: 100m, memory: 64Mi}
---
apiVersion: v1
kind: Pod
metadata: {name: qos-burstable}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sleep", "3600"]
      resources:
        requests: {cpu: 50m, memory: 32Mi}
        limits:   {cpu: 200m, memory: 128Mi}
---
apiVersion: v1
kind: Pod
metadata: {name: qos-besteffort}
spec:
  containers:
    - name: c
      image: busybox
      command: ["sleep", "3600"]
```

</details>

```sh
kubectl apply -f qos.yaml
for p in qos-guaranteed qos-burstable qos-besteffort; do
  echo -n "$p: "; kubectl get pod $p -o jsonpath='{.status.qosClass}'; echo
done
```

`Guaranteed`, `Burstable`, `BestEffort`. The class is *derived*, never written — try setting it and the API ignores/rejects it. Also see where requests landed in the scheduler's books:

```sh
kubectl describe node course-worker | grep -A8 'Allocated resources'
```

### 3. Throttle lab: CPU at the wall

Give the podlab Deployment (still running from Day 7) a tight CPU limit — add to its container:

```yaml
          resources:
            requests: {cpu: 100m, memory: 64Mi}
            limits:   {cpu: 100m, memory: 128Mi}
```

```sh
kubectl apply -f deploy.yaml && kubectl rollout status deployment/podlab
kubectl port-forward svc/podlab 8081:80 &
```

podlab's `/load?seconds=n` burns CPU on **all cores** — it *wants* multiple CPUs. Fire 30 s of load at one pod and watch the cgroup hold the line. Terminal 2:

```sh
watch -n 3 'kubectl top pods -l app=podlab'
```

Terminal 1:

```sh
curl -s 'localhost:8081/load?seconds=30'
```

`kubectl top` (15-ish second granularity — be patient) shows the loaded pod pinned at **~100m**, not the multiple cores it's trying to use. Nothing crashed, no event fired, no restart — the only symptom is that the `/load` call takes its sweet time and any concurrent request gets slow. That silence is exactly why CPU throttling is the most under-diagnosed performance problem in Kubernetes. Now remove the cpu limit line (keep the request and the memory lines), apply, rerun — `top` happily shows several hundred millicores.

### 4. OOM lab: memory at the wall

A pod that allocates ~64 MiB under a 32 MiB limit — `oom.yaml` (python is multi-arch and allocation is deterministic):

```yaml
apiVersion: v1
kind: Pod
metadata: {name: oom-demo}
spec:
  containers:
    - name: eater
      image: python:3-alpine
      command: ["python3", "-c", "x = bytearray(64*1024*1024); import time; time.sleep(3600)"]
      resources:
        requests: {memory: 32Mi}
        limits:   {memory: 32Mi}
```

```sh
kubectl apply -f oom.yaml
kubectl get pod oom-demo --watch
```

`Running` → `OOMKilled` → (backoff) → `Running` → `OOMKilled` → `CrashLoopBackOff`, RESTARTS climbing. The kernel killed it mid-`bytearray`. The forensics, which you should be able to find half-asleep:

```sh
kubectl describe pod oom-demo | grep -B1 -A6 'Last State'
#   Last State:  Terminated
#     Reason:    OOMKilled
#     Exit Code: 137
kubectl get pod oom-demo -o jsonpath='{.status.containerStatuses[0].restartCount}'; echo
```

Exit code 137 = 128 + 9 (SIGKILL). There is no warning, no grace period, no SIGTERM — memory limits are a trapdoor, which is why you size them from observed `kubectl top`/metrics data (Day 21+) plus headroom, not vibes. Fix it by raising the limit to `128Mi`, apply, and watch it go `Running` and stay.

### 5. Requests can refuse to schedule

```sh
kubectl run greedy --image=busybox --restart=Never \
  --overrides='{"spec":{"containers":[{"name":"greedy","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"cpu":"64"}}}]}}'
kubectl get pod greedy        # Pending — forever
kubectl describe pod greedy | grep -A4 Events
```

`0/3 nodes are available: ... Insufficient cpu`. No node has 64 spare CPUs *on paper*; actual usage is irrelevant. This is the other half of requests: too low → noisy neighbors and evictions; too high → Pending pods and wasted nodes. `kubectl delete pod greedy`.

LimitRange — the namespace object that injects default requests/limits so BestEffort can't happen by accident — is tomorrow's lab; you now know exactly what it's defending against.

## Verify ✅

- [ ] `kubectl top nodes` → three rows with CPU/memory numbers (no `error: Metrics API not available`)
- [ ] `kubectl get pod qos-guaranteed -o jsonpath='{.status.qosClass}'` → `Guaranteed`; burstable → `Burstable`; besteffort → `BestEffort`
- [ ] During `/load` with the 100m limit: `kubectl top pods -l app=podlab` shows the loaded pod at ~`100m`; after removing the limit, the same test shows it well above `100m`
- [ ] `kubectl describe pod oom-demo` (before the fix) → `Reason: OOMKilled`, `Exit Code: 137`, restart count ≥ 1
- [ ] `kubectl describe pod greedy` → event containing `Insufficient cpu`

## CKA corner 🎓

Exam notes:

- Know the schema path blind: `spec.containers[].resources.{requests,limits}.{cpu,memory}` — and `kubectl explain pod.spec.containers.resources` when not.
- `kubectl set resources deployment x --requests=cpu=100m,memory=64Mi --limits=cpu=200m,memory=128Mi` — the fast imperative for existing workloads.
- Diagnose from status, not logs: `OOMKilled` lives in `describe` under Last State; `Insufficient cpu/memory` lives in the *pod's events*; `kubectl top` needs metrics-server (the exam cluster has it).

**Drill 1 (2 min):** Create pod `fixed` (nginx) that is QoS class `Guaranteed` with 200m CPU / 128Mi memory, using only imperative commands + one edit-free flag combo. Verify the class.

<details><summary>Solution</summary>

```sh
kubectl run fixed --image=nginx \
  --overrides='{"spec":{"containers":[{"name":"fixed","image":"nginx","resources":{"requests":{"cpu":"200m","memory":"128Mi"},"limits":{"cpu":"200m","memory":"128Mi"}}}]}}'
kubectl get pod fixed -o jsonpath='{.status.qosClass}'   # Guaranteed
kubectl delete pod fixed
```

(Equally valid: `kubectl run fixed --image=nginx --dry-run=client -o yaml`, add resources, apply. With limits set and requests omitted, requests default to limits — also Guaranteed.)

</details>

**Drill 2 (3 min):** A pod keeps restarting. In one minute, determine *whether* it was OOMKilled and *what* its memory limit is, using exactly two commands. Practice on `oom-demo` (recreate it with the 32Mi limit).

<details><summary>Solution</summary>

```sh
kubectl get pod oom-demo -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}'   # OOMKilled
kubectl get pod oom-demo -o jsonpath='{.spec.containers[0].resources.limits.memory}'                # 32Mi
```

(`kubectl describe pod oom-demo` shows both, if jsonpath escapes you under exam pressure.)

</details>

## Stretch goals

- Find the actual cgroup: `kubectl debug` into a podlab pod (busybox, `--target=podlab`) and `cat /sys/fs/cgroup/cpu.max` — with the 100m limit you'll see `10000 100000` (10 ms quota per 100 ms period).
- Run `/load` simultaneously against two podlab pods on the same node with *no* limits and watch them share the node by request-weight.
- Look at `kubectl get --raw /apis/metrics.k8s.io/v1beta1/pods | python3 -m json.tool | head -40` — the raw Metrics API the HPA will consume on Day 18.
- Read about `LimitedSwap` and in-place resize (`kubectl resize`?) — resource management is actively evolving; check what's GA on your cluster version with `kubectl explain pod.spec.containers.resizePolicy`.

## Cleanup

```sh
kill %1 2>/dev/null
kubectl delete pod qos-guaranteed qos-burstable qos-besteffort oom-demo greedy --ignore-not-found
```

**Keep:** **metrics-server** (HPA on Day 18 requires it; after the Day 15 cluster rebuild you'll reinstall it with the same helm one-liner). Keep the `podlab` Deployment **with its resources block** (requests cpu 100m/memory 64Mi, memory limit 128Mi, no cpu limit) — it's now respectably Burstable, and Day 10 builds on it.
