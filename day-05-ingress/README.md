# Day 05 — Ingress

> **Time:** ~3 h · **Builds on:** Days 1, 3, 4

## Objectives

- Explain what Ingress adds over Services (L7 vs L4) and why one entry point serves many apps.
- Install ingress-nginx on kind and understand how it reaches your Mac through Day 1's port mappings.
- Route by hostname and by path to two podlab variants, and debug `pathType` behavior.
- Know what `ingressClassName` and `defaultBackend` do, and where TLS will plug in later.

## Concepts

### L4 ran out of road

Everything Day 4 built operates at **L4** (TCP/IP): kube-proxy rewrites packet destinations without ever reading them. A NodePort/LoadBalancer per service therefore means: one external IP *per service*, no shared TLS termination, no "route by URL". Real platforms run tens of services; nobody provisions tens of load balancers.

**Ingress** moves up to **L7** (HTTP): a single entry point that reads the `Host:` header and the request path, then proxies to the right Service:

```
                        ┌────────────────────────────┐
 blue.localhost:8080 ──▶│                            │──▶ svc podlab-blue ──▶ pods
                        │  ingress-nginx controller  │
 green.localhost:8080 ─▶│  (one pod, one entry IP)   │──▶ svc podlab-green ──▶ pods
                        └────────────────────────────┘
```

One IP, one TLS certificate point, many services. The economics alone (one cloud LB ≈ $20+/month each) made this the standard pattern.

### Ingress is an API; a controller is the engine

Crucial split: the **Ingress resource** is just routing *rules* stored in the API — applying one to a bare cluster does nothing. An **ingress controller** is the actual proxy (here, nginx) that watches Ingress objects and rewrites its own config to match. Several controllers can coexist (nginx, Traefik, HAProxy, cloud-native ones); each Ingress picks its engine via `ingressClassName`. Forgetting that field on a cluster with no default class is the classic "my Ingress does nothing" bug — the resource sits there, watched by nobody.

(For completeness: the **Gateway API** is Ingress's richer successor and ingress-nginx is in maintenance mode, but Ingress remains what you'll meet in nearly every existing cluster and interview. Learn this first.)

### How traffic reaches a kind cluster

On a cloud, the controller's Service is type LoadBalancer and the cloud wires up an IP. On kind we pre-wired it on Day 1: the kind-specific ingress-nginx manifest schedules the controller onto the node labelled `ingress-ready=true` (our control-plane) and binds **hostPorts 80/443 on that node**; our kind config maps Mac ports `8080→80` and `8443→443` on the same node. Chain:

```
curl blue.localhost:8080 ──▶ Docker port map 8080→80 ──▶ controller pod (hostPort 80) ──▶ Service ──▶ podlab pod
```

One more macOS gift: anything under `*.localhost` resolves to `127.0.0.1` natively — no `/etc/hosts` editing for `blue.localhost`, `green.localhost`, or any name you invent today.

### pathType — the gotcha field

Every path rule must declare how to match:

| pathType | `/api` matches | Notes |
|---|---|---|
| `Exact` | `/api` only — **not** `/api/` | rarely what you want |
| `Prefix` | `/api`, `/api/`, `/api/v1` — but **not** `/apiary` | matches whole path *segments* |
| `ImplementationSpecific` | whatever the controller decides | avoid; non-portable |

