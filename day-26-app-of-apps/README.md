# Day 26 — App-of-Apps: Bootstrap a Stack from One Manifest

> **Time:** ~3 h · **Builds on:** Days 24, 25 (and the Day 21 guestbook chart)

## Objectives

- Build the **app-of-apps** pattern: a root Application whose source directory contains more Application manifests — and explain why every serious ArgoCD shop runs one.
- Order a multi-app sync with **sync waves** (infra before apps) and watch the ordering happen in the UI.
- Adopt an **existing, manually-installed** component (metrics-server) into ArgoCD management without an outage — the real-world migration problem.
- Use **resource hooks** and explain how your Helm hooks map onto them; demo the Application finalizer cascade safely.

## Concepts

### One Application doesn't scale

You have one Application, hand-applied with kubectl. A real platform has dozens: ingress, cert-manager, monitoring, sealed-secrets, ten product apps. If each needs a manual `kubectl apply -f app.yaml`, you've rebuilt the problem GitOps solves — un-audited, un-reproducible kubectl from laptops, just one level up.

The fix is delightfully recursive: **Application manifests are Kubernetes resources, so an Application can deploy them.** Point a "root" Application at a directory of Application YAMLs:

```
k8s-gitops/
└── argocd/
    ├── root.yaml              ← apply THIS once, by hand, ever
    └── apps/                  ← root's source path
        ├── metrics-server.yaml   (wave 0 — infra)
        ├── podlab.yaml           (wave 1 — already there since Day 24)
        └── guestbook.yaml        (wave 1)
```

`kubectl apply -f root.yaml` is now the only imperative command in your platform's life. Root syncs → child Applications appear in `argocd` → each child syncs its own workload. Adding a platform component = adding a file to `argocd/apps/` in a PR. Deleting the file (root has `prune: true`) removes the component. This exact `root.yaml` is what **Day 48's capstone** applies to an *empty* cluster to resurrect everything — treat today's structure as load-bearing.

It also gives you separation of duties: the platform team owns `argocd/apps/`, each product team owns its chart/overlay directory, and ArgoCD Projects (which you've left at `default`) can enforce that split with RBAC.

### Sync waves: ordering inside a sync

Within one Application, ArgoCD sorts resources sensibly (namespaces before deployments, CRDs first). For ordering *you* care about, annotate resources:

```yaml
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "0"    # default is 0; lower syncs first
```

ArgoCD syncs wave by wave, **waiting for every resource in a wave to be healthy** before starting the next. Here's the trick that makes app-of-apps powerful: the root app's "resources" are Applications, and an Application's health rolls up from its children. So wave 0 on `metrics-server.yaml` and wave 1 on the app manifests means: *infrastructure must be Synced+Healthy before product apps even get created*. That's "install the monitoring stack before the apps that need ServiceMonitors" expressed in one annotation. (Wave gating needs the children to sync on their own — keep them `automated`.)

### Resource hooks

Some workloads aren't "apply and reconcile" — DB migrations, smoke tests, cache warms. ArgoCD hooks run Jobs (or any resource) at sync phases:

| Annotation | Runs |
|---|---|
| `argocd.argoproj.io/hook: PreSync` | before the sync applies anything |
| `hook: Sync` | with the sync, ordered by its wave |
| `hook: PostSync` | after everything is Synced + Healthy |
| `hook: SyncFail` | only if the sync fails |

plus `hook-delete-policy` (e.g. `BeforeHookCreation` — delete the previous Job before re-running). You already wrote hooks on Day 21 — in Helm. ArgoCD doesn't execute Helm hooks (remember: it runs `helm template`, not `helm install`) but it **translates** them: `pre-install`/`pre-upgrade` → `PreSync`, `post-install`/`post-upgrade` → `PostSync`. So your guestbook chart's pre-install Job runs as a PreSync hook with zero changes. (Helm `test` hooks are skipped; `pre-delete`/`post-delete` have no equivalent — know this for migration conversations.)

### The adoption problem

Day 5 installed ingress-nginx by hand; Day 8 installed metrics-server with Helm. They're running, they're load-bearing, and they're *not in git*. Every real team migrating to GitOps faces this: you can't delete-and-recreate the ingress controller that's serving production, and naively pointing ArgoCD at a chart with different values will happily "correct" your live config into an outage.

The safe adoption recipe:

1. **Match reality first**: extract the live install's exact values (`helm get values`), pin the same chart version, render and diff *before* letting ArgoCD touch anything.
2. Let ArgoCD apply: since live ≈ desired, the first sync is a near-no-op patch — same names and namespace means kubectl-style apply just takes ownership, no recreation, no downtime.
3. **Decommission the old installer's bookkeeping** — and here's the trap: `helm uninstall` *deletes the live resources*. You remove Helm's release Secrets instead, leaving the resources untouched.

We'll adopt metrics-server today (small, low-blast-radius, perfect practice). Adopting ingress-nginx — the component your every `curl` depends on — is the stretch goal, deliberately. And monitoring lands directly under ArgoCD on Day 30, no adoption needed (you'll add a `kube-prometheus-stack.yaml` to this same directory).

