# Day 44 — Troubleshooting Gauntlet 2: Cluster-Level Triage

> **Time:** ~4 h · **Builds on:** Days 17 (gauntlet 1), 4 (DNS), 14 (RBAC), 15 (NetworkPolicies), 26 (ArgoCD), 40 (TLS)

## Objectives

- Internalize a **top-down triage method** for cluster-level failures — and stop debugging apps that aren't broken.
- Diagnose five blind breakages (DNS, network policy, RBAC, scheduling, ingress/TLS) inside a 20-minute time box each.
- Practice reading the three most informative artifacts in Kubernetes: pod **Events**, **controller logs**, and **`kubectl auth can-i`**.
- For every failure, name the alert that should have caught it — closing the loop with Phase 5.

## Concepts

### Workload debugging vs cluster debugging

Day 17 drilled workload-level failures: a bad image, a wrong selector, a failing probe — problems *inside* one app, found by staring at that app. Today's failures live in the **substrate**: DNS, the network fabric, authorization, scheduling, the ingress path. Their signature is different and recognizing it is half the skill:

| Workload problem smells like | Substrate problem smells like |
|---|---|
| one app broken, others fine | *multiple unrelated* things broken at once |
| errors mention the app's own logic | errors mention timeouts, lookups, "forbidden", "pending" |
| fixed by changing the app | the app was never wrong |

The cardinal sin of cluster triage is **starting at the app**. You'll burn the whole time box reading guestbook logs when guestbook's only crime was needing DNS. Hence:

### The top-down method

Work the layers in order. Each step is cheap; each *rules out* an entire class before you descend.

```
1. CONTROL PLANE   kubectl get --raw /readyz?verbose      api server + etcd ok?
                   kubectl get nodes                       all Ready?
                   kubectl get pods -n kube-system         anything crashing/missing?
        │ healthy ▼
2. DNS             kubectl run test --rm -it --image=busybox:1.36 \
   (it's always      --restart=Never -- nslookup kubernetes.default
    DNS)           name fails but pod-IP works ⇒ it was, in fact, DNS
        │ resolves ▼
3. NETWORK PATH    can pod A reach pod B's IP? (CNI)       kubectl get netpol -A
                   does the ingress path work end-to-end?  (policy? controller? cert?)
        │ flows ▼
4. AUTHZ           controller logs full of "Forbidden"?    kubectl auth can-i ... --as=system:serviceaccount:NS:NAME
        │ permitted ▼
5. THE APP         only now — Day 17 rules apply
```

Why this order? **Dependency direction.** Apps depend on DNS, the network, and authz; none of those depend on the app. Checking in dependency order means a single pass with no backtracking. And step 2 gets its own layer because in practice an outsized share of "everything is weird" incidents are DNS — it's the one service literally everything calls, constantly, implicitly.

Three artifacts do most of the diagnostic work today:

- **Events** (`kubectl describe pod`, bottom; or `kubectl get events --sort-by=.lastTimestamp`). The scheduler, kubelet, and controllers write their complaints here in full sentences. A Pending pod's event *names every node and why it refused*.
- **Controller logs.** When a controller (ArgoCD, cert-manager, the operator du jour) "does nothing", the reason is in *its* logs, not in the resources it failed to touch. UIs summarize; logs accuse.
- **`kubectl auth can-i --as=`** — test any ServiceAccount's permissions in one line, no token games: hypothesis → verification in ten seconds.

### Localization: the differential

When something is broken, what's *not* broken is data. Prod broken + dev fine + same manifests ⇒ the difference lives in the namespace, not the code. All URLs dead ⇒ shared layer. One node's pods dead ⇒ that node. Asking "what do the broken things share that the working things don't?" is usually faster than any log. You'll use this consciously in at least three of today's five.

### The first 60 seconds

Before any hypothesis, run the sweep — four commands, memorized, in this order. They cost a minute and either clear or indict layers 1–2 wholesale:

