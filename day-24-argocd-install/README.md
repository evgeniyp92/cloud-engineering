# Day 24 — GitOps & ArgoCD: Install and First Application

> **Time:** ~3.5 h · **Builds on:** Days 20, 22, 23

## Objectives

- Explain GitOps and argue *why pull-based deployment beats push-based CI/CD* — with the three concrete wins (credentials, drift, audit).
- Install ArgoCD on the `course` cluster, reach its UI through your existing ingress, and log in with the CLI.
- Publish `~/Code/k8s-gitops` to GitHub and deploy your podlab Helm chart from it via a **declarative `Application` manifest** — no UI clicking.
- Read an Application's two independent statuses — **sync** and **health** — and walk a change through git → OutOfSync → diff → sync.

## Concepts

### Git as the single source of truth

For 23 days you've been the deployment mechanism: `kubectl apply`, `helm upgrade`, `kustomize build | kubectl apply`. That works for one person and one cluster. It does not survive a team: who applied what, when, from which working copy, with which uncommitted edits? The cluster's actual state lives nowhere except the cluster.

GitOps inverts this. The desired state of the cluster is **fully described in a git repo**, and an in-cluster agent continuously makes reality match the repo. Three consequences:

1. **Deploys are merges.** Code review, approvals, CI checks — your existing git workflow *is* your deployment workflow.
2. **Audit log = `git log`.** Every change has an author, a timestamp, a diff, and a revert button (`git revert`).
3. **The cluster is reproducible.** Day 48 will prove it: empty cluster + one `kubectl apply` = your entire platform. Clusters become cattle.

### Pull vs push

| | Push (CI pipeline runs `kubectl apply`) | Pull (agent in cluster watches git) |
|---|---|---|
| Cluster credentials | live in CI (GitHub Actions secrets…) — a CI breach is a cluster breach | **never leave the cluster**; the agent only needs read access to git |
| Drift (someone `kubectl edit`s prod) | invisible until the next pipeline run overwrites — or doesn't | detected continuously, surfaced as **OutOfSync**, optionally auto-reverted (Day 25) |
| Multi-cluster | N pipelines × N kubeconfigs | each cluster pulls; git doesn't even know the clusters exist |
| "What's running?" | check the last pipeline run and *hope* | ask the agent: it diffs live state vs git on a loop |

The pull model wins on security and observability. ArgoCD and Flux are the two big pull-based agents; this course uses ArgoCD because its UI makes the reconciliation loop *visible*, which is worth a lot while learning (and in interviews).

### The reconciliation loop

ArgoCD does, forever, for each Application:

```
   ┌────────────────────────────────────────────────────┐
   │  1. fetch git repo @ targetRevision                │
   │  2. RENDER  (helm template / kustomize build / raw)│
   │  3. DIFF    rendered manifests  vs  live cluster   │
   │  4. status: Synced  or  OutOfSync                  │
   │  5. if allowed (Day 25): APPLY the diff            │
   └──────────────────────△─────────────────────────────┘
                  every ~3 min + on webhook
```

Note step 2: ArgoCD doesn't *run helm install*. It runs `helm template` and applies the result itself — there is no Helm release object, no `helm list` entry. Same for Kustomize. ArgoCD is the only thing applying.

### ArgoCD architecture

The install drops several deployments into the `argocd` namespace:

| Component | Job |
|---|---|
| **repo-server** | clones repos, runs `helm template` / `kustomize build` — the *render* step |
| **application-controller** | the reconciler: diffs rendered vs live, syncs, computes health — the *diff/apply* step |
| **api-server** (`argocd-server`) | gRPC/REST API consumed by the UI and the `argocd` CLI; enforces RBAC |
| **dex** | OIDC broker for SSO (idle in this course — we use the local `admin` user) |
| applicationset-controller | renders ApplicationSets (Day 27) |
| redis | cache between the above |

### Sync status vs health status — two different questions

These are independent axes, and confusing them is the #1 ArgoCD beginner mistake:

- **Sync status** (`Synced` / `OutOfSync`): *does live state match git?* A pure diff. Says nothing about whether anything works.
- **Health status** (`Healthy` / `Progressing` / `Degraded` / `Missing`): *is the workload actually OK?* Computed per resource kind — a Deployment is Healthy when its rollout finished and replicas are ready, regardless of git.

You can be `Synced + Degraded` (git says image `podlab:v999`, cluster faithfully runs the ImagePullBackOff). You can be `OutOfSync + Healthy` (new commit pushed, old version still running fine). Both statuses appear on every app card; read them as two answers to two questions.

## Lab

### 1. Push k8s-gitops to GitHub

ArgoCD needs to pull your repo from somewhere reachable. GitHub, public, free — and a public repo means no credentials to configure today (Day 28 makes "secrets in a public repo" safe; until then nothing sensitive goes in).