Two classic surprises: `Exact` failing on a trailing slash, and `Prefix: /` swallowing everything (it matches every request — fine as a catch-all, fatal above more specific rules on some controllers; nginx evaluates longest-match first, so it works, but don't rely on that habit across controllers). Also: the *backend app* still receives the full original path. `Prefix: /blue` routed to podlab means podlab gets a request for `/blue` — which it 404s. Fixing that requires a **rewrite**, which is controller-specific (an nginx annotation). You'll hit this live in the lab.

### defaultBackend and TLS

`defaultBackend` catches requests matching no rule (otherwise the controller serves its own `404 Not Found` page from a built-in handler). TLS is one stanza away (`spec.tls` + a certificate Secret) — you'll do it properly on Day 40 with cert-manager; today's `https://...:8443` simply serves the controller's self-signed "fake certificate".

## Lab

### 1. Install ingress-nginx (kind flavor)

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml
kubectl wait --namespace ingress-nginx \
  --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller \
  --timeout=120s
```

Inspect what made the kind flavor special:

```sh
kubectl -n ingress-nginx get pods -o wide          # controller runs on course-control-plane
kubectl -n ingress-nginx get deploy ingress-nginx-controller -o yaml | grep -B2 -A3 'ingress-ready\|hostPort'
kubectl get ingressclass                           # "nginx" — remember this name
```

Smoke test the wiring before any Ingress exists:

```sh
curl -s http://localhost:8080/
```

`404 Not Found` **from nginx** is success — your request crossed Mac → Docker → controller; there's just no route yet.

### 2. Deploy the blue and green variants

Two podlab deployments differing only in `COLOR`, with matching services. This is boilerplate (you wrote both kinds by hand already) — apply inline as `colors.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: podlab-blue}
spec:
  replicas: 2
  selector: {matchLabels: {app: podlab-blue}}
  template:
    metadata: {labels: {app: podlab-blue}}
    spec:
      containers:
        - name: podlab
          image: podlab:v1
          ports: [{containerPort: 8080}]
          env: [{name: COLOR, value: blue}, {name: VERSION, value: "1.0.0"}]
---
apiVersion: v1
kind: Service
metadata: {name: podlab-blue}
spec:
  selector: {app: podlab-blue}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: podlab-green}
spec:
  replicas: 2
  selector: {matchLabels: {app: podlab-green}}
  template:
    metadata: {labels: {app: podlab-green}}
    spec:
      containers:
        - name: podlab
          image: podlab:v1
          ports: [{containerPort: 8080}]
          env: [{name: COLOR, value: green}, {name: VERSION, value: "1.0.0"}]
---
apiVersion: v1
kind: Service
metadata: {name: podlab-green}
spec:
  selector: {app: podlab-green}
  ports: [{port: 80, targetPort: 8080}]
```

```sh
kubectl apply -f colors.yaml
kubectl get pods -l 'app in (podlab-blue,podlab-green)'
```

### 3. Host-based routing — the core object

Requirements — write `ingress.yaml` yourself:

- Ingress `colors`, `ingressClassName: nginx`
- host `blue.localhost` → service `podlab-blue` port 80 (path `/`, pathType `Prefix`)
- host `green.localhost` → service `podlab-green` port 80 (same)

<details><summary>Solution</summary>

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: colors
spec:
  ingressClassName: nginx
  rules:
    - host: blue.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: podlab-blue
                port:
                  number: 80
    - host: green.localhost
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: podlab-green
                port:
                  number: 80
```

</details>

```sh
kubectl apply -f ingress.yaml
kubectl get ingress colors        # HOSTS lists both names
curl -s http://blue.localhost:8080/  | grep -o '"color":"[^"]*"'    # "color":"blue"
curl -s http://green.localhost:8080/ | grep -o '"color":"[^"]*"'    # "color":"green"
curl -s http://localhost:8080/ -H 'Host: blue.localhost' | grep -o '"color":"[^"]*"'
```

That last command is the truth of host routing: it's *only* the `Host:` header. Same TCP destination, different header, different backend.

### 4. Path-based routing and the rewrite gotcha

Add a third rule to `ingress.yaml` — a single host fanning out by path:

```yaml
    - host: colors.localhost
      http:
        paths:
          - path: /blue
            pathType: Prefix
            backend:
              service: {name: podlab-blue, port: {number: 80}}
          - path: /green
            pathType: Prefix
            backend:
              service: {name: podlab-green, port: {number: 80}}
```

```sh
kubectl apply -f ingress.yaml
curl -s http://colors.localhost:8080/blue
```