## Lab

### 1. Annotate the existing child: podlab → wave 1

In `~/Code/k8s-gitops/argocd/apps/podlab.yaml` add:

```yaml
metadata:
  name: podlab-helm
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"
  finalizers:
    - resources-finalizer.argocd.argoproj.io
```

The finalizer means: if this file is ever pruned from git, the workload goes with it (full cascade). Right choice for product apps.

### 2. metrics-server.yaml — the adoption (wave 0)

First, capture reality:

```sh
helm list -A | grep metrics-server          # release name, namespace, chart version
helm get values metrics-server -n kube-system   # the values you installed with on Day 8
```

Write `argocd/apps/metrics-server.yaml`. Requirements:

- Application `metrics-server` in ns `argocd`, sync-wave `"0"`, **no finalizer** (infra: if the file is mis-pruned, you want the live component to survive the mistake)
- source: `repoURL: https://kubernetes-sigs.github.io/metrics-server/`, `chart: metrics-server`, `targetRevision:` **pinned to the chart version `helm list` showed** — a Helm-repo source uses `chart:` + a version instead of `path:` + a branch
- `helm.valuesObject` reproducing your live values exactly (on kind that's at least `args: ["--kubelet-insecure-tls"]`)
- destination ns `kube-system`; syncPolicy automated + selfHeal (hold off on prune until adoption is verified), syncOption `ServerSideApply=true` (long CRD-ish manifests + multiple past field owners — SSA handles ownership cleanly)

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: metrics-server
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://kubernetes-sigs.github.io/metrics-server/
    chart: metrics-server
    targetRevision: 3.12.2        # ← your version from `helm list -A`
    helm:
      valuesObject:
        args:
          - --kubelet-insecure-tls
  destination:
    server: https://kubernetes.default.svc
    namespace: kube-system
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
      - ServerSideApply=true
```

</details>

### 3. guestbook.yaml — chart with a dependency (wave 1)

Your Day 21 guestbook chart pulls `postgresql` as a dependency; ArgoCD's repo-server runs `helm dependency build` for you (make sure `Chart.lock` is committed). Write `argocd/apps/guestbook.yaml`: Application `guestbook-helm`, wave `"1"`, finalizer on, source path `charts/guestbook` with the values you used on Day 21, destination namespace **`guestbook-helm`** — *not* `guestbook`. The `guestbook` namespace still runs your hand-built Day 11 StatefulSet + NetworkPolicies; adopting that into a chart-shaped app would be a name-mismatched mess (the adoption recipe only works when shapes match). Side-by-side is the honest move. Add `CreateNamespace=true`, automated + selfHeal + prune.

### 4. root.yaml

Write `argocd/root.yaml`. Requirements: Application `root` in ns `argocd`, **with** the finalizer; source = your repo, path `argocd/apps`, `directory.recurse: false`; destination ns `argocd` (the children are Applications and must land there); automated + selfHeal + prune.

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: root
  namespace: argocd
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: https://github.com/<you>/k8s-gitops
    targetRevision: main
    path: argocd/apps
    directory:
      recurse: false
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      selfHeal: true
      prune: true
```

</details>

### 5. The one command

```sh
cd ~/Code/k8s-gitops
git add argocd/ && git commit -m "app-of-apps: root + metrics-server + guestbook" && git push
kubectl apply -f argocd/root.yaml
argocd app list --watch    # or watch the UI grid
```

Watch the order: `root` syncs → `metrics-server` (wave 0) appears and syncs *first* → only when it's Healthy do `podlab-helm` and `guestbook-helm` (wave 1) follow. In the UI, open `root`: its resource tree is *Applications* — click through to grandchildren. Notes on what you're seeing:

- `podlab-helm` already existed from Day 24 — root **adopted** it (same name/namespace, manifest matches the file you've been applying), no churn. Mini-preview of step 6's adoption, the easy case.
- `guestbook-helm` takes the longest: postgres has to start. Check `curl -s http://<your-guestbook-host>.localhost:8080/entries` once green (host = whatever your chart's ingress sets; port-forward if you didn't enable it).

### 6. Finish the metrics-server adoption

`argocd app get metrics-server` — Synced and Healthy, and `kubectl top nodes` never stopped working: the first sync was a patch onto identical live resources, not a recreation (check pod age: `kubectl get pods -n kube-system -l app.kubernetes.io/name=metrics-server` — old pods, never restarted, *that's* a clean adoption). Now retire Helm's bookkeeping so nobody ever `helm upgrade`s it behind ArgoCD's back. **Not** `helm uninstall` — that deletes the deployment. Helm's state is just Secrets:

```sh
kubectl get secrets -n kube-system -l name=metrics-server,owner=helm   # sh.helm.release.v1.metrics-server.v1 ...
kubectl delete secrets -n kube-system -l name=metrics-server,owner=helm
helm list -A | grep metrics-server || echo "helm no longer knows it — ArgoCD owns it"
```

Once you've watched it stay Healthy, add `prune: true` to its syncPolicy (commit, push — root delivers the change; notice you didn't `kubectl apply` the app file, and never will again).

### 7. See your Helm hook run as a PreSync hook

Find your Day 21 hook Job in `charts/guestbook/templates/` (the one annotated `helm.sh/hook: pre-install,pre-upgrade`). Trigger a sync that exercises it — bump anything in the chart, push, then watch the guestbook app in the UI during the sync: the hook Job appears *first*, tagged **PreSync**, the rolling update waits for it. `kubectl get jobs -n guestbook-helm` shows the run. If your hook lacks `"helm.sh/hook-delete-policy": before-hook-creation`, add it — otherwise the second sync fails on "job already exists" (Jobs are immutable), a classic.

### 8. Cascade demo — on a copy, never on root

What does deleting an Application with the finalizer actually do? Find out on a throwaway, applied by hand (deliberately *not* committed — it's an experiment, not desired state):

```sh
argocd app create doomed \
  --repo https://github.com/<you>/k8s-gitops --path charts/podlab --revision main \
  --dest-server https://kubernetes.default.svc --dest-namespace doomed \
  --sync-option CreateNamespace=true --sync-policy automated
kubectl patch application doomed -n argocd --type merge \
  -p '{"metadata":{"finalizers":["resources-finalizer.argocd.argoproj.io"]}}'
kubectl get all -n doomed                  # workload is up
kubectl delete application doomed -n argocd
kubectl get all -n doomed                  # …and gone (deletion blocked until cascade finished)
kubectl delete ns doomed
```

Now reason it through for `root`: root has the finalizer, so `kubectl delete application root` would cascade to the child Applications, and the children with finalizers would cascade to their workloads — your entire platform, one command. That's exactly the property Day 48 exploits in reverse (one apply = everything), and exactly why infra apps like metrics-server *don't* carry the finalizer. Don't test it on root. Knowing you could is the point.

## Verify ✅

- [ ] `argocd app list` → `root`, `metrics-server`, `podlab-helm`, `guestbook-helm` — all `Synced` / `Healthy`
- [ ] `kubectl get applications -n argocd root -o jsonpath='{.metadata.finalizers}'` → contains `resources-finalizer.argocd.argoproj.io`
- [ ] In the UI, `root`'s tree shows three child Applications; the sync history shows metrics-server reaching Healthy before wave-1 apps started
- [ ] `kubectl top nodes` works, pods in `kubectl get pods -n kube-system -l app.kubernetes.io/name=metrics-server` predate the adoption, and `helm list -A | grep metrics-server` → empty
- [ ] `kubectl get jobs -n guestbook-helm` shows your hook Job, and the UI marked it `PreSync` during the last sync
- [ ] Namespace `doomed` no longer exists, and `guestbook` (Day 11, hand-built) is untouched: `kubectl get sts -n guestbook`
- [ ] `kubectl apply -f argocd/root.yaml` on a fresh shell is the *only* kubectl-apply your platform now needs — everything else flows from git

## Interview corner 💬

**"How would you bootstrap a brand-new cluster from nothing?"** Strong answer in three beats: (1) cluster comes from IaC (Terraform/CAPI/kind config — also in git); (2) install ArgoCD with its manifest and `kubectl apply` *one* root app-of-apps Application; (3) root fans out — sync-wave 0 installs platform prerequisites (secrets controller, ingress, monitoring), later waves bring product apps, each child syncing automated. Total imperative surface: two kubectl commands, both scriptable. Bonus points for mentioning the chicken-and-egg cases: the sealed-secrets key must be restored *before* apps needing secrets (Day 28), and ArgoCD can even manage its own install once running.

**"App-of-apps children are also editable via kubectl — what stops config drift at that layer?"** The root app watches them like any resource: selfHeal reverts hand edits to child Applications, prune removes children whose files vanish. Drift protection is recursive because the pattern is.

## Stretch goals

- **Adopt ingress-nginx** — the high-stakes version of step 6. Extract live values (it was a manifest install, so build the equivalent ingress-nginx chart values for kind: hostPort, `ingress-ready` nodeSelector, controller service type), dry-run the diff with `argocd app diff` *before* enabling automated sync, and keep selfHeal off until the diff is empty. If you break it, every `*.localhost:8080` URL in the course dies — that pressure is the lesson.
- Make ArgoCD manage **itself**: an `argocd.yaml` child pointing at the upstream install manifest (or the argo-cd Helm chart) with your `argocd-cmd-params-cm` patch as an override. Sync waves: it should be wave `-1`.
- Add `argocd/apps/kube-prometheus-stack.yaml` as a *commented-out placeholder* with a `# Day 30` note — your future self will thank you.
- Explore `argocd app resources root` and `argocd admin app generate-spec` for scripting.

## Cleanup

Nothing to delete beyond what step 8 already removed (`doomed`). **Keep everything**: root, all child apps, the adopted metrics-server, `guestbook-helm`. The `argocd/apps/` directory is now the table of contents of your platform — Days 27, 28, 30+ each add a file to it, and Day 48 replays the whole thing onto an empty cluster.
