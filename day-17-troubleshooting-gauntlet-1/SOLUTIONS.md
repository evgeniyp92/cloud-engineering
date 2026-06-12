# Solutions — root cause, fix, and the one command that revealed it

Spoilers. Score yourself first (rubric in the README).

---

## 01 — web-pull: ImagePullBackOff

**Root cause.** Image tag typo: `podlab:v1.0` does not exist — the loaded image is `podlab:v1`. The node has no such image locally, tries the registry, and fails (podlab was never pushed anywhere).

**Revealing command.**
```sh
kubectl describe pod -n gauntlet -l app=web-pull
# Events: Failed to pull image "podlab:v1.0" ... not found
```

**Fix.**
```sh
kubectl set image deployment/web-pull podlab=podlab:v1 -n gauntlet
# or: kubectl edit deployment web-pull -n gauntlet
```

---

## 02 — web-restarts: liveness probe kills a healthy app

**Root cause.** Liveness probe targets port **8081**; podlab listens on 8080. Every probe is refused, and `failureThreshold: 1` gives zero grace — the kubelet kills the container after a single miss, forever. The app itself is fine (its logs show normal startup each time).

**Revealing command.**
```sh
kubectl describe pod -n gauntlet -l app=web-restarts
# Events: Liveness probe failed: ... connect: connection refused (port 8081)
# Killing container ... failed liveness probe, will be restarted
```

**Fix.** Edit the deployment: probe port → `8080` (and, good practice, raise `failureThreshold` to 3 — one blip should never restart a container).

---

## 03 — web-pending: unschedulable resource request

**Root cause.** `requests.memory: 10Gi` exceeds any node's allocatable memory; the filter phase eliminates every node, the pod stays Pending forever.

**Revealing command.**
```sh
kubectl describe pod -n gauntlet -l app=web-pending
# Events: 0/3 nodes are available: ... 3 Insufficient memory.
```

**Fix.** Edit the deployment: a sane request (e.g. `memory: 64Mi`, limit `128Mi`). The pod schedules within seconds — no need to touch the stuck pod, the ReplicaSet replaces it.

---

## 04 — web-noroute: Service selector typo, zero endpoints

**Root cause.** Pods carry `app: podlab`; the Service selects `app: podlb`. Nothing matches, so the Endpoints object is empty and every connection to the Service dies with no backend. The workload itself is 100% healthy — this is a *wiring* failure, invisible in pod views.

**Revealing command.**
```sh
kubectl get endpoints web-noroute -n gauntlet
# ENDPOINTS: <none>        ← the entire diagnosis in one word
```

**Fix.**
```sh
kubectl patch service web-noroute -n gauntlet \
  -p '{"spec":{"selector":{"app":"podlab"}}}'
kubectl get endpoints web-noroute -n gauntlet   # two pod IPs appear
```

---

## 05 — web-config: envFrom a ConfigMap that doesn't exist

**Root cause.** The pod references ConfigMap `podlab-settings` via `envFrom`; no such ConfigMap exists in `gauntlet`. The kubelet can't assemble the container's environment → `CreateContainerConfigError` (the container is never created — distinct from a crash).

**Revealing command.**
```sh
kubectl describe pod -n gauntlet -l app=web-config
# Events: Error: configmap "podlab-settings" not found
```

**Fix.** Create the missing object (or delete the reference):
```sh
kubectl create configmap podlab-settings -n gauntlet \
  --from-literal=COLOR=teal --from-literal=VERSION=v1
```
The kubelet retries automatically; the pod starts without a redeploy.

---

## 06 — web-notready: app and probe disagree about the port

**Root cause.** Env `PORT=9090` makes podlab listen on 9090, but the readiness probe (and Service `targetPort`) point at 8080. Probe: connection refused → pod Running but never Ready → never an endpoint. `containerPort: 8080` in the spec is *documentation only* — it binds nothing and validates nothing, which is what makes this one sneaky. The giveaway is the app's own first log line: `"msg":"podlab starting","port":"9090"`.

**Revealing command.**
```sh
kubectl logs -n gauntlet deploy/web-notready | head -1
# {"level":"INFO","msg":"podlab starting","port":"9090",...}
```

**Fix.** Make reality agree — simplest: delete the `PORT` env var so podlab uses its 8080 default (`kubectl edit deployment web-notready -n gauntlet`). Equally valid: change probe + targetPort + containerPort all to 9090. Either way: pod goes 1/1, endpoint appears.

---

## Meta-lesson

Six scenarios, six different *first* signals:

| Scenario | The signal lived in |
|---|---|
| 01 | pod STATUS column + describe Events |
| 02 | RESTARTS column + describe Events (who killed it?) |
| 03 | describe Events (scheduler's verdict) |
| 04 | **Endpoints**, not pods |
| 05 | pod STATUS (`CreateContainerConfigError`) + Events |
| 06 | the **application's own logs** |

`get` → `describe` → `logs` → `exec`, in that order, covers all of them. The loop works.
