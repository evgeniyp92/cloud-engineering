# Day 44 — Solutions

Read a scenario's section only after you've fixed it (or burned the full 20 minutes). Each one: root cause → the diagnostic path that finds it fastest → fix → prevention → what monitoring should have caught it.

---

## Break 01 — CoreDNS scaled to zero

**Root cause.** `kubectl -n kube-system scale deploy coredns --replicas=0`. No DNS pods → every in-cluster name lookup fails. Service IPs still route (kube-proxy/Cilium don't depend on DNS), so anything holding an established connection or using raw IPs keeps working — which is why the failure is *partial* and confusing: guestbook's `/readyz` 503s (it resolves `postgres` by name on new connections), Prometheus scrape targets fall over one by one, anything restarting fails its way into CrashLoopBackOff.

**Diagnostic path.** Top-down: `kubectl get --raw /readyz` → ok, nodes Ready, so control plane fine. Step 2 of the method — *is it DNS?*: `kubectl run test --rm -it --image=busybox:1.36 --restart=Never -- nslookup kubernetes.default` → timeout. Name fails; direct pod IP works. DNS confirmed. Supplier check: `kubectl get pods -n kube-system -l k8s-app=kube-dns` → no pods → `kubectl get deploy coredns -n kube-system` → `0/0`. Three commands from symptom to cause once you ask the DNS question — which is why the method asks it second.

**Fix.**
```sh
kubectl -n kube-system scale deployment coredns --replicas=2
kubectl run test --rm -i --image=busybox:1.36 --restart=Never -- nslookup kubernetes.default   # verify
```

**Prevention.** Nobody should be hand-scaling kube-system. Real-world equivalents: an HPA misconfigured on CoreDNS, an overzealous "cost-saving" script, an eviction storm. Guard with a PodDisruptionBudget (`minAvailable: 1`) on CoreDNS and RBAC that keeps humans out of kube-system scale operations.

**What monitoring should have caught it.** kube-prometheus-stack ships `KubeDeploymentReplicasMismatch` / CoreDNS-absent style alerts; the sharpest signal is `kube_deployment_status_replicas_available{deployment="coredns"} == 0` or a blackbox probe doing an actual DNS lookup. Lesson: alert on the *dependency* (DNS up) not just per-app symptoms, or one root cause pages you fifteen times.

---

## Break 02 — silent default-deny egress in podlab-prod

**Root cause.** A NetworkPolicy `sneaky-default-deny` in `podlab-prod` with `podSelector: {}` and `policyTypes: [Egress]` but **no egress rules**: selects every pod, declares egress policy, allows nothing. All outbound from prod pods — including DNS to kube-system — is dropped. Ingress is untouched, so the pods still *serve* fine; they just can't *call* anything. NetworkPolicies are additive allow-lists: an empty list is total denial.

**Diagnostic path.** Localization first: dev/stage fine + prod broken + identical manifests ⇒ the diff lives in the namespace, not the app. In-pod test in prod (`nslookup` fails) vs same test in dev (works). Inventory the namespace for non-workload objects: `kubectl get netpol -n podlab-prod` → there's one more policy than Day 15 left there. `kubectl describe netpol sneaky-default-deny -n podlab-prod` → read `Allowing egress traffic: <none>`.

**Fix.**
```sh
kubectl delete networkpolicy sneaky-default-deny -n podlab-prod
kubectl run -n podlab-prod test --rm -i --image=busybox:1.36 --restart=Never -- nslookup kubernetes.default
```
(In real life: don't just delete — find who/what applied it. `kubectl get netpol sneaky-default-deny -o yaml | grep -A5 managedFields` shows the manager and timestamp.)

**Prevention.** Default-deny is *good* — when it arrives **with** its allow-rules (DNS egress to kube-dns, required service-to-service paths) in the same commit, via GitOps where review happens. Day 42's generate policy does exactly this for new namespaces; pair it with a standing `allow-dns` policy.

**What monitoring should have caught it.** Cilium drops are observable: Hubble flow metrics / `cilium_drop_count_total` spiking in one namespace is the direct signal. Indirectly: guestbook-style readiness failures and Prometheus target errors scoped to one namespace. An alert on "policy drop rate by namespace deviates from baseline" turns silent drops into a page with the namespace already named.

---

## Break 03 — ArgoCD application-controller's ClusterRoleBinding deleted

**Root cause.** The `argocd-application-controller` ClusterRoleBinding was deleted (and the controller pod bounced so cached watches dropped). The controller's ServiceAccount token still *authenticates* fine — but every authorization check now fails: the controller can't list or watch the resources it manages. Apps freeze in their last known state; syncs error out. AuthN ≠ AuthZ: the identity is valid, the permissions are gone.

**Diagnostic path.** Symptom: `kubectl get applications -n argocd` stuck/Unknown, no reaction to commits. The teaching point of this scenario: **read the controller's logs, not the UI** — UIs summarize, logs accuse: `kubectl logs statefulset/argocd-application-controller -n argocd --tail=50` → wall of `Forbidden`, `User "system:serviceaccount:argocd:argocd-application-controller" cannot list resource ...`. That message contains the *who* and the *what*. Confirm and locate the missing grant:
```sh
kubectl auth can-i list deployments --as=system:serviceaccount:argocd:argocd-application-controller   # no
kubectl get clusterrolebinding | grep argocd     # the application-controller binding is missing
```

**Fix.**
```sh
kubectl apply -f /tmp/day44/break-03-crb.yaml      # the break script's backup
# or, the durable route — re-assert the install manifest:
# kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl auth can-i list deployments --as=system:serviceaccount:argocd:argocd-application-controller   # yes
```
Then watch apps resync (`kubectl get apps -n argocd -w`).

**Prevention.** This is the bootstrap paradox: ArgoCD can self-heal everything it manages *except its own ability to manage*. Keep ArgoCD's install manifests in git and applied by something other than ArgoCD alone (Day 25's bootstrap script), and restrict `delete` on ClusterRoleBindings to break-glass identities.

**What monitoring should have caught it.** ArgoCD exports `argocd_app_info{sync_status,health_status}` — alert on apps not Synced/Healthy for >N minutes, and on controller error-log rate. The genuinely sharp alert is "time since last successful reconciliation" — a freshness check, because a *frozen* GitOps controller looks healthy on every liveness probe it has.

---

## Break 04 — workers tainted NoSchedule

**Root cause.** Both workers tainted `maintenance=true:NoSchedule`. Taints only gate **scheduling** — running pods are untouched (that would need `NoExecute`). So the cluster looks perfectly healthy until anything needs placement: the break script's rollout-restart of podlab-dev created new pods that join the Pending queue forever — with the control-plane node also tainted (since Day 1), zero schedulable nodes exist.

**Diagnostic path.** "Existing fine, new stuck" → that's the scheduler's domain. Interrogate a Pending pod — `kubectl describe pod <pending> -n podlab-dev`, Events:
`0/3 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }, 2 node(s) had untolerated taint {maintenance: true}.`
The event *is* the diagnosis — Kubernetes' single most informative error message, and the habit of reading it is what this scenario drills. Confirm: `kubectl describe node course-worker | grep -A3 Taints`.

**Fix.**
```sh
kubectl taint nodes course-worker course-worker2 maintenance:NoSchedule-    # trailing '-' removes
kubectl get pods -n podlab-dev -w    # Pending pods schedule within seconds
```

**Prevention.** Taints are how real maintenance works (`kubectl cordon`/`drain` taint with `unschedulable`); the failure is *forgetting to untaint*. Maintenance procedures should be checklists with a re-enable step, and ideally driven by tooling with timeouts rather than ad-hoc taint commands.

**What monitoring should have caught it.** `kube_pod_status_phase{phase="Pending"} > 0 for 10m` is the universal catch-all. More targeted: `kube_node_spec_taint` changing, or scheduler metrics (`scheduler_pending_pods`). Pending-pods-age is one of the highest-value, lowest-noise alerts a cluster can have — it catches taints, quota exhaustion, resource starvation, and PVC binding failures with one rule.

---

## Break 05 — ingress controller at 0 + TLS secret deleted

**Root cause.** Two simultaneous cuts: `ingress-nginx-controller` scaled to 0 (kills *every* `*.localhost` URL — the single shared front door behind kind's port mapping), and the `podlab-tls` secret deleted in `rollouts-lab`. The twist: the second cut **self-heals** — cert-manager's Certificate `podlab-tls` still exists, its controller notices the missing secret and re-issues within seconds. One incident, two faults, only one needs a human: triage must *distinguish L7-path-down from app-down*, and you must verify the supposed second fault instead of assuming it.

**Diagnostic path.** Breadth of blast radius first: grafana, argocd, canary — different namespaces, all dead from the Mac, with connection-refused/reset rather than app errors. Shared-fate ⇒ shared layer. Confirm apps are alive underneath: `kubectl get pods -n monitoring` fine; `kubectl port-forward svc/...` works → the path is dead, not the apps. Walk the path: Mac:8080 → kind port-map → ingress-nginx: `kubectl get pods -n ingress-nginx` → none; `kubectl get deploy -n ingress-nginx` → `0/0`. Scale up, retest HTTP. Then the TLS check: `curl --cacert course-ca.crt https://canary.localhost:8443/` — works; `kubectl get secret podlab-tls -n rollouts-lab` shows a *young* secret, and `kubectl describe certificate podlab-tls -n rollouts-lab` shows a recent `Issuing`→`Ready` event pair. cert-manager beat you to it.

**Fix.**
```sh
kubectl -n ingress-nginx scale deployment ingress-nginx-controller --replicas=1
curl -s -o /dev/null -w '%{http_code}\n' http://grafana.localhost:8080      # 200/302
curl --cacert course-ca.crt https://canary.localhost:8443/ -s -o /dev/null -w '%{http_code}\n'
```

**Prevention.** The ingress controller is a tier-0 dependency: ≥2 replicas + a PodDisruptionBudget in production; RBAC-restrict scaling it. For TLS: this is *why* certificates are declarative — the Secret is a cache, the Certificate is the truth; nothing to prevent because the design already absorbed it.

**What monitoring should have caught it.** Blackbox-exporter probes through the front door (`probe_success{instance="http://grafana.localhost"} == 0`) catch the user-visible path no internal metric represents — internal scrapes can all be green while the front door is off its hinges. Plus `kube_deployment_status_replicas_available{deployment="ingress-nginx-controller"} == 0` and cert-manager's `certmanager_certificate_ready_status` for the TLS half.

---

## The meta-lesson

Five scenarios, one method. Each fix was one command; each *diagnosis* was the work. Score yourself honestly against the rubric in the README, then do the reflection step: for every scenario, write the one alert you'd add. You already run the stack that can host all five.
