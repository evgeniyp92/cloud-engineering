# Day 15 — Network Policies (and a Cluster Rebuild with Cilium)

> **Time:** ~4 h · **Builds on:** Days 1, 5, 11

## Objectives

- Explain the flat pod network model and why NetworkPolicy needs CNI enforcement
- Rebuild the `course` cluster from YAML with Cilium as the CNI — and learn that clusters are cattle
- Lock down the guestbook namespace with default-deny, then re-open exactly the flows it needs
- Diagnose the classic policy failure: forgetting DNS egress

## Concepts

### The flat network, and the firewall nobody turned on

Kubernetes networking has one radical rule: **every pod can reach every pod, no NAT, no permission asked.** Your guestbook API reaches Postgres; so can any pod in any namespace, any compromised container, any `kubectl run` curl. Namespaces are *organizational* boundaries (Day 9), not network ones.

**NetworkPolicy** is the firewall: a namespaced object that selects pods and whitelists traffic. The model is allow-list with an important twist:

- A pod selected by **no** policy: everything allowed (the default-open world above).
- A pod selected by **any** policy of type Ingress: only ingress matching *some* policy is allowed; everything else dropped. Same independently for Egress.
- Policies are **additive** — they union, never conflict. There is no "deny rule"; you deny by selecting a pod and allowing little.

So the standard pattern is: one `default-deny` policy selecting all pods in the namespace, then one small policy per legitimate flow.

```
default state:        with default-deny + allow policies:
┌─────┐    ┌─────┐    ┌─────┐  :5432   ┌─────┐
│ any │───▶│ db  │    │ api │── ✓ ────▶│ db  │
└─────┘    └─────┘    └─────┘          └─────┘
  anything goes          anything else: ✗ dropped
```

A policy rule can match peers three ways — `podSelector` (pods in *this* namespace), `namespaceSelector` (all pods in matching namespaces; combine both in one element for "these pods in those namespaces"), and `ipBlock` (CIDRs, for non-cluster traffic). Every namespace automatically carries the label `kubernetes.io/metadata.name: <name>`, which makes namespace selection precise — use it.

**The trap everyone falls into once** (today, on purpose): default-deny *egress* also blocks DNS. Your pod can't even resolve `guestbook-db` to an IP, so every connection fails at lookup, and the error says "no such host" — not "connection refused". Every locked-down namespace needs an explicit egress allowance to kube-dns in `kube-system` on port 53/UDP (and TCP, for large responses).

### Why today starts with `kind delete cluster`

Here is the catch: **NetworkPolicy objects do nothing by themselves.** The API server happily stores them; *enforcement* is the CNI plugin's job. kind's default CNI, **kindnet**, does not implement NetworkPolicy. You could write perfect policies on your current cluster and they would silently no-op — the most dangerous kind of security: the kind you believe you have.

So today you replace the CNI with **Cilium** (eBPF-based, enforces NetworkPolicy, plus observability via Hubble). CNI choice is baked in at pod-network level — the honest move is to rebuild the cluster. Which is the *second* lesson of the day, arguably the bigger one: **clusters are cattle.** Everything you have built in 14 days — guestbook stack, ingress controller, metrics-server — exists as YAML files and images. If rebuilding the cluster scares you, your infrastructure isn't code yet. Today you prove it isn't scary: delete, recreate, restore, ~20 minutes.

