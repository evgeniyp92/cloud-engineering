# Day 02 — Pods Deep Dive

> **Time:** ~3 h · **Builds on:** Day 1

## Objectives

- Write a complete pod spec by hand and explain every field you typed.
- Use init containers and native sidecars, and know precisely how they differ.
- Inject `POD_IP`, `NODE_NAME`, and `POD_NAMESPACE` via the Downward API so podlab's `/` finally tells the truth.
- Debug a distroless container (no shell!) with `kubectl debug` ephemeral containers.

## Concepts

### The pod is the atom

Kubernetes never schedules a container. It schedules a **pod**: one or more containers that always land on the same node and share three things — a network namespace (one IP, one `localhost`, one port space), optionally volumes, and a lifecycle (created together, die together). "Why not just containers?" Because real workloads are rarely one process: an app plus a log shipper, an app plus a proxy. Co-locating them with shared `localhost` and shared volumes is the primitive everything else (Deployments, Jobs, StatefulSets) stamps out copies of.

A consequence worth tattooing somewhere: **containers in a pod share one IP**. Two containers in the same pod can't both listen on 8080, and they reach each other on `localhost`, not via service DNS.

### Anatomy of a pod spec

Every Kubernetes object has the same four top-level fields:

```
apiVersion: v1          # which API group/version defines this kind
kind: Pod
metadata:               # name, namespace, labels, annotations
spec:                   # desired state — what YOU write
status:                 # observed state — what the CLUSTER writes; never author it
```

Inside `spec`, the fields you'll touch this week: `containers` (name, image, ports, env, resources, volumeMounts, probes), `initContainers`, `volumes`, `restartPolicy`, `terminationGracePeriodSeconds`. Today you write all of this by hand — no `--dry-run` generators yet. Typing it once is how the structure sticks; generators come at the end of the day as the speed tool.

### Init containers vs sidecars

| | Init container | Native sidecar |
|---|---|---|
| Defined in | `initContainers` | `initContainers` **with `restartPolicy: Always`** |
| Runs | to completion, *before* app containers, in order | starts before app containers, then **keeps running alongside** them |
| Restart on exit | per pod restartPolicy, until success | always |
| Blocks pod "Ready" | yes, until it exits 0 | no (once started/ready) |
| Typical job | wait-for-dependency, fetch config, run migrations | log shipper, proxy, config reloader |

The "native sidecar" (stable since Kubernetes 1.29) fixed a decade-old wart: sidecars used to be ordinary containers, so your app could start before its proxy was up, and a Job's pod would never finish because the log shipper kept running. Putting the sidecar in `initContainers` with `restartPolicy: Always` gives ordered startup *and* automatic shutdown after the main containers exit.

### The Downward API

A process shouldn't have to call the Kubernetes API to learn its own pod name, IP, namespace, or node — and usually it isn't allowed to (RBAC, Day 13). The **Downward API** projects pod/metadata fields *down* into the container as env vars (`valueFrom.fieldRef`) or files. podlab is built for this: it reads `POD_IP`, `NODE_NAME`, `POD_NAMESPACE` from the environment and reports them at `/`. Yesterday they were empty strings; today you fill them. This pattern is everywhere in real clusters — Prometheus relabeling, log enrichment, peer discovery.

### Lifecycle phases and restartPolicy

A pod's `status.phase` is coarse: `Pending` (accepted, not all containers running — includes scheduling and image pulls) → `Running` → `Succeeded`/`Failed`. The interesting detail lives in container states (`waiting`/`running`/`terminated`) and `kubectl describe`'s conditions. `restartPolicy` (`Always` | `OnFailure` | `Never`) is pod-wide and controls what kubelet does when a *container* exits — the pod object itself is never "restarted", and crash-looping containers back off exponentially up to 5 minutes (`CrashLoopBackOff` is a *waiting reason*, not a phase).

### Debugging when there's no shell

podlab's image is distroless: no `sh`, no `ls`, no package manager — a tiny attack surface and nothing for an intruder to use. Great for security, awkward for `kubectl exec`. The answer is **ephemeral containers**: `kubectl debug` attaches a *new* temporary container (with your choice of image, e.g. busybox) to the *running* pod. With `--target`, it shares the target container's **process namespace**, so you can see podlab's PID 1 and hit its `localhost:8080` — without the app image carrying a single debugging byte.

## Lab

### 1. A pod spec by hand

