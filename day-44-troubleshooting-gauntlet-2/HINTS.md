# Day 44 — Hints

Use these only when stuck. Take them one level at a time: read a hint, go back and try for a few minutes before reading the next. Each level read costs you rubric points (see README) — that's the deal.

---

## Break 01

**Level 1.** Pick any two symptoms and write them down precisely. "Guestbook returns 503" and "ArgoCD shows errors" — what could possibly be *shared* between two unrelated apps? Run the top-down method from the README, step by step, no skipping.

**Level 2.** Get inside the cluster's perspective: `kubectl run test --rm -it --image=busybox:1.36 --restart=Never -- sh`, then `nslookup kubernetes.default`. Compare with `wget -qO- <a-pod-IP>:8080/healthz` (get a pod IP from `kubectl get pods -o wide -n podlab-dev`). One works, one doesn't. What's the difference between reaching a name and reaching an IP?

**Level 3.** It's always DNS. Who *serves* DNS inside the cluster, in which namespace, and how many replicas of it are currently running?

---

## Break 02

**Level 1.** The blast radius is your localization tool: prod broken, dev and stage fine — yet they run *identical* manifests from the same kustomize base. So the difference is not the app. List everything that can differ between two namespaces running the same workload.

**Level 2.** Run your in-pod test (`kubectl run -n podlab-prod test --rm -it --image=busybox:1.36 --restart=Never -- sh`, try `nslookup`) — then run the *same* pod in `podlab-dev`. Confirms namespace-scoped. Now inventory the namespace: `kubectl get all,netpol,quota,limitrange -n podlab-prod` and diff against dev. Anything in prod that dev doesn't have?

**Level 3.** `kubectl get networkpolicy -n podlab-prod` — read each one and ask "would Day 15 me have written this?" Check `policyTypes`. An empty `podSelector` with `Egress` listed and no egress rules means: every pod, allowed egress to nowhere. Including DNS.

---

## Break 03

**Level 1.** Define "stopped working" precisely: `kubectl get applications -n argocd` — what's the sync status and how stale? Push a trivial commit to k8s-gitops; does anything react? Now: which *component* of ArgoCD is responsible for reconciling? Find its pod.

**Level 2.** Read the controller's logs, not the UI: `kubectl logs statefulset/argocd-application-controller -n argocd --tail=50`. There's a word that appears over and over. That word is an HTTP 403 wearing its Kubernetes name. What identity is being refused, and refused *what*?

**Level 3.** The controller's ServiceAccount needs cluster-wide rights, granted by a ClusterRoleBinding. List them: `kubectl get clusterrolebinding | grep argocd`. Something that should be there isn't. Verify your theory with `kubectl auth can-i list deployments --as=system:serviceaccount:argocd:argocd-application-controller`. Rebuild the binding (your break script saved a copy under /tmp/day44/, or re-apply the ArgoCD install manifest).

---

## Break 04

**Level 1.** "Existing pods fine, new pods stuck" is a fingerprint, not a mystery — which single control-plane component only ever touches *new* pods? Find a Pending pod and ask it directly: `kubectl describe pod <pending-pod> -n podlab-dev` — the Events section at the bottom is the answer in prose.

**Level 2.** The event says something like `0/3 nodes are available`, with reasons per node group. One reason mentions the control-plane (expected — that taint has been there since Day 1). What's the reason for the *two workers*?

**Level 3.** `kubectl describe node course-worker | grep -A3 Taints`. A taint nobody remembers adding (`maintenance=true:NoSchedule`) — this is what a forgotten maintenance window looks like. Remove it from both workers: `kubectl taint nodes course-worker course-worker2 maintenance:NoSchedule-` (note the trailing minus).

---

## Break 05

**Level 1.** "ALL *.localhost URLs dead" — apps in different namespaces (grafana, argocd, podlab) don't share fate unless something they all sit *behind* died. What's the one shared layer? First, check whether the apps themselves are even unhealthy: `kubectl get pods -n monitoring -n podlab-dev` — pick one and port-forward to it directly. App-up + URL-down = the path, not the app.

**Level 2.** Trace the path from your Mac: port 8080 → kind extraPortMapping → ingress controller pods → Service → app. `kubectl get pods -n ingress-nginx`. How many pods does a deployment scaled to zero have?

**Level 3.** Scale `ingress-nginx-controller` back up. Then re-check the *second* casualty: `https://canary.localhost:8443` with your `--cacert` from Day 40 — and look at `kubectl get secret,certificate -n rollouts-lab`. If the TLS secret is already back, ask yourself who recreated it (you installed that controller on Day 40; check the secret's age and the Certificate's events). Two things were broken; only one needed you.