On [github.com/new](https://github.com/new): create repo `k8s-gitops`, **public**, no README/license (you have content already). Then:

```sh
cd ~/Code/k8s-gitops
git remote add origin https://github.com/<you>/k8s-gitops.git
git push -u origin main
```

If your default branch is `master`, either rename (`git branch -m main`) or use `master` as `targetRevision` everywhere below — just be consistent for the rest of the course. If you won't use GitHub, see the Gitea stretch goal, but every lesson from here on assumes GitHub.

### 2. Install ArgoCD

```sh
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl get pods -n argocd --watch     # Ctrl-C when all 7 are Running (~1-2 min)
```

Skim what arrived: `kubectl get deploy,sts,svc,cm -n argocd`. Match the deployments to the architecture table above. Note `applications.argoproj.io` and friends in `kubectl get crd | grep argoproj` — ArgoCD is configured *with Kubernetes resources*, which is what lets Day 26 manage ArgoCD with ArgoCD.

### 3. Get the initial admin password

The install generates a one-time admin password into a Secret:

```sh
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

Save it somewhere for the day (or `argocd account update-password` later; then you may delete the secret).

### 4. Reach the UI — port-forward now, ingress forever

Quick check that it's alive:

```sh
kubectl port-forward svc/argocd-server -n argocd 8081:443
```

Open https://localhost:8081 (accept the self-signed cert), log in as `admin`. Works, but you don't want a port-forward running for the next 26 days. Ctrl-C it; let's give ArgoCD a permanent address.

**The tradeoff:** `argocd-server` terminates TLS itself and speaks gRPC, which clashes with ingress-nginx terminating TLS in front of it. Two clean fixes: (a) TLS passthrough — nginx forwards raw TLS to argocd-server; requires the controller flag `--enable-ssl-passthrough`, which our kind install doesn't set; or (b) **`server.insecure` mode** — argocd-server serves plain HTTP and the ingress fronts it like any app. For a production internet-facing install you'd do passthrough or TLS at the ingress with a real cert; for a local lab, insecure mode behind `localhost` is the right call.

Flip the flag via ArgoCD's parameter ConfigMap and restart the server:

```sh
kubectl patch configmap argocd-cmd-params-cm -n argocd \
  --type merge -p '{"data":{"server.insecure":"true"}}'
kubectl rollout restart deploy argocd-server -n argocd
```

Now write `ingress.yaml` in this day's folder: an Ingress in ns `argocd`, `ingressClassName: nginx`, host `argocd.localhost`, path `/` (Prefix) → service `argocd-server` port `80`. You've written five of these since Day 5 — no solution block for boilerplate:

```sh
kubectl apply -f ingress.yaml
open http://argocd.localhost:8080      # log in: admin / <password from step 3>
```

### 5. Install and log in with the CLI

```sh
brew install argocd
argocd login argocd.localhost:8080 --username admin \
  --password '<password>' --plaintext --grpc-web
argocd version          # client AND server versions ⇒ login worked
```

`--plaintext` because the server is in insecure mode; `--grpc-web` tunnels gRPC over HTTP/1.1 so it survives nginx. (Fallback if this fights you: port-forward and `argocd login localhost:8081 --insecure`.)

### 6. The first Application — as YAML

You could click "New App" in the UI. Don't. The `Application` CRD *is* the lesson: it's the unit ArgoCD reconciles, and on Day 26 these manifests themselves go into git. Every Application answers three questions — **what** (source: repo + revision + path, rendered how), **where** (destination: cluster + namespace), **how** (syncPolicy; absent = manual).

Create `argocd/apps/podlab.yaml` **in your k8s-gitops repo** (the directory becomes important on Day 26). Requirements:

- `apiVersion: argoproj.io/v1alpha1`, `kind: Application`, name `podlab-helm`, **namespace `argocd`** (Applications live where ArgoCD watches)
- `spec.project: default`
- source: your GitHub repo URL, `targetRevision: main`, `path: charts/podlab`, and a `helm.valuesObject` block that sets ~2 replicas and enables your chart's ingress at host `podlab-helm.localhost` (use *your* Day 20 value names — the keys below are illustrative)
- destination: `server: https://kubernetes.default.svc` (the magic in-cluster URL), `namespace: argocd-podlab`
- **no `syncPolicy`** — today every sync is a deliberate act

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: podlab-helm
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/<you>/k8s-gitops
    targetRevision: main
    path: charts/podlab
    helm:
      valuesObject:          # inline values; valueFiles: [values.yaml] also works
        replicaCount: 2
        ingress:
          enabled: true
          host: podlab-helm.localhost
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd-podlab
```

</details>

Commit it, push, and apply (the apply is by hand today; Day 26 removes even that):

```sh
cd ~/Code/k8s-gitops
git add argocd/apps/podlab.yaml && git commit -m "podlab-helm Application" && git push
kubectl create namespace argocd-podlab
kubectl apply -f argocd/apps/podlab.yaml
argocd app list
```

### 7. Sync it and read the tree

The app shows `OutOfSync` + `Missing` — git describes resources that don't exist yet. Sync:

```sh
argocd app sync podlab-helm
argocd app get podlab-helm
```

`app get` is your new `helm status`: sync status, health, and every managed resource with per-resource status. Now open the app in the UI and spend 10 minutes in the **resource tree**: Deployment → ReplicaSet → Pods, the Service with its Endpoints, the ConfigMap your chart renders, the Ingress. Click a pod — logs and events without leaving the page. Confirm the app serves:

```sh
curl -s http://podlab-helm.localhost:8080/ | python3 -m json.tool
```

### 8. A change, the GitOps way

Edit something observable in the chart — e.g. in `charts/podlab/values.yaml`, change `color` and add/modify a key in the config dict that renders into `/etc/podlab`:

```sh
cd ~/Code/k8s-gitops
# edit charts/podlab/values.yaml: color: "purple", config.motd: "deployed by argocd"
git add -A && git commit -m "podlab: purple + motd" && git push
```

ArgoCD polls every ~3 min. Force the check instead of waiting, then inspect the diff *before* syncing:

```sh
argocd app get podlab-helm --refresh     # → OutOfSync
argocd app diff podlab-helm              # the exact server-side diff
```

In the UI the app is yellow; **App Diff** shows the same thing rendered. This gap is the GitOps review moment: git already says purple, the cluster still says the old color, and ArgoCD is *telling* you instead of silently acting (that changes tomorrow). Sync and prove it landed:

```sh
argocd app sync podlab-helm
curl -s http://podlab-helm.localhost:8080/config | python3 -m json.tool | grep -iA2 motd
```

Finally, close the loop on auditability — the deployed revision is a git SHA:

```sh
argocd app get podlab-helm -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["sync"]["revision"])'
git -C ~/Code/k8s-gitops log -1 --format=%H     # identical
```

## Verify ✅

- [ ] `git -C ~/Code/k8s-gitops remote -v` → shows your GitHub `origin`, and the repo is visible on github.com
- [ ] `kubectl get pods -n argocd` → all pods `Running`, including `argocd-server`, `argocd-repo-server`, `argocd-application-controller-0`
- [ ] `curl -s http://argocd.localhost:8080/healthz` → `ok` (UI reachable without a port-forward)
- [ ] `argocd app get podlab-helm | grep -E 'Sync Status|Health Status'` → `Synced` and `Healthy`
- [ ] `curl -s http://podlab-helm.localhost:8080/config` → shows the `motd` value you pushed in step 8
- [ ] The revision SHA from `argocd app get` equals `git log -1 --format=%H` in k8s-gitops
- [ ] `helm list -n argocd-podlab` → **empty** — and you can explain why ArgoCD-deployed charts aren't Helm releases

