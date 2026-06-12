# Day 04 — Services & DNS

> **Time:** ~3.5 h · **Builds on:** Days 1, 3

## Objectives

- Explain why pod IPs can't be addressed directly, and how a Service's stable virtual IP fixes it.
- Resolve services through CoreDNS using all three name forms, and describe what kube-proxy actually programs.
- Read Endpoints/EndpointSlices and show a not-ready pod dropping out of them.
- Expose podlab via ClusterIP, NodePort, and (with cloud-provider-kind) a real LoadBalancer.

## Concepts

### The problem: pods are cattle with random IPs

Day 3 proved pods get replaced constantly — every replacement gets a **new IP**. Hardcode a pod IP into a client and it's stale within one rollout. And with 3 replicas, *which* IP would you even pick? You need (1) a stable name, (2) a stable virtual IP, (3) load-balancing across whatever pods currently match. That triple is a **Service**.

A Service is gloriously simple under the hood: a label **selector** plus a **ClusterIP** (a virtual IP allocated from the service CIDR, attached to no machine anywhere). Two controllers make it work:

- The **endpoints machinery** continuously evaluates the selector and writes the matching, *ready* pod IPs into **EndpointSlice** objects. Service selects; EndpointSlices list the current truth.
- **kube-proxy** on every node watches Services + EndpointSlices and programs the node's kernel.

### How the virtual IP actually works (kube-proxy)

There is no process listening on a ClusterIP. In the default iptables mode, kube-proxy writes NAT rules on **every node**, roughly:

```
packet to 10.96.143.7:80 (ClusterIP)
  → iptables: pick one backend at random
      ├─ 1/3 chance → DNAT to 10.244.1.5:8080
      ├─ 1/3 chance → DNAT to 10.244.2.8:8080
      └─ 1/3 chance → DNAT to 10.244.2.9:8080
```

The packet's destination is rewritten *in the sender's own kernel* before it ever leaves the node. Consequences worth knowing: you can't ping a ClusterIP (no ICMP rules, nothing answers), load-balancing is per-*connection* not per-request, and `kubectl get svc` showing a ClusterIP says nothing about whether traffic works — only EndpointSlices do. (IPVS and the newer nftables mode exist for scale; same model.)

### DNS: CoreDNS names every service

**CoreDNS** (running in `kube-system` — you saw it in k9s on Day 1) watches Services and serves records of the form `<svc>.<namespace>.svc.cluster.local` → ClusterIP. Every container's `/etc/resolv.conf` points at CoreDNS's own ClusterIP and carries search domains, so three spellings work from a pod in `default`:

| You write | Resolves because |
|---|---|
| `podlab` | search domain `default.svc.cluster.local` is appended |
| `podlab.default` | search domain `svc.cluster.local` is appended |
| `podlab.default.svc.cluster.local` | fully qualified |

Rule of thumb: short name within a namespace, `svc.ns` across namespaces (that's Day 9's cross-namespace traffic), FQDN in config files that must survive being moved around.

### Service types are a ladder, not alternatives

| Type | Adds | Reachable from |
|---|---|---|
| `ClusterIP` | virtual IP + DNS | inside the cluster only |
| `NodePort` | a port (30000–32767) opened on **every node**, forwarding to the ClusterIP machinery | anything that can reach a node IP |
| `LoadBalancer` | asks a **cloud controller** for an external LB pointing at the NodePorts | the outside world |