```sh
kubectl get --raw /readyz?verbose | tail -3        # control plane verdict, component by component
kubectl get nodes                                  # any NotReady?
kubectl get pods -A | grep -vE 'Running|Completed' # what's unhappy, everywhere, one screen
kubectl get events -A --sort-by=.lastTimestamp | tail -15   # the cluster's last complaints
```

Resist the urge to chase the first red thing you see — finish the sweep, *then* pick the highest layer that's abnormal. Half of bad troubleshooting is anchoring on the first symptom instead of the earliest cause.

### Time-boxing is a production skill, not an exam gimmick

The 20-minute box mirrors real incident discipline: if you're not converging, your *method* is wrong — more minutes of the same flailing won't help, but a hint (in real life: a colleague, a runbook) will. Practicing the escalate-at-the-box reflex now is what stops you from being the engineer who sits on a sev-1 alone for three hours out of pride.

### The rules of the gauntlet

Five `break-NN.sh` scripts. Run them **blind** — no peeking at the script, no `git diff`-style cheating; the script even tells you off in its header. One at a time, **fix N before breaking N+1** (some failures would mask others — DNS down makes everything else undiagnosable). 20-minute time box. [`HINTS.md`](HINTS.md) has three escalating hints per scenario; [`SOLUTIONS.md`](SOLUTIONS.md) has the full debrief — read it *after* each fix, even when you solved it clean: the prevention and monitoring sections are the part that compounds.

## Lab

### 1. Baseline — know what healthy looks like

You can't detect deviation from a baseline you never recorded:

```sh
chmod +x break-*.sh restore-all.sh
kubectl get --raw /readyz >/dev/null && echo "apiserver ok"
kubectl get nodes
kubectl get pods -A | grep -vE 'Running|Completed' || echo "all pods healthy"
curl -s -o /dev/null -w 'guestbook: %{http_code}\n' http://guestbook.localhost:8080/entries
curl -s -o /dev/null -w 'grafana:   %{http_code}\n' http://grafana.localhost:8080
kubectl get applications -n argocd
```