(From today the course runs on Cilium permanently. Day 48's capstone repeats this trick at full scale: empty cluster → one `kubectl apply` → entire platform.)

## Lab

### Part 1 — the rebuild

#### 1. Burn it down

Make sure your Day 11 YAML is at hand (`postgres.yaml`, `api.yaml`, the Secret command). Then:

```sh
kind delete cluster --name course
kind create cluster --name course --config kind-config-cilium.yaml
```

The config in this folder is your Day 1 config plus one line: `networking.disableDefaultCNI: true`.

#### 2. Observe a cluster with no CNI

```sh
kubectl get nodes
kubectl get pods -n kube-system
```

Nodes are **NotReady** — `kubectl describe node course-control-plane | grep -A3 Conditions` says the runtime network is not ready (`cni plugin not initialized`). CoreDNS pods sit `Pending`: they need the pod network, which doesn't exist. This is what "the CNI provides the pod network" looks like when it's missing. Worth 60 seconds of looking around — you'll meet this exact symptom in broken real clusters.

#### 3. Install Cilium

```sh
helm repo add cilium https://helm.cilium.io
helm repo update
helm install cilium cilium/cilium \
  --namespace kube-system \
  --set operator.replicas=1
```

(`operator.replicas=1` because the default of 2 wants two nodes for anti-affinity and one operator is plenty for a lab.) Watch the cluster come alive:

```sh
kubectl get pods -n kube-system -w   # cilium DaemonSet pods (one per node — Day 12!), operator, then CoreDNS goes Running
kubectl get nodes                    # Ready, Ready, Ready
```

#### 4. The rebuild checklist

This is deliberate practice — same commands as Days 1/5/8/11, no copy-pasting from old terminal scrollback; work from your files.

```sh
# 1. ingress-nginx (kind provider manifest — same as Day 5)
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml
kubectl wait --namespace ingress-nginx --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller --timeout=120s

# 2. metrics-server (same as Day 8)
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

# 3. demo app images back onto the nodes
kind load docker-image podlab:v1 guestbook:v1 --name course

# 4. the guestbook stack (Day 11 files)
kubectl create namespace guestbook
kubectl create secret generic guestbook-db -n guestbook \
  --from-literal=POSTGRES_PASSWORD=supersecret \
  --from-literal=DATABASE_URL='postgres://guestbook:supersecret@guestbook-db:5432/guestbook?sslmode=disable'
kubectl apply -f ../day-11-statefulsets-and-storage/postgres.yaml
kubectl apply -f ../day-11-statefulsets-and-storage/api.yaml
```

Verify the whole stack end-to-end before proceeding:

```sh
kubectl get pods -n guestbook        # db 1/1, two api pods 1/1
curl -s -X POST http://guestbook.localhost:8080/entries -H 'Content-Type: application/json' \
  -d '{"message":"rebuilt on cilium"}'
curl -s http://guestbook.localhost:8080/entries
```

(The old entries are gone — the PV died with the cluster. Day 43 fixes *that* with Velero. The *system* is what survived, as code.)

### Part 2 — lock down guestbook

#### 5. Default deny — and watch everything break

`default-deny.yaml` — the core pattern, memorize it:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
  namespace: guestbook
spec:
  podSelector: {}          # selects EVERY pod in the namespace
  policyTypes: ["Ingress", "Egress"]
```

```sh
kubectl apply -f default-deny.yaml
```

Now observe the damage, methodically:

```sh
# from outside the namespace — blocked:
kubectl run tester --image=curlimages/curl --rm -it --restart=Never -- \
  curl -sS --max-time 5 http://guestbook.guestbook.svc/entries
# → timeout

# the API can't reach the DB anymore — readiness collapses:
kubectl get pods -n guestbook -w     # api pods drop to 0/1 within ~30s (readyz now 503)
kubectl logs -n guestbook deploy/guestbook --tail=5   # db connection errors — note "no such host"!
curl -si http://guestbook.localhost:8080/entries | head -1   # 503 — no ready endpoints
```

"No such host": even DNS is dead, because egress-deny blocks the lookup to CoreDNS. Postgres itself still runs happily (`guestbook-db-0` stays 1/1 — its readiness probe is a local exec, no network). Sit with this picture; it's the day's most instructive broken state.

#### 6. Re-open exactly three flows

Flow map for the namespace:

```
ingress-nginx ns ──:8080──▶ api pods (app=guestbook) ──:5432──▶ db pod (app=guestbook-db)
                                  │
                                  └──:53/UDP+TCP──▶ kube-dns (kube-system)
```

Write the three policies yourself — this is the day's core YAML. Requirements:

1. `allow-dns`: applies to **all** pods (`podSelector: {}`), egress to namespace `kube-system`'s pods labeled `k8s-app: kube-dns`, ports 53 UDP **and** TCP.
2. `api-policy`: applies to `app: guestbook` pods; **ingress** from the `ingress-nginx` namespace (use `namespaceSelector` with `kubernetes.io/metadata.name`) on TCP 8080; **egress** to `app: guestbook-db` pods on TCP 5432.
3. `db-policy`: applies to `app: guestbook-db`; **ingress** from `app: guestbook` pods on TCP 5432 (no egress rules — the db initiates nothing, and the deny already covers it).

<details><summary>Solution</summary>

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-dns
  namespace: guestbook
spec:
  podSelector: {}
  policyTypes: ["Egress"]
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
      podSelector:
        matchLabels:
          k8s-app: kube-dns
    ports:
    - protocol: UDP
      port: 53
    - protocol: TCP
      port: 53
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: api-policy
  namespace: guestbook
spec:
  podSelector:
    matchLabels:
      app: guestbook
  policyTypes: ["Ingress", "Egress"]
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
    ports:
    - protocol: TCP
      port: 8080
  egress:
  - to:
    - podSelector:
        matchLabels:
          app: guestbook-db
    ports:
    - protocol: TCP
      port: 5432
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: db-policy
  namespace: guestbook
spec:
  podSelector:
    matchLabels:
      app: guestbook-db
  policyTypes: ["Ingress"]
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: guestbook
    ports:
    - protocol: TCP
      port: 5432
```

Gotcha worth noticing in `allow-dns`: `namespaceSelector` and `podSelector` in **one list element** means AND (kube-dns pods in kube-system). As two separate `-` elements it would mean OR (all of kube-system, plus kube-dns-labeled pods *in this namespace*). One dash changes the meaning.
</details>

Save all three in one file `guestbook-policies.yaml`, apply, and watch the app heal:

```sh
kubectl apply -f guestbook-policies.yaml
kubectl get pods -n guestbook -w     # api pods back to 1/1 (DNS works, db reachable)
curl -s http://guestbook.localhost:8080/entries   # entries again, through ingress
```

#### 7. The test matrix

Prove each flow's status — allowed paths work, everything else still drops:

```sh
# ✓ end-to-end through ingress-nginx:
curl -s -o /dev/null -w "%{http_code}\n" http://guestbook.localhost:8080/entries     # 200

# ✗ random pod in default ns → api (not from ingress-nginx ns):
kubectl run tester --image=curlimages/curl --rm -it --restart=Never -- \
  curl -sS --max-time 5 http://guestbook.guestbook.svc/entries                       # timeout

# ✗ random pod → db directly:
kubectl run tester --image=busybox --rm -it --restart=Never -- \
  nc -zv -w 3 guestbook-db.guestbook.svc 5432                                        # timeout

# ✗ api pod egress to anywhere but db+DNS (exec a curl FROM the api? no shell in guestbook —
#   prove it from the pod network instead): a curl pod INSIDE guestbook ns:
kubectl run tester -n guestbook --image=curlimages/curl --rm -it --restart=Never -- \
  curl -sS --max-time 5 http://podlab.localhost:8080/                                # blocked: tester has no egress except DNS
```

In k9s, `:networkpolicies` shows the four policies; `describe` renders the rule sets readably — handy when a policy "should" match and doesn't.

## Verify ✅

- [ ] `kubectl get nodes` → 3 nodes `Ready`; `kubectl get ds cilium -n kube-system` → `DESIRED 3, READY 3`
- [ ] `helm list -n kube-system` shows the `cilium` release deployed
- [ ] `kubectl top nodes` works (metrics-server back) and `kubectl get pods -n ingress-nginx` shows the controller Ready
- [ ] With only `default-deny-all` applied: api pods `0/1`, logs show DNS/connection failures, external curl → 503
- [ ] With all policies applied: `curl http://guestbook.localhost:8080/entries` → 200 with your post-rebuild entry
- [ ] Tester pod in `default` ns times out against both `guestbook.guestbook.svc` and `guestbook-db.guestbook.svc:5432`
- [ ] `kubectl get netpol -n guestbook` → `default-deny-all`, `allow-dns`, `api-policy`, `db-policy`

## CKA corner 🎓

NetworkPolicy questions are formulaic: "deny all in namespace X", "allow only pods labeled A to reach pods labeled B on port N". Speed comes from having the default-deny skeleton and the from/to shapes memorized — and from remembering `kubectl explain networkpolicy.spec.ingress.from` when you blank. Two traps: empty `podSelector: {}` = *all pods* (not none); and `policyTypes` decides whether egress is restricted at all — a policy with only ingress rules but `policyTypes: ["Ingress","Egress"]` silently deny-alls egress.

**Drill 1 (4 min).** In namespace `web` (create it), deny all ingress to all pods, but leave egress untouched. Verify with a busybox `wget` between two nginx pods.

<details><summary>Solution</summary>

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-ingress
  namespace: web
spec:
  podSelector: {}
  policyTypes: ["Ingress"]
```
```sh
kubectl create ns web
kubectl run a --image=nginx -n web; kubectl run b --image=busybox -n web --restart=Never -- sleep 600
kubectl apply -f deny-ingress.yaml
kubectl exec -n web b -- wget -qO- --timeout=3 http://$(kubectl get pod a -n web -o jsonpath='{.status.podIP}')   # times out
# egress from b to the internet/kube-dns still works (no Egress in policyTypes)
```
</details>

**Drill 2 (6 min).** Allow pods labeled `role=frontend` in namespace `web` to reach pods labeled `role=backend` in namespace `api` on TCP 6379. Whose namespace does the policy live in?

<details><summary>Solution</summary>

The **destination's** — policies protect the pods they select, so it's an *ingress* policy in `api`:
```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: backend-from-frontend
  namespace: api
spec:
  podSelector:
    matchLabels:
      role: backend
  policyTypes: ["Ingress"]
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: web
      podSelector:
        matchLabels:
          role: frontend
    ports:
    - protocol: TCP
      port: 6379
```
(If `web` also has egress-deny, a matching egress policy is needed there too — the exam usually states which side is locked.)
</details>

**Drill 3 (2 min).** A team applies a correct default-deny policy on a kindnet/flannel-without-policy cluster and traffic still flows. Why, and how do you check?

<details><summary>Solution</summary>

The CNI doesn't implement NetworkPolicy — the object is stored but unenforced. Check what CNI runs (`kubectl get pods -n kube-system | grep -iE 'cilium|calico|kindnet|flannel'`) and consult its docs; there is no API-level signal that a policy is being enforced. Trust, but `curl`.
</details>

## Stretch goals

- **Hubble** — see verdicts live. Install the cilium CLI (`brew install cilium-cli`), then `cilium hubble enable`, `cilium hubble port-forward &`, `brew install hubble`, and `hubble observe --namespace guestbook --verdict DROPPED` while re-running the test matrix. Watching your own packets get verdict-stamped `DROPPED by policy` beats any diagram.
- Run `cilium connectivity test` — Cilium's own end-to-end test suite against your cluster.
- Add an egress policy allowing the api pods HTTPS (443) to `0.0.0.0/0` via `ipBlock`, excluding the pod CIDR — the "may call external APIs but nothing internal" pattern.

## Cleanup

- Delete drill resources: `kubectl delete ns web` (if created).
- **The four policies on `guestbook` STAY** — a locked-down namespace is the realistic steady state, and later days must work within it.
- **The cluster now runs Cilium for the rest of the course.** Keep `kind-config-cilium.yaml` in this folder — it is now the canonical config if you ever rebuild again.