Requirements — write `pod.yaml` yourself before peeking:

- `apiVersion: v1`, `kind: Pod`, name `podlab`, label `app: podlab`
- one container `podlab`, image `podlab:v1`, containerPort 8080
- env var `VERSION` = `1.0.0`
- env vars `POD_IP`, `NODE_NAME`, `POD_NAMESPACE` from the Downward API fieldRefs `status.podIP`, `spec.nodeName`, `metadata.namespace`

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: podlab
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
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
```

</details>

```sh
kubectl apply -f pod.yaml
kubectl get pod podlab -o wide
kubectl port-forward pod/podlab 8081:8080 &
curl -s localhost:8081/ | python3 -m json.tool
```

**Core verify:** `pod_ip`, `node_name`, `namespace` are now populated — and `pod_ip`/`node_name` match the `-o wide` columns exactly. Kill the port-forward (`kill %1`) and delete the pod (`kubectl delete pod podlab`) before the next step.

### 2. Init container: wait, then start

Requirements — `pod-init.yaml`, name `podlab-init`:

- init container `wait-a-bit`, image `busybox`, command `sh -c 'echo waiting for imaginary dependency...; sleep 15; echo done'`
- then the same podlab container as step 1 (Downward API env included)

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: podlab-init
  labels:
    app: podlab
spec:
  initContainers:
    - name: wait-a-bit
      image: busybox
      command: ["sh", "-c", "echo waiting for imaginary dependency...; sleep 15; echo done"]
  containers:
    - name: podlab
      image: podlab:v1
      ports:
        - containerPort: 8080
      env:
        - name: POD_IP
          valueFrom: { fieldRef: { fieldPath: status.podIP } }
        - name: NODE_NAME
          valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
        - name: POD_NAMESPACE
          valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
```

</details>

```sh
kubectl apply -f pod-init.yaml
kubectl get pod podlab-init --watch
```

Watch the status walk through `Init:0/1` → `PodInitializing` → `Running`. While it's initializing:

```sh
kubectl logs podlab-init -c wait-a-bit -f
```

Real-world version of this: an init container running `nc -z postgres 5432` in a loop, or a migration job. Day 11's guestbook uses exactly that shape.

### 3. Native sidecar

Requirements — `pod-sidecar.yaml`, name `podlab-sidecar`:

- an `emptyDir` volume `shared-logs`
- **native sidecar** `logger` (busybox, in `initContainers` with `restartPolicy: Always`): `sh -c 'touch /logs/app.log && tail -F /logs/app.log'`, mounts `shared-logs` at `/logs`
- main container: busybox, writes a timestamp line to `/logs/app.log` every 2 s, same mount

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: podlab-sidecar
spec:
  volumes:
    - name: shared-logs
      emptyDir: {}
  initContainers:
    - name: logger
      image: busybox
      restartPolicy: Always          # ← this one line makes it a sidecar
      command: ["sh", "-c", "touch /logs/app.log && tail -F /logs/app.log"]
      volumeMounts:
        - name: shared-logs
          mountPath: /logs
  containers:
    - name: writer
      image: busybox
      command: ["sh", "-c", "while true; do echo \"$(date) hello from writer\" >> /logs/app.log; sleep 2; done"]
      volumeMounts:
        - name: shared-logs
          mountPath: /logs