Everything green? Open k9s in a second terminal (you'll live in `:events`, `:pods`, and `l` today) and begin.

### 2. The loop — run for each scenario, 01 through 05

```sh
./break-01.sh        # then START A 20-MINUTE TIMER
```

Work the method. Write down — actually write, this becomes your incident log. Template (copy it five times into a scratch file now):

```
## Break NN — <date>           time to root cause: ___ min   hints used: 0/1/2/3
SYMPTOMS   (command → observed output, verbatim):
LAYER      (control plane / DNS / network / authz / app) and how I ruled out the layers above:
ROOT CAUSE (one sentence):
FIX        (commands):
ALERT      (the Prometheus rule that should have paged first):
```

Filling in "how I ruled out the layers above" is the part that builds the method into reflex — it forces the top-down pass even when you think you already know the answer.

When you believe you've fixed it, run that scenario's health check:

| # | Fixed when this passes |
|---|---|
| 01 | `kubectl run t --rm -i --image=busybox:1.36 --restart=Never -- nslookup kubernetes.default` resolves **and** `curl -s http://guestbook.localhost:8080/readyz` → `ok` |
| 02 | `kubectl run -n podlab-prod t --rm -i --image=busybox:1.36 --restart=Never -- nslookup kubernetes.default` resolves (and the same in `podlab-dev` still works) |
| 03 | `kubectl auth can-i list deployments --as=system:serviceaccount:argocd:argocd-application-controller` → `yes`, and `kubectl get apps -n argocd` returns to `Synced` within ~2 min |
| 04 | `kubectl run t --rm -i --image=busybox:1.36 --restart=Never -- true` completes (a *new* pod scheduled), and `kubectl get pods -n podlab-dev` shows no `Pending` |
| 05 | `curl -s -o /dev/null -w '%{http_code}' http://grafana.localhost:8080` → `200` or `302`, **and** `curl --cacert course-ca.crt -s -o /dev/null -w '%{http_code}' https://canary.localhost:8443/` → `200` |

Health check green → next break script. Stuck at 20 minutes → HINTS.md, level by level. Still stuck → SOLUTIONS.md, apply the fix, and treat the scenario as a worked example rather than a loss — then re-run the health check anyway.

### 3. Scoring rubric

Per scenario, score your *diagnosis* (the fix is always one command — the diagnosis is the job):

| Points | Criteria |
|---|---|
| 4 | Root cause identified < 10 min, no hints |
| 3 | Root cause identified < 20 min, no hints |
| 2 | Solved with hint level 1–2 |
| 1 | Needed hint level 3 or SOLUTIONS.md |
| 0 | Unsolved + debrief not written |

**16–20:** you triage clusters; trust yourself on call. **10–15:** solid — re-run the gauntlet in a week (the scripts are reusable; shuffle the order). **< 10:** no judgment, but re-read each SOLUTIONS diagnostic path and re-run the gauntlet before Phase 7 — this skill is the capstone's foundation and ~30% of the CKA.

### 4. Reflection — "what should have caught this?"

The difference between a firefighter and an engineer: the engineer leaves each fire with a smoke detector. For each scenario, write the one Prometheus alert that would have paged *before a human noticed*, in your Phase 5 stack's terms. Sketch (then compare with each SOLUTIONS entry):

| # | Signal worth alerting on |
|---|---|
| 01 | CoreDNS available replicas == 0 (or a blackbox DNS probe failing) |
| 02 | per-namespace network-policy drop rate (Cilium/Hubble) deviating from baseline |
| 03 | ArgoCD app not Synced/Healthy for > 10 min (`argocd_app_info`) |
| 04 | any pod `Pending` > 10 min (`kube_pod_status_phase`) |
| 05 | blackbox HTTP probe through the *ingress* failing while in-cluster scrapes stay green |

Stretch (recommended): actually add two of these as `PrometheusRule`s in your gitops repo next to Day 35's `PodlabHighErrorRate`, then re-run the matching break script and watch the alert fire before your timer starts.

### 5. Restore and verify

If anything is still half-broken (or you want a clean slate to re-run):

```sh
./restore-all.sh
```

It restores CoreDNS replicas, deletes the sneaky NetworkPolicy, re-applies the saved ClusterRoleBinding, untaints workers, rescales ingress-nginx, and finishes with a health sweep. Re-run your baseline block from step 1 — everything should match.

## Verify ✅

- [ ] All five health checks from the table in step 2 pass
- [ ] `kubectl get pods -A | grep -vE 'Running|Completed'` → nothing
- [ ] `kubectl get apps -n argocd` → all `Synced/Healthy`
- [ ] `kubectl describe nodes | grep Taints` → only the control-plane's built-in taint remains
- [ ] Your incident log has five entries, each with symptoms → layer → root cause → fix → alert
- [ ] You scored the rubric and wrote the number down somewhere you'll find it before the capstone

## CKA corner 🎓

Troubleshooting is the **largest CKA domain (~30%)**, and today's method maps onto it directly — the exam's failures are exactly these: nodes NotReady, control-plane components down, DNS broken, services unreachable. Exam realities: you get a terminal and `kubectl`/`ssh` only (no k9s), the docs are available but slow — muscle memory wins, and `kubectl describe` + events solve a scandalous share of tasks. Three timed drills on your kind cluster — where "ssh to the node" is `docker exec`:

**Drill A — dead kubelet (10 min).**
```sh
docker exec course-worker systemctl stop kubelet     # break it
```
Watch `kubectl get nodes -w` → `course-worker` goes `NotReady` in ~40s (the node-monitor grace period). Diagnose like the exam: `kubectl describe node course-worker` (Conditions: `NodeStatusUnknown`/kubelet stopped posting), then onto the "node": `docker exec course-worker systemctl status kubelet`, `docker exec course-worker journalctl -u kubelet --no-pager | tail -20`. Fix: `docker exec course-worker systemctl start kubelet` (exam: also `systemctl enable kubelet` — they love a kubelet that's stopped *and* disabled). Node returns to `Ready`.

**Drill B — broken static pod manifest (10 min).**
```sh
docker exec course-control-plane mv /etc/kubernetes/manifests/kube-scheduler.yaml /tmp/
```
The kubelet runs control-plane components from `/etc/kubernetes/manifests` (Day 16); remove the manifest and the scheduler pod vanishes — no controller recreates it, because no controller owns it. Symptom: `kubectl run drill-b --image=podlab:v1` → forever `Pending` with **no scheduling events at all** (nobody is alive to write them — subtly different from Break 04's "events full of taints"; learn to tell silence from refusal). Confirm: `kubectl get pods -n kube-system | grep scheduler` → gone. Fix: `docker exec course-control-plane mv /tmp/kube-scheduler.yaml /etc/kubernetes/manifests/` — kubelet re-creates it within seconds; the Pending pod schedules. `kubectl delete pod drill-b`.

**Drill C — NotReady triage table (memorize).**

| `describe node` says | Meaning | Fix |
|---|---|---|
| `NodeStatusUnknown` / last heartbeat old | kubelet not reporting | ssh in: `systemctl status/start kubelet`, read `journalctl -u kubelet` |
| kubelet logs: cert/connection errors to apiserver | kubelet can't reach/authenticate to control plane | check apiserver up, kubeconfig at `/etc/kubernetes/kubelet.conf`, certs |
| `NetworkUnavailable` / pods stuck `ContainerCreating` with CNI errors | CNI not installed/crashed | check CNI DaemonSet pods (here: Cilium) on that node |
| `MemoryPressure` / `DiskPressure` true | resource exhaustion, kubelet evicting | free disk (`crictl rmi --prune`), find the hog |
| `SchedulingDisabled` | someone cordoned it | `kubectl uncordon` — check it wasn't cordoned for a reason |

First moves, always: `kubectl get nodes` → `kubectl describe node <n>` → ssh → `systemctl status kubelet` → `journalctl -u kubelet`. That sequence is most of the node-troubleshooting points on the exam.

## Stretch goals

- Implement two alerts from the reflection table as `PrometheusRule`s in git, re-run the matching break script, watch them fire. (The single highest-value stretch in Phase 6.)
- Design `break-06.sh` for a study partner: it must be substrate-level, blind-runnable, restorable. Good candidates: wrong `ingressClassName` on one app, a `Forbidden`-causing change to Prometheus's ServiceAccount, kube-proxy scaled to 0.
- Re-run the full gauntlet **in k9s only** — no raw kubectl except `auth can-i`. Then once more, kubectl only, no k9s (CKA conditions).
- Combine breaks 01+04 and observe how failures *compound*: DNS down means you can't trust any name-based health check while also unable to schedule fresh debug pods.
- Read the kubeadm cluster-troubleshooting docs page once, now, so the exam version of Drill B (broken etcd manifest, wrong apiserver flag) isn't your first contact.

## Cleanup

```sh
./restore-all.sh                      # if you haven't already
rm -rf /tmp/day44                     # the scripts' state backups
kubectl delete pod drill-b --ignore-not-found
docker exec course-worker systemctl start kubelet 2>/dev/null    # belt and braces, if you did Drill A
```

**Everything stays.** This is the last Phase 6 day: the platform you'll take into Phase 7's CI/CD and capstone is now complete — Argo Rollouts, cert-manager, Kyverno, Velero+MinIO, the observability stack, and three hardened podlab environments. If your Mac is wheezing, the reversible savings are: Kyverno's cleanup/reports controllers to 0 (Day 42), `rollouts-lab` replicas to 1 (Day 39), and the Velero schedule deleted (Day 43). Don't remove any *component* — the capstone uses all of them.