Each type *includes* the previous one. On bare kind there is no cloud, so `LoadBalancer` services sit at `EXTERNAL-IP <pending>` forever — until you run **cloud-provider-kind**, a tiny local implementation of the cloud side that watches for LoadBalancer services and wires up a reachable IP. Same control loop a real cloud runs; you get to see the contract with no cloud bill. (For HTTP, Day 5's Ingress is the better tool — one entry point, many services.)

A fourth shape, the **headless** service (`clusterIP: None`), skips the virtual IP entirely: DNS returns the *pod IPs themselves*. That's for clients that must talk to specific peers — StatefulSets, Day 11. Today you just look at one.

## Lab

### 1. Replace the quick service with a hand-written one

Day 3 left the `podlab` Deployment (3 replicas) and a `kubectl expose` service running. Delete the service and write it properly — requirements for `svc.yaml`:

- Service `podlab`, type ClusterIP (the default — omit `type`)
- selector `app: podlab`; port `80` → targetPort `8080`, name the port `http`

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Service
metadata:
  name: podlab
spec:
  selector:
    app: podlab
  ports:
    - name: http
      port: 80
      targetPort: 8080
```

</details>

```sh
kubectl delete svc podlab
kubectl apply -f svc.yaml
kubectl get svc podlab        # note the CLUSTER-IP — yours will differ from any example
```

### 2. Use it from inside the cluster

ClusterIPs only exist inside; spin up a throwaway client pod:

```sh
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- sh
```

Inside:

```sh
curl -s podlab/ | grep -o '"hostname":"[^"]*"'
curl -s podlab/ ; curl -s podlab/ ; curl -s podlab/    # hostname varies → load-balancing is real
curl -s podlab.default/healthz
curl -s podlab.default.svc.cluster.local/
cat /etc/resolv.conf                                    # the search-domain trick, in the flesh
nslookup podlab                                         # A record = the ClusterIP from step 1
exit
```

Run each curl a few times and watch `hostname` rotate across the three pod names — that's iptables picking different backends per connection.

### 3. Look at what the selector produced

```sh
kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide
kubectl get pods -l app=podlab -o wide
```

The slice's three addresses are exactly the three pod IPs. Now break the selector on purpose and watch the service die *while looking perfectly healthy*:

```sh
kubectl patch svc podlab -p '{"spec":{"selector":{"app":"typo"}}}'
kubectl get endpointslices -l kubernetes.io/service-name=podlab   # endpoints empty
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- curl -s --max-time 3 podlab/
# times out — DNS resolves, ClusterIP exists, zero backends
kubectl patch svc podlab -p '{"spec":{"selector":{"app":"podlab"}}}'
```

Burn this in: **"service not working" is almost always "EndpointSlices empty"**, and that's almost always a selector/label mismatch or pods not Ready. This is the single most common CKA troubleshooting question and the most common real-world one.

### 4. Readiness controls membership (Day 10 preview)

Only **Ready** pods are listed as ready endpoints. Make one pod unready by flipping podlab's health and adding a readiness probe? — not yet; today, see the simpler version: scale and watch slices track it.

```sh
kubectl scale deployment podlab --replicas=2
kubectl get endpointslices -l kubernetes.io/service-name=podlab -o wide   # 2 addresses now
kubectl scale deployment podlab --replicas=3
```

Day 10 does the full demo where a *live but unready* pod leaves the slice while still running.

### 5. NodePort

```sh
kubectl patch svc podlab -p '{"spec":{"type":"NodePort"}}'
kubectl get svc podlab    # PORT(S) now "80:3XXXX/TCP" — note your nodePort
```

Every node now answers on that port, even nodes running zero podlab pods. macOS can't reach the node IPs (Day 1 concepts), but the nodes can reach each other — prove it via docker:

```sh
NODEPORT=$(kubectl get svc podlab -o jsonpath='{.spec.ports[0].nodePort}')
WORKER_IP=$(kubectl get node course-worker -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
docker exec course-worker2 curl -s http://$WORKER_IP:$NODEPORT/ | head -c 200; echo
docker exec course-control-plane curl -s http://$WORKER_IP:$NODEPORT/healthz; echo
```

Hitting `course-worker`'s IP works from any node and load-balances to *all* podlab pods, wherever they run — kube-proxy on the receiving node forwards into the same ClusterIP machinery.

### 6. LoadBalancer with cloud-provider-kind

```sh
brew install cloud-provider-kind
```

In a **separate terminal**, leave it running (it needs root on macOS to make the LB endpoint reachable from your host):

```sh
sudo cloud-provider-kind
```

Then:

```sh
kubectl patch svc podlab -p '{"spec":{"type":"LoadBalancer"}}'
kubectl get svc podlab --watch    # EXTERNAL-IP: <pending> → an actual IP. Ctrl-C.
LB_IP=$(kubectl get svc podlab -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -s http://$LB_IP/ | python3 -m json.tool
```

You just watched the LoadBalancer contract execute: service asks, cloud controller provisions (here: a container running an envoy proxy — `docker ps | grep kindccm`), controller writes the IP back into `status.loadBalancer`. On EKS/GKE the only difference is *what* gets provisioned.

### 7. Headless preview

```sh
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- nslookup podlab
```

One A record — the ClusterIP. Now create a headless twin and compare:

```sh
kubectl create service clusterip podlab-headless --clusterip=None --tcp=80:8080
kubectl patch svc podlab-headless -p '{"spec":{"selector":{"app":"podlab"}}}'
kubectl run client --rm -it --image=curlimages/curl --restart=Never -- nslookup podlab-headless
```

**Three** A records — the pod IPs, no virtual IP in between. Park the thought until Day 11 (StatefulSets), then delete it (see Cleanup).

## Verify ✅

- [ ] `kubectl get svc podlab -o jsonpath='{.spec.type}'` → `LoadBalancer` (after step 6) with a non-pending `EXTERNAL-IP`
- [ ] From the client pod: `nslookup podlab` returns the ClusterIP; `curl -s podlab/` returns podlab JSON; repeated curls show ≥2 distinct `hostname` values
- [ ] `kubectl get endpointslices -l kubernetes.io/service-name=podlab -o jsonpath='{.items[0].endpoints[*].addresses[0]}'` lists exactly the IPs from `kubectl get pods -l app=podlab -o wide`
- [ ] `docker exec course-worker2 curl -s http://<worker-ip>:<nodeport>/healthz` → `{"status":"ok"}`
- [ ] `curl http://$LB_IP/` from your Mac returns podlab JSON while `cloud-provider-kind` runs

## CKA corner 🎓

Exam notes:

- "Service not routing" playbook, in order: ① `kubectl get endpointslices -l kubernetes.io/service-name=X` — empty? ② `kubectl get svc X -o yaml` → compare `spec.selector` against `kubectl get pods --show-labels`. ③ Selector fine? Check pods are `Ready` and `targetPort` matches the container's actual port. 90% of cases end at ②.
- `kubectl expose deployment x --port=80 --target-port=8080` is the fast generator; add `--type=NodePort` when asked.
- You can't set a *specific* nodePort imperatively — generate with `--dry-run=client -o yaml`, add `nodePort: 30080`, apply.

**Drill 1 (3 min):** A deployment `shop` (labels `app=shop`) exists; service `shop-svc` returns connection timeouts. Find and fix the fault. Set it up first: `kubectl create deployment shop --image=nginx --replicas=2 && kubectl create service clusterip shop-svc --tcp=80:80` (note: this service's selector is `app=shop-svc` — that's the planted bug).

<details><summary>Solution</summary>

```sh
kubectl get endpointslices -l kubernetes.io/service-name=shop-svc   # no endpoints
kubectl get svc shop-svc -o jsonpath='{.spec.selector}'             # {"app":"shop-svc"}
kubectl get pods --show-labels | grep shop                          # app=shop
kubectl patch svc shop-svc -p '{"spec":{"selector":{"app":"shop"}}}'
kubectl get endpointslices -l kubernetes.io/service-name=shop-svc   # 2 endpoints — fixed
kubectl delete deploy shop && kubectl delete svc shop-svc
```

</details>

**Drill 2 (2 min):** Expose deployment `podlab` on nodePort `30080` exactly, service name `podlab-np`, then delete it.

<details><summary>Solution</summary>

```sh
kubectl expose deployment podlab --name=podlab-np --port=80 --target-port=8080 \
  --type=NodePort --dry-run=client -o yaml > np.yaml
# edit: add  nodePort: 30080  under the ports entry
kubectl apply -f np.yaml
kubectl get svc podlab-np   # 80:30080/TCP
kubectl delete svc podlab-np
```

</details>

## Stretch goals

- `docker exec course-worker iptables-save | grep podlab` — read the actual NAT chains kube-proxy wrote (look for `KUBE-SVC-` and the random-probability `KUBE-SEP-` rules).
- Set `sessionAffinity: ClientIP` on the service and show repeated curls from one client pod now stick to one hostname.
- Create a service with **no selector** plus a manual EndpointSlice pointing at an external IP — that's how clusters name out-of-cluster databases.
- `kubectl -n kube-system get configmap coredns -o yaml` — read the Corefile; find the `kubernetes` plugin block that serves `cluster.local`.

## Cleanup

```sh
kubectl delete svc podlab-headless
kubectl patch svc podlab -p '{"spec":{"type":"ClusterIP"}}'   # back to ClusterIP; Ingress takes over tomorrow
```

Stop `sudo cloud-provider-kind` (Ctrl-C) — re-run it any time you want LoadBalancer services. **Keep:** the `podlab` Deployment and Service — Day 5 puts an Ingress in front of them.