`{"error":"not found"}` — **from podlab**, not nginx. Routing worked; podlab received the path `/blue` and it only serves `/`. The fix is a rewrite, which in ingress-nginx is an annotation. Add under `metadata:`:

```yaml
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /
```

```sh
kubectl apply -f ingress.yaml
curl -s http://colors.localhost:8080/blue  | grep -o '"color":"[^"]*"'   # blue
curl -s http://colors.localhost:8080/green | grep -o '"color":"[^"]*"'   # green
```

Note the annotation applies to the whole Ingress — in real setups you'd keep rewrite rules in their own Ingress object. (Annotations are nginx-specific; this is the portability tax `pathType: ImplementationSpecific` warned you about.)

### 5. pathType experiments + defaultBackend

```sh
curl -s http://colors.localhost:8080/bluebird -o /dev/null -w '%{http_code}\n'
```

`404` — `Prefix: /blue` does *segment* matching, so `/bluebird` doesn't match (good!). Now catch strays — add to the `colors.localhost` rule a catch-all and watch ordering not matter:

```sh
curl -s http://nosuch.localhost:8080/ -o /dev/null -w '%{http_code}\n'   # 404 from nginx default handler
```

Give the whole Ingress a `defaultBackend` (top of `spec:`, beside `rules:`):

```yaml
  defaultBackend:
    service:
      name: podlab-blue
      port:
        number: 80
```

```sh
kubectl apply -f ingress.yaml
curl -s http://nosuch.localhost:8080/healthz    # now answered by podlab-blue
```

Unmatched hosts fall through to your backend instead of nginx's 404 page. Remove the `defaultBackend` block again after testing (a global catch-all on a shared controller is usually a mistake) and re-apply.

### 6. Watch the controller work

```sh
stern -n ingress-nginx ingress-nginx-controller --include blue.localhost
```

Curl `blue.localhost:8080` a few times — every request logs through the controller: one proxy seeing all L7 traffic is also why ingress controllers are the natural home for access logs, rate limiting, and auth. In k9s: `:ingress` shows your rules; `:pods ingress-nginx` then `l` tails the same logs.

## Verify ✅

- [ ] `kubectl -n ingress-nginx get pods` → controller `Running` on `course-control-plane`
- [ ] `curl -s http://blue.localhost:8080/ | grep color` → `"color":"blue"`; same for green
- [ ] `curl -s http://localhost:8080/ -H 'Host: green.localhost' | grep color` → `"color":"green"`
- [ ] `curl -s http://colors.localhost:8080/blue | grep color` → `"color":"blue"` (rewrite in place)
- [ ] `curl -s -o /dev/null -w '%{http_code}' http://colors.localhost:8080/bluebird` → `404`
- [ ] `kubectl get ingress colors -o jsonpath='{.spec.ingressClassName}'` → `nginx`

## Stretch goals

- `kubectl -n ingress-nginx exec deploy/ingress-nginx-controller -- cat /etc/nginx/nginx.conf | grep -A5 'server_name blue.localhost'` — your Ingress YAML, compiled to actual nginx config.
- Hit `https://blue.localhost:8443/ -k` and inspect the self-signed "Kubernetes Ingress Controller Fake Certificate" with `openssl s_client` — the hole Day 40 fills.
- Add a canary: ingress-nginx's `canary` + `canary-weight: "20"` annotations on a second Ingress for the same host — 20% of requests to blue.localhost answer green. (Day 45 does this properly with Argo Rollouts.)
- Scale `podlab-blue` to 0 and observe what the Ingress returns (`503` from nginx — empty endpoints again, Day 4's lesson at L7).

## Cleanup

```sh
kubectl delete -f colors.yaml
kubectl delete ingress colors
```

**Keep:** the **ingress-nginx installation** (namespace `ingress-nginx`) — it serves the rest of the course. Also keep the `podlab` Deployment + Service from Days 3–4; Day 6 mounts ConfigMaps into it.