## Interview corner 💬

**"Sell me GitOps — why not just have CI run kubectl apply?"** Push-based CI needs cluster-admin credentials sitting in the CI system, so your CI is now in your cluster's threat model; pull-based agents keep credentials in-cluster and only read git. Push deploys are point-in-time — nothing notices drift between runs; a GitOps agent reconciles continuously and *reports* divergence. And the audit story collapses to `git log`: every change reviewed, attributable, revertable.

**"ArgoCD says Synced but the app is down. What does that tell you?"** Sync and health are independent. Synced+Degraded means the cluster faithfully matches git — and git describes something broken (bad image tag, failing probe config). The fix is a git revert or fix-forward commit, not kubectl surgery; ArgoCD already did its job by separating "matches desired state" from "desired state is good".

## Stretch goals

- Make the repo **private** and teach ArgoCD to authenticate: GitHub fine-grained PAT (read-only, this repo), then `argocd repo add https://github.com/<you>/k8s-gitops --username <you> --password <token>`, or declaratively as a Secret labeled `argocd.argoproj.io/secret-type: repository`. Flip back to public after (or keep — everything still works).
- No-GitHub path: run [Gitea](https://gitea.com) in-cluster via its Helm chart, push k8s-gitops to it, point the Application at the in-cluster URL.
- Add a GitHub **webhook** (repo → Settings → Webhooks → `http://argocd.localhost:8080/api/webhook`) — it won't reach your laptop from github.com, which is exactly the point: reason about why, and look at `smee.io`-style relays.
- `kubectl get application podlab-helm -n argocd -o yaml` and read `status:` — everything the CLI/UI showed you is just CRD status fields.

## Cleanup

Nothing. **ArgoCD, the `argocd-podlab` namespace, the `podlab-helm` Application, and the ingress stay for the rest of the course** — Day 25 builds directly on this exact app.
