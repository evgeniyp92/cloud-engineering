# Day 01 — Cluster & Toolbelt

> **Time:** ~3 h · **Builds on:** none

## Objectives

- Create the multi-node kind cluster (`course`) you'll use for the next 14 days, and prove its "nodes" are just Docker containers.
- Read a kubeconfig file and explain contexts, clusters, and users; switch between them with `kubectx`/`kubens`.
- Navigate a cluster fluently in **k9s** — the course's daily driver.
- Build both demo apps, load them into kind, run `podlab` as your first pod, and tail its logs with `stern`.

## Concepts

### Why kind

You need a real, multi-node, conformant Kubernetes cluster, and you need it to be disposable. [kind](https://kind.sigs.k8s.io/) ("Kubernetes IN Docker") runs each cluster *node* as a Docker container. Inside each container runs a full node stack: kubelet, containerd, and — on the control-plane node — the API server, scheduler, controller-manager, and etcd. It's the same software you'd run on cloud VMs, just packed into containers, which is why kind is what the Kubernetes project itself uses for CI.

This matters for learning: most local options (minikube default, Docker Desktop's built-in cluster) give you a single node, and a single node hides half of Kubernetes. Scheduling, node affinity, pod distribution, "the service routes to a pod on a *different* node" — none of it is visible with one node. Our cluster has 1 control-plane + 2 workers.

### The container-in-container nesting

Hold this picture; it explains every weird kind behavior you'll hit:

```
your Mac
└── Docker VM (Docker Desktop / OrbStack)
    ├── container: course-control-plane   ← a "node"
    │   ├── kube-apiserver, etcd, scheduler, controller-manager
    │   └── containerd → your pods' containers (nested!)
    ├── container: course-worker          ← a "node"
    │   └── containerd → pods
    └── container: course-worker2
        └── containerd → pods
```

Two consequences you'll feel immediately:

1. **Images on your Mac are invisible to the cluster.** `docker build` puts the image in *your Mac's* Docker daemon. The nodes have their *own* containerd image stores. `kind load docker-image` copies an image from your daemon into every node — that's why the demo apps need loading, and why there's no registry push/pull today.
2. **Node IPs aren't reachable from macOS.** The nodes live on a Docker bridge network inside the Docker VM. To reach workloads from your Mac you either `kubectl port-forward`, or use ports explicitly mapped in the kind config. Our config maps host `8080→80` and `8443→443` on the control-plane — that's the door ingress-nginx will sit behind from Day 5 onward.

### kubeconfig: how kubectl knows where to point

Everything `kubectl` does is an HTTPS call to the API server. `~/.kube/config` tells it where and as whom. Three lists, glued together by **contexts**:

| Section | What it holds |
|---|---|
| `clusters` | API server URL + its CA certificate |
| `users` | your credentials (client cert/key for kind) |
| `contexts` | a named (cluster, user, default-namespace) triple |

`kind create cluster` writes a `kind-course` context and makes it current. When you later have several clusters (Day 15 rebuilds this one; real jobs give you ten), `kubectx` switches contexts and `kubens` switches the context's default namespace — both are thin, fast wrappers over `kubectl config`.

### The toolbelt

| Tool | Job | You'll use it… |
|---|---|---|
| `kubectl` | the API client | constantly |
| `k9s` | terminal UI over the API | daily — watching, describing, log-tailing |
| `kubectx` / `kubens` | context / namespace switching | from Day 9 onward, a lot |
| `stern` | multi-pod log tailing with regex pod selection | whenever >1 replica exists |
| `helm` | package manager | Day 8 (metrics-server) onward |

k9s deserves a sentence of *why*: `kubectl get pods; kubectl describe pod x; kubectl logs x` is three commands and three screens. k9s is one live-updating screen where describe is one keystroke away. You'll still learn raw kubectl (the CKA exam terminal has no k9s), but for *observing* a cluster, k9s is strictly better.

### kubectl's output flags — learn them on day one

The same four flags unlock every later lesson; this course uses all of them constantly:

| Flag | Gives you | First used |
|---|---|---|
| `-o wide` | extra columns: pod IP, node, etc. | today |
| `-o yaml` | the full object as stored — including `status` | today |
| `-o jsonpath='{...}'` | one precise field, script-friendly | Day 3 onward, heavily |
| `--watch` / `-w` | live updates instead of a snapshot | today |

And the two reading tools: `kubectl describe <kind> <name>` (human-formatted state **plus recent events** — your default debugging move) and `kubectl explain <kind>.<field.path>` (the API schema as offline docs — `kubectl explain pod.spec.containers` beats googling and is exam-legal on the CKA).

## Lab

### 1. Install the toolbelt

```sh
brew install kind kubectl helm k9s kubectx stern
kind version && kubectl version --client && k9s version --short
```

### 2. Create the cluster

The fixture [`kind-config.yaml`](kind-config.yaml) in this folder defines 1 control-plane + 2 workers, labels the control-plane `ingress-ready=true`, and maps host ports 8080/8443 for the ingress controller you'll install on Day 5. Read it before applying — it's 26 lines.

```sh
cd day-01-cluster-and-toolbelt
kind create cluster --name course --config kind-config.yaml
kubectl get nodes
```

Wait until all three nodes show `Ready` (≤ a minute). Note the control-plane carries the `node-roles.kubernetes.io/control-plane` role and a matching taint — your pods will land on the workers.

### 3. Prove the nodes are containers

```sh
docker ps --filter "name=course"
```

Three containers: `course-control-plane`, `course-worker`, `course-worker2`. Poke inside one — there's a whole node in there:

```sh
docker exec course-worker crictl ps        # containerd's view of pods on this node
docker exec course-control-plane crictl ps | grep -E 'etcd|apiserver'
```

That `etcd` and `kube-apiserver` are themselves containers *inside* the control-plane container. Day 16 dissects them.

### 4. Read your kubeconfig

```sh
kubectl config view --minify        # current context only, secrets redacted
kubectl config get-contexts
```

Find in the output: the server URL (`https://127.0.0.1:<random-port>` — kind maps the API server to a host port), the cluster CA (`certificate-authority-data`), the user's client cert, and the `kind-course` context tying cluster + user together. Three more commands you'll use weekly:

```sh
kubectl config current-context
kubectl config use-context kind-course     # explicit switch (no-op right now)
kubectl cluster-info                       # where the API server and CoreDNS live
```

Now the ergonomic versions:

```sh
kubectx                             # lists contexts; `kubectx kind-course` switches; `kubectx -` toggles
kubens                              # namespaces of the current context
kubens kube-system
kubectl get pods                    # no -n flag needed — the context's default ns changed
kubens -                            # toggle back to default
```

Peek at what `kubens` actually did: `kubectl config view --minify | grep namespace` — it just wrote a `namespace:` field into your context. No magic, ever; these tools are all kubeconfig editors.

### 5. k9s guided tour

```sh
k9s
```

Spend 15 real minutes here — muscle memory now pays off for 49 days.

**Navigation model:** k9s is modal like vim. `:resource` switches views, arrows/`j`/`k` move, `Enter` drills down (pod → containers → logs), `Esc` backs out, `?` shows keys for the current view.

- `:pods` — pod view. The number keys hop namespaces: `0` all, `1` default. Try `:pods kube-system` and identify coredns, kube-proxy, etcd, the apiserver — the cast of Day 16.
- On a selected pod: `d` describe, `l` logs (`0` for full tail, `w` wrap, `s` toggle autoscroll), `y` full YAML, `e` edit, `Enter` to list its containers.
- `:deploy`, `:svc`, `:nodes`, `:ns` — every resource type has a view; type `:` and tab-complete. On `:nodes`, press `d` on a worker and skim Capacity/Allocatable — Day 8 lives there.
- `:events` — cluster events sorted live; your first stop whenever anything is `Pending`, `CrashLoopBackOff`, or mysteriously absent.
- `/` filters any view by regex (try `/coredns` in `:pods kube-system`); `Ctrl-\` is fullscreen logs.
- `Ctrl-d` deletes the selected resource (asks for confirmation) — the fast path you'll use instead of `kubectl delete` a hundred times.
- `:xray deploy` — tree view of deployments → pods → containers; great once Day 3 adds layers.
- `:q` quits.

A habit worth forming today: keep k9s open in a dedicated terminal pane all day, every day, set to `:events` or `:pods` — you'll *see* every lab in this course happen in real time as you type kubectl commands elsewhere.

### 6. Build and load the demo apps

```sh
cd ../apps/podlab    && docker build -t podlab:v1 .
cd ../guestbook      && docker build -t guestbook:v1 .
kind load docker-image podlab:v1    --name course
kind load docker-image guestbook:v1 --name course
docker exec course-worker crictl images | grep -E 'podlab|guestbook'
```

The last command proves the images now exist *on the node*, not just on your Mac. (Guestbook isn't used until Day 11 — loading it now means Day 11 starts clean.)

### 7. First pod

```sh
kubectl run podlab --image=podlab:v1 --port=8080
kubectl get pods -o wide --watch     # Ctrl-C once Running; note which worker it's on
kubectl describe pod podlab          # read the Events section bottom-up: Scheduled → Pulled → Created → Started
```

`-o wide` adds the pod IP and node. That IP is on the pod network — unreachable from macOS, so tunnel to it:

```sh
kubectl port-forward pod/podlab 8081:8080 &
curl -s localhost:8081/ | python3 -m json.tool
curl -s localhost:8081/config | python3 -m json.tool | head -30
```

Look at `/`: `version` is `dev` (no `VERSION` env set), and `pod_ip` / `node_name` / `namespace` are **empty strings**. The pod doesn't magically know these — Day 2's Downward API fixes that. `/config` dumps every env var; spot the `KUBERNETES_SERVICE_*` vars Kubernetes injected for free.

### 8. Tail logs with stern

In a second terminal:

```sh
stern podlab
```

Now curl a few endpoints (`curl localhost:8081/ ; curl localhost:8081/healthz`) and watch structured JSON log lines stream. stern's superpower over `kubectl logs`: the argument is a *regex over pod names*, so when Day 3 gives you 3 replicas, one `stern podlab` tails all of them, color-coded. Ctrl-C, then kill the port-forward:

```sh
kill %1
```

## Verify ✅

- [x] `kubectl get nodes` → 3 nodes, all `STATUS Ready`, one with role `control-plane`
- [x] `docker ps --filter name=course --format '{{.Names}}'` → exactly `course-control-plane`, `course-worker`, 
  `course-worker2`
- [x] `kubectl get node course-control-plane -o jsonpath='{.metadata.labels.ingress-ready}'` → `true`
- [x] `docker exec course-worker crictl images | grep podlab` → shows `podlab` with tag `v1`
- [x] `kubectl get pod podlab` → `Running`, `READY 1/1`
- [x] With a port-forward active, `curl -s localhost:8081/` → JSON containing `"app":"podlab"`, `"version":"dev"`, 
  and a `"hostname":"podlab"` field
- [x] `stern podlab` shows a JSON log line for each curl you send

## Stretch goals

- `kind get clusters`, then `kind get kubeconfig --name course` — compare with `~/.kube/config`.
- `docker exec course-control-plane cat /etc/kubernetes/manifests/kube-apiserver.yaml` — the control plane is itself defined as pod YAML (static pods, Day 16).
- In k9s, `:pods`, select `podlab`, press `t` to see its resource usage columns are empty — metrics-server arrives Day 8.
- Break it on purpose: `kubectl run broken --image=podlab:v999`, watch `:events` in k9s explain `ErrImagePull`, then `kubectl delete pod broken`.

## Cleanup

```sh
kubectl delete pod podlab
```

**Keep:** the `course` cluster and both loaded images — every day through Day 14 builds on them. Don't run `kind delete cluster` until Day 15 tells you to.