```

</details>

```sh
kubectl apply -f pod-sidecar.yaml
kubectl get pod podlab-sidecar          # READY 2/2 — the sidecar counts
kubectl logs podlab-sidecar -c logger -f
```

The sidecar is tailing a file the *other* container writes, via the shared emptyDir. Remove the `restartPolicy: Always` line in your head: it would become a plain init container that never exits, and `writer` would never start. That one line is the whole feature.

### 4. Lifecycle and restartPolicy

```sh
kubectl run boom --image=busybox --restart=Never -- sh -c 'sleep 5; exit 1'
kubectl get pod boom --watch        # Running → Error (phase Failed); never restarts
kubectl run boom2 --image=busybox -- sh -c 'sleep 5; exit 1'   # default restart=Always
kubectl get pod boom2 --watch       # Error → Running → CrashLoopBackOff, RESTARTS climbing
kubectl describe pod boom2 | grep -A4 'Last State'
kubectl delete pod boom boom2
```

`describe` shows `Last State: Terminated, Exit Code: 1` — your forensics when something crashes at 3 a.m.

### 5. Debug a distroless pod

First, prove the problem:

```sh
kubectl exec -it podlab-init -- sh
# error: ... "sh": executable file not found
```

No shell exists in the image. Attach an ephemeral busybox sharing podlab's process namespace:

```sh
kubectl debug -it pod/podlab-init --image=busybox --target=podlab
```

Inside the debug shell:

```sh
ps aux                      # you can SEE /podlab running — shared PID namespace
kill -0 1 && echo "pid 1 alive"
wget -qO- localhost:8080/   # shared NET namespace: podlab answers on localhost
exit
```

`kubectl describe pod podlab-init` now lists an `Ephemeral Containers` section — it stays in the pod spec until the pod dies, but never restarts. In k9s: select the pod, `d` to confirm the ephemeral container; `:events` shows it starting.

## Verify ✅

- [ ] `kubectl get pod podlab-init -o jsonpath='{.status.phase}'` → `Running`
- [ ] `curl -s localhost:8081/` (port-forward to `podlab-init`) → JSON where `pod_ip` equals `kubectl get pod podlab-init -o jsonpath='{.status.podIP}'` and `node_name` names a worker
- [ ] `kubectl logs podlab-init -c wait-a-bit` → ends with `done`
- [ ] `kubectl get pod podlab-sidecar` → `READY 2/2`; `kubectl logs podlab-sidecar -c logger --tail=3` → timestamped `hello from writer` lines
- [ ] `kubectl exec podlab-init -- sh` → fails with `executable file not found`
- [ ] `kubectl debug` shell: `wget -qO- localhost:8080/healthz` → `{"status":"ok"}`

## CKA corner 🎓

Exam notes:

- You will NOT hand-write pods in the exam — generate and edit: `kubectl run x --image=img --dry-run=client -o yaml > pod.yaml`. Know it cold.
- Multi-container pods: generate a single-container pod, then duplicate the entry under `containers:`. Faster than remembering structure.
- `kubectl explain pod.spec.initContainers --recursive | less` is your offline schema reference — allowed and fast.

**Drill 1 (4 min):** Create a pod `twin` with two containers: `web` (nginx) and `poller` (busybox, `sh -c 'while true; do wget -qO- localhost:80 >/dev/null && echo up; sleep 5; done'`). Verify `poller` logs print `up`.

<details><summary>Solution</summary>

```sh
kubectl run twin --image=nginx --dry-run=client -o yaml > twin.yaml
```

Edit `twin.yaml` — rename the container to `web`, append under `containers:`:

```yaml
    - name: poller
      image: busybox
      command: ["sh", "-c", "while true; do wget -qO- localhost:80 >/dev/null && echo up; sleep 5; done"]
```

```sh
kubectl apply -f twin.yaml && sleep 10 && kubectl logs twin -c poller
```

Works because both containers share the network namespace — `localhost:80` is nginx.

</details>

**Drill 2 (3 min):** Pod `prep`: init container (busybox) writes `<h1>built by init</h1>` to `/work/index.html` on an emptyDir; main container nginx serves that emptyDir at `/usr/share/nginx/html`. Verify with a port-forward + curl.

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata: { name: prep }
spec:
  volumes: [{ name: work, emptyDir: {} }]
  initContainers:
    - name: build
      image: busybox
      command: ["sh", "-c", "echo '<h1>built by init</h1>' > /work/index.html"]
      volumeMounts: [{ name: work, mountPath: /work }]
  containers:
    - name: web
      image: nginx
      volumeMounts: [{ name: work, mountPath: /usr/share/nginx/html }]
```

```sh
kubectl apply -f prep.yaml
kubectl port-forward pod/prep 8082:80 & sleep 2 && curl -s localhost:8082 && kill %1
```

</details>

## Stretch goals

- Project Downward API fields as *files* instead of env (`volumes[].downwardAPI`) — mount labels at `/etc/podinfo/labels` and read them via `/config` (set `CONFIG_DIR=/etc/podinfo`).
- Add `shareProcessNamespace: true` to a two-container pod and `ps` from one container to see the other — same effect `--target` gave you, but built into the spec.
- `kubectl get pod podlab-init -o yaml | less` — read the entire `status` block and map every condition to something you observed.

## Cleanup

```sh
kubectl delete pod podlab-init podlab-sidecar twin prep --ignore-not-found
```

**Keep:** the cluster and images. Save your `pod.yaml` — Day 3 turns it into a Deployment.
