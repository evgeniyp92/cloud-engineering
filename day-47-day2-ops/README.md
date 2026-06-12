# Day 47 — Day-2 Operations

> **Time:** ~3.5 h · **Builds on:** Days 8, 18, 23, 39

## Objectives

- Upgrade your kubectl game with krew plugins: ownership trees, capacity views, clean YAML
- Right-size workloads from measured usage with Goldilocks + VPA instead of guessing
- Drain a node safely and watch a PodDisruptionBudget refuse an eviction that would break availability
- Explain how real cluster upgrades work (kubeadm flow, version skew) and how priority/preemption resolves resource pressure

## Concepts

### Day-2 is the job

Day-0 is design, day-1 is install, **day-2 is everything after**: upgrades, capacity management, node maintenance, resource pressure. Install tutorials are everywhere; day-2 competence is what interviews actually probe, because it's where outages live. The questions in this lesson — "how do you set requests?", "how do you patch a node without an outage?", "what happens when a node runs out of memory?" — all have the same shape: Kubernetes gives you a mechanism, and your job is to configure the *policy* so routine operations don't become incidents.

Two vocabulary items carry most of today:

**Voluntary vs involuntary disruptions.** Involuntary: hardware death, kernel panic, OOM, accidental VM deletion — nobody asked, your defense is replicas + anti-affinity + requests. Voluntary: *you* (or your cloud's upgrade automation) deliberately evicting pods — drains, scale-downs, cluster upgrades. Voluntary disruptions go through the **Eviction API**, and that's the hook: a **PodDisruptionBudget (PDB)** says "at most this much of my app may be voluntarily disrupted at once," and the Eviction API returns `429` rather than violate it. A drain will sit and retry — annoying by design — until evicting is safe. PDBs do nothing against involuntary disruptions; OOM doesn't ask permission.

**Measured, not guessed.** On Day 8 you set podlab's requests by squinting at `kubectl top`. The grown-up answer is the **VPA recommender**: it continuously builds a usage histogram per container and computes recommended requests (lower bound / target / upper bound). You don't have to let VPA *apply* anything (its auto-update mode restarts pods and fights HPA on the same resource); run it in recommender-only mode and read the numbers. **Goldilocks** is a dashboard over those recommendations — label a namespace, get a per-deployment "you asked for 250m, you use 12m" report. Requests drive scheduling and bin-packing; systematic over-requesting is the most common way companies pay triple for idle clusters.

### Upgrades, honestly

Real clusters upgrade **one minor version at a time**, control plane first:

```
kubeadm upgrade plan                 # what can I go to?
kubeadm upgrade apply v1.34.x        # first control-plane node
kubeadm upgrade node                 # remaining control-plane nodes
# then per worker: drain → upgrade kubelet/kubectl packages → restart kubelet → uncordon
```

The **version skew policy** makes the ordering non-negotiable: kubelet may be up to **3 minor versions older** than the apiserver, never newer; kube-proxy same; controller-manager/scheduler at most 1 older. Control plane first is not a convention, it's the only legal order. Before any upgrade you also check for **deprecated APIs**: manifests in git using removed versions will fail to apply after the bump — `pluto detect-files` (Day 23) against your gitops repo is the pre-flight.

On kind, the node image *is* the version — "upgrading" is creating a new cluster from a new image. That's an honest local limitation; it's also accidentally on-message: Day 48 proves your cluster is disposable anyway. You'll do today's upgrade work as a paper drill, which conveniently is also how the CKA tests it.

### Pressure and priority

When a node runs low on memory/disk, the **kubelet** (not the scheduler) starts **node-pressure eviction**: BestEffort and over-request bursting pods go first — your Day 8 QoS classes are the kill order. When the *scheduler* can't fit a new pod anywhere, **priority and preemption** kick in: a pod whose `PriorityClass` value beats running pods' can have them evicted to make room. That's how cluster-critical components (CNI, DNS — look at their priorityClassName) survive a packed cluster, and how you guarantee "the payment service schedules even if batch jobs must die."

## Lab

### 1. kubectl productivity: krew

```sh
brew install krew
# add to your shell rc, then restart the shell:
export PATH="${KREW_ROOT:-$HOME/.krew}/bin:$PATH"
kubectl krew install tree neat view-secret resource-capacity
```

Take each for a lap:

```sh
# tree: ownership graphs — finally SEE controller chains
kubectl tree rollout podlab -n podlab-prod        # Rollout → ReplicaSets → Pods
kubectl tree deployment guestbook -n guestbook

# neat: YAML without managedFields/status noise — for committing to git
kubectl get deploy guestbook -n guestbook -o yaml | kubectl neat

# view-secret: stop typing base64 -d
kubectl view-secret guestbook-db -n guestbook DATABASE_URL

# resource-capacity: requests/limits vs allocatable, per node — instant capacity review
kubectl resource-capacity
kubectl resource-capacity --pods --util -n podlab-prod
```

Quality-of-life while you're here (add to `~/.zshrc`):

```sh
alias k=kubectl
export KUBE_EDITOR="code --wait"     # or vim; used by kubectl edit
# you already have kubectx/kubens from Day 1 — krew's ctx/ns plugins are the same tools
```

`kubectl resource-capacity` answers in one command what took you a jsonpath safari on Day 8. Note your cluster's totals — you'll compare against Goldilocks shortly.

### 2. Right-sizing with Goldilocks

VPA first, recommender-only (Fairwinds publishes a chart for exactly this):

```sh
helm repo add fairwinds-stable https://charts.fairwinds.com/stable
helm install vpa fairwinds-stable/vpa -n vpa --create-namespace \
  --set recommender.enabled=true \
  --set updater.enabled=false \
  --set admissionController.enabled=false
helm install goldilocks fairwinds-stable/goldilocks -n goldilocks --create-namespace
```

(Updater off = it never restarts your pods; admission controller off = it never mutates them. We want opinions, not actions. GitOps-purist option: make both into Applications in `argocd/apps/` — recommended if you want them back after Day 48.)

Opt namespaces in by label, then give the recommender time to observe:

```sh
kubectl label ns podlab-prod goldilocks.fairwinds.com/enabled=true
kubectl label ns guestbook goldilocks.fairwinds.com/enabled=true
kubectl get vpa -n podlab-prod      # goldilocks created one per workload
```

Generate some honest load so the histogram has data (Day 31's traffic script, or):

```sh
for i in $(seq 1 300); do curl -s http://podlab.prod.localhost:8080/ > /dev/null; sleep 1; done &
```

After ~15–30 minutes, open the dashboard:

```sh
kubectl port-forward -n goldilocks svc/goldilocks-dashboard 8081:80
open http://localhost:8081
```

For each container you get recommended requests/limits per QoS goal ("Guaranteed" vs "Burstable" columns) next to what's currently set. Compare with your Day 8 hand-tuning: were you over or under? This is the answer to the interview question "how do you set resource requests?" — **measure under representative load, set requests near the recommendation, revisit periodically.** Don't blindly apply lab numbers though: 30 minutes of synthetic traffic is not a week of production traffic, and the recommender's lower bounds early on are aggressive. The method is the lesson, not today's millicores.

### 3. Node maintenance: cordon, drain, and the PDB that says no

Make sure podlab-prod runs 3 replicas (scale via git if your overlay says otherwise — selfHeal will revert kubectl). Then protect it. Write `pdb.yaml` yourself: a PodDisruptionBudget named `podlab`, namespace `podlab-prod`, `minAvailable: 2`, selecting podlab's pods.

<details><summary>Solution</summary>

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: podlab
  namespace: podlab-prod
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: podlab        # match your overlay's pod labels — check with: kubectl get pods -n podlab-prod --show-labels
```

(Commit it to the prod overlay in `k8s-gitops` — a PDB is exactly the kind of thing that must survive Day 48.)
</details>

```sh
kubectl apply -f pdb.yaml      # or push via git
kubectl get pdb -n podlab-prod # ALLOWED DISRUPTIONS: 1  (3 healthy − 2 required)
```

**Cordon** marks a node unschedulable — running pods stay, new ones won't land:

```sh
kubectl cordon course-worker2
kubectl get nodes               # course-worker2: Ready,SchedulingDisabled
```

**Drain** = cordon + evict everything evictable:

```sh
kubectl drain course-worker2 --ignore-daemonsets --delete-emptydir-data
```

(`--ignore-daemonsets` because DaemonSet pods — Cilium, monitoring agents — would just be recreated on the same node; the flag acknowledges they stay.) Watch evicted pods reschedule onto worker1, and check the PDB held: at no point did podlab-prod drop below 2 ready.

Now the interesting failure. Uncordon, then engineer a violation — scale podlab-prod to 2 replicas (in git; let it sync) so `ALLOWED DISRUPTIONS` becomes 0, and ensure at least one podlab pod is on worker2 (delete a pod until one lands there — it's cordon-free now). Then:

```sh
kubectl get pdb -n podlab-prod                      # ALLOWED DISRUPTIONS: 0
kubectl drain course-worker2 --ignore-daemonsets --delete-emptydir-data
```

The drain **hangs**, retrying with `error when evicting pod ... Cannot evict pod as it would violate the pod's disruption budget` (an HTTP 429 under the hood). This is the system working: the Eviction API is refusing to make your app unavailable, and the drain politely keeps asking. In a managed cloud, this exact mechanism is what makes automated node upgrades wait for your app. Resolve it the right way — add capacity, not force:

```sh
# in another terminal: bump prod replicas back to 3 in git, push, wait for sync
kubectl get pdb -n podlab-prod        # ALLOWED DISRUPTIONS: 1 → the hanging drain proceeds
kubectl uncordon course-worker2
```

(The wrong way, `--disable-eviction`, bypasses the Eviction API with plain deletes. Know it exists for true emergencies; never lead with it.)

### 4. Upgrades: the paper drill

Nothing to execute on kind, so do what the CKA expects you to know cold — see the CKA corner below. The one executable piece is the deprecation pre-flight you'd run before any real upgrade:

```sh
pluto detect-files -d ~/Code/k8s-gitops/    # Day 23 tool: flags removed/deprecated apiVersions
kubectl version                              # server vs client skew — keep kubectl within ±1 of the apiserver
```

### 5. Resource pressure: priority and preemption

Build the "payments beats batch" guarantee in miniature:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: high-priority
value: 1000000
description: "Schedules even if lower-priority pods must be preempted."
EOF
kubectl get priorityclass    # note the built-ins: system-cluster-critical (2 billion), system-node-critical
```

Fill the cluster with low-priority ballast (default priority 0). Size the request so ~3 fit per worker — check `kubectl resource-capacity` and adjust:

```sh
kubectl create ns pressure-lab
kubectl create deploy ballast -n pressure-lab --image=registry.k8s.io/pause:3.9 --replicas=6
kubectl set resources deploy ballast -n pressure-lab --requests=cpu=1
kubectl get pods -n pressure-lab    # some may already be Pending — good, the cluster is full
```

Now a high-priority pod arrives needing room:

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: vip
  namespace: pressure-lab
spec:
  priorityClassName: high-priority
  containers:
  - name: app
    image: registry.k8s.io/pause:3.9
    resources:
      requests:
        cpu: "1"
EOF
kubectl get pods -n pressure-lab -w
```

Watch a ballast pod get **terminated mid-life** and `vip` schedule into the hole. Confirm the story in events:

```sh
kubectl get events -n pressure-lab --sort-by=.lastTimestamp | grep -i preempt
kubectl describe pod vip -n pressure-lab | grep -i nominated
```

Preemption is a *scheduler* action on `requests` math; contrast with kubelet node-pressure eviction, which acts on *actual usage* when a node hits `memory.available`/`imagefs.available` thresholds and kills in QoS order (BestEffort → Burstable-over-request → Guaranteed last). Two different reapers; interviews love asking you to tell them apart.

## Verify ✅

- [ ] `kubectl krew list` → includes `neat`, `tree`, `view-secret`, `resource-capacity`
- [ ] `kubectl tree rollout podlab -n podlab-prod` prints Rollout → ReplicaSet → Pod hierarchy
- [ ] Goldilocks dashboard shows recommendations for `podlab-prod` (VPA objects exist: `kubectl get vpa -n podlab-prod` non-empty)
- [ ] `kubectl get pdb podlab -n podlab-prod` → `MIN AVAILABLE 2`, allowed disruptions computed
- [ ] With 2 replicas: drain of worker2 hangs with `would violate the pod's disruption budget`; with 3 replicas it completes
- [ ] `kubectl get nodes` → all nodes `Ready` (no `SchedulingDisabled` left over)
- [ ] `kubectl describe pod vip -n pressure-lab | grep -i priority` → `Priority: 1000000`, and a ballast pod was preempted (event log shows it)

## CKA corner 🎓

Drain/cordon and PDBs are exam staples, and there's reliably an upgrade question. The upgrade one is mechanical if you've memorized the sequence — recite it now, on paper:

**The kubeadm upgrade sequence (control plane node):**

1. `kubectl drain <cp-node> --ignore-daemonsets`
2. `apt-get update && apt-get install -y kubeadm=1.34.x-*` (upgrade kubeadm first — it drives the rest)
3. `kubeadm upgrade plan` → `kubeadm upgrade apply v1.34.x`
4. `apt-get install -y kubelet=1.34.x-* kubectl=1.34.x-*` then `systemctl daemon-reload && systemctl restart kubelet`
5. `kubectl uncordon <cp-node>`

**Per worker:** drain (from a machine with kubectl access) → upgrade kubeadm → `kubeadm upgrade node` → upgrade kubelet/kubectl + restart kubelet → uncordon. One minor version per hop; kubelet ≤ apiserver, max n-3 behind.

**Drill 1 — safe maintenance (6 min).** `course-worker` needs a (pretend) kernel patch. Without violating the podlab PDB: make it unschedulable, evict all evictable pods, verify nothing except DaemonSet pods remains, then return it to service.

<details><summary>Solution</summary>

```sh
kubectl drain course-worker --ignore-daemonsets --delete-emptydir-data
kubectl get pods -A -o wide --field-selector spec.nodeName=course-worker   # only DaemonSet pods (cilium, etc.)
kubectl uncordon course-worker
```

`drain` cordons implicitly — a separate `cordon` first is fine but not required. If it hangs on a PDB, the fix is capacity (scale the app up) not force. `--delete-emptydir-data` is needed when pods use emptyDir volumes; the exam will tell you data loss is acceptable.
</details>

**Drill 2 — PDB math (4 min).** A deployment has 4 replicas and a PDB with `maxUnavailable: 25%`. How many pods can a drain evict simultaneously? What changes if 1 pod is already crash-looping? Write a PDB for it without a manifest file.

<details><summary>Solution</summary>

25% of 4 = 1 → one voluntary eviction allowed. If a pod is already unhealthy, the budget counts *healthy* pods: 3 healthy, required 3 (4−1) → **zero** evictions allowed; the drain blocks until the crash-loop is fixed. Imperative creation:

```sh
kubectl create pdb mypdb --selector=app=myapp --max-unavailable=25% -n myns
```
</details>

**Drill 3 — skew triage (3 min, no cluster).** Apiserver is v1.34. Which of these kubelets are legal: v1.35, v1.34, v1.32, v1.30? In what order do you upgrade apiserver, controller-manager, kubelet, kubectl?

<details><summary>Solution</summary>

Legal kubelets: v1.34, v1.32 (within n-3: 1.31+ are fine), v1.30 is **too old**, v1.35 is **illegal** (kubelet must never be newer than the apiserver). Order: apiserver first → controller-manager/scheduler (≤1 behind apiserver) → kubelets (≤3 behind) → kubectl anywhere within ±1 of the apiserver.
</details>

## Stretch goals

- Commit the PDB, the PriorityClass, and Goldilocks/VPA Applications into `k8s-gitops` so Day 48's bootstrap restores your day-2 posture too.
- Apply Goldilocks's recommended requests for podlab to the prod overlay (via git), then re-run `kubectl resource-capacity --util` and compare cluster headroom before/after.
- Add a PDB for guestbook — then reason through why a PDB on a **1-replica** StatefulSet (`minAvailable: 1`) is a foot-gun: every drain of its node blocks forever. What's the actual fix? (Replicas — or yesterday's CNPG, which manages PDBs for you; look: `kubectl get pdb -n cnpg-lab` if you kept it.)
- Explore `kubectl drain`'s lesser-known flags: `--grace-period`, `--timeout`, `--pod-selector`. When would you drain only *some* pods?

## Cleanup

- `kubectl delete ns pressure-lab` and `kubectl delete priorityclass high-priority` (or keep the PriorityClass — in git — if you did the stretch).
- `kubectl uncordon` anything still cordoned — check `kubectl get nodes` now, not on Day 48.
- Keep krew + plugins, Goldilocks/VPA (they're cheap; delete the helm releases if RAM is tight and they're not in git), and the podlab PDB (in git).
