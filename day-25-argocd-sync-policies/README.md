# Day 25 — Sync Policies: Self-Heal, Prune, and Living with Controllers

> **Time:** ~3 h · **Builds on:** Day 24

## Objectives

- Upgrade `podlab-helm` from manual to fully automated sync, and articulate exactly what `automated`, `selfHeal`, and `prune` each add.
- Watch ArgoCD revert a hand-made cluster change in seconds, and prune a resource deleted from git.
- Resolve the classic **ArgoCD-vs-HPA replica fight** with `ignoreDifferences` + `RespectIgnoreDifferences`.
- Use sync options, sync windows, and Application finalizers deliberately instead of cargo-culting them.

## Concepts

### Three switches, three different problems

`syncPolicy.automated` is not one feature — it's three, and they answer **different kinds of drift**:

| Setting | Drift it answers | Without it |
|---|---|---|
| `automated: {}` | *git moved ahead of the cluster* — new commit | app sits OutOfSync until a human syncs |
| `selfHeal: true` | *the cluster moved away from git* — someone `kubectl edit`ed/`scale`d/deleted | auto-sync only fires on new commits; live drift sits OutOfSync forever |
| `prune: true` | *git shrank* — a resource was deleted from the repo | the resource stays in the cluster, marked OutOfSync, never removed |

Think of git state and live state as two pointers. `automated` chases git forward. `selfHeal` drags the cluster back. `prune` lets the chase include deletions — which is why it's the scary one: a bad commit that drops a file deletes production resources. Teams routinely run `automated + selfHeal` everywhere and enable `prune` only after they trust their review process (or with `PruneLast`, below).

Mechanics worth knowing: selfHeal triggers when ArgoCD's watch sees a live change, with a short revert delay (default 5s, backing off on repeated drift). Prune only deletes resources ArgoCD *tracks* — ones it previously applied and labeled — never random bystanders in the namespace.

### Sync options

Per-app `syncPolicy.syncOptions`, the four you'll actually use:

- **`CreateNamespace=true`** — ArgoCD creates the destination namespace before syncing. Removes yesterday's manual `kubectl create namespace` step; essential for Day 27 where namespaces are templated.
- **`ApplyOutOfSyncOnly=true`** — apply only the resources that differ instead of everything. Matters for apps with hundreds of resources (Day 30's kube-prometheus-stack).
- **`ServerSideApply=true`** — use server-side apply instead of `kubectl apply`'s client-side three-way merge. Fixes "annotation too long" on huge CRDs, and gives saner field ownership when multiple controllers manage one object.
- **`PruneLast=true`** — within a sync, do all creates/updates first, prune after. New version fully up before the old one's leftovers go.

### When two controllers want the same field

GitOps assumes git owns the spec. But Kubernetes is full of controllers that legitimately write into specs at runtime — the HPA writes `Deployment.spec.replicas`, cert-manager writes Secret data, some webhooks inject fields. With `selfHeal` on, ArgoCD reverts those writes: **two reconcilers, one field, infinite fight**.

The escape hatch is `ignoreDifferences`: tell ArgoCD which fields to exclude from its diff. Crucially, ignoring a field in the *diff* doesn't stop a *sync* from overwriting it — for that you also need the `RespectIgnoreDifferences=true` sync option, which makes syncs leave ignored fields untouched. You need both; forgetting the second is a classic half-fix. (The even cleaner fix: don't render `replicas` in your manifests at all when an HPA owns it — Helm charts often gate it with `if not .Values.autoscaling.enabled`. Know both answers.)

### Application finalizers

`metadata.finalizers: [resources-finalizer.argocd.argoproj.io]` on an Application changes what *deleting the Application* means: with the finalizer, ArgoCD deletes all managed resources first (cascade); without it, the Application object vanishes and the workload keeps running, orphaned. Neither is "correct" — cascade is what you want for ephemeral envs, non-cascade is a safety net for prod app definitions. Day 26 demos this.

### Keep the Application spec itself declarative

One trap before you start: the `argocd` CLI can mutate Applications directly — `argocd app set podlab-helm --sync-policy automated`, `--self-heal`, and the UI's "Enable auto-sync" button all edit the live CRD. Convenient, and exactly how your `argocd/apps/podlab.yaml` goes stale: the file in git says one thing, the cluster's Application says another, and there's no controller reconciling *that* gap — yet. (Day 26 closes it: a root app will manage the Application manifests too, and any `argocd app set` will get selfHealed away like every other drift.) Build the habit today: every policy change below is an edit to the YAML file, applied with kubectl, committed, pushed. The CLI is for *reading* (`app get`, `app diff`, `app history`) and for deliberate one-off actions (`app sync`).

### Sync windows

AppProjects can define **sync windows** — cron-scheduled allow/deny periods for syncing. Classic use: deny automated syncs to prod outside business hours, so a 2 a.m. merge doesn't roll prod while nobody's watching, with `manualSync: true` so a human override still works during the window.

## Lab

All edits below go into `~/Code/k8s-gitops/argocd/apps/podlab.yaml`, then `kubectl apply -f` it (until Day 26 automates that, too). Commit and push after each stage — the file in git should always match what you applied.

### 0. Baseline

Confirm where yesterday left you, and that file and cluster agree:

```sh
argocd app get podlab-helm | grep -E 'Sync Policy|Sync Status|Health Status'   # Manual / Synced / Healthy
kubectl get application podlab-helm -n argocd -o jsonpath='{.spec.syncPolicy}'; echo   # empty — no policy yet
git -C ~/Code/k8s-gitops status --short                                        # clean
```

### 1. Stage 1 — automated

Add to the Application spec:

```yaml
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
```

```sh
kubectl apply -f argocd/apps/podlab.yaml
```

Test it: change `color` in `charts/podlab/values.yaml`, commit, push, and just watch (`argocd app get podlab-helm --refresh` to skip the poll wait). No sync command — the app goes OutOfSync → Syncing → Synced on its own. Commits now deploy themselves.

### 2. Stage 2 — selfHeal: the wow moment

`automated` alone ignores cluster-side drift. Prove it, then fix it. First, in one terminal:

```sh
kubectl get deploy podlab -n argocd-podlab -w
```

In another, play the 2 a.m. cowboy:

```sh
kubectl scale deploy podlab -n argocd-podlab --replicas=5
```

The watch shows 5. `argocd app get podlab-helm --refresh` → OutOfSync — detected, reported, **not reverted**. Now upgrade the policy:

```yaml
  syncPolicy:
    automated:
      selfHeal: true
```

Apply it and watch the first terminal: within seconds, replicas snap back to your chart's count without you touching anything. Do it again — `kubectl scale --replicas=5` — and time the revert. *This* is the demo to remember for interviews: the cluster now actively refuses to drift from git. Try `kubectl delete svc podlab -n argocd-podlab` too; the Service is back before your curl fails twice.

### 3. Stage 3 — prune

Delete `charts/podlab/templates/service.yaml` (yes, really — git remembers):

```sh
cd ~/Code/k8s-gitops
git rm charts/podlab/templates/service.yaml
git commit -m "drop service (prune demo)" && git push
```

`argocd app get podlab-helm --refresh`: the app auto-syncs the rest but the Service shows OutOfSync with a `requires pruning` marker — automated sync **won't delete** without permission. Grant it:

```yaml
  syncPolicy:
    automated:
      selfHeal: true
      prune: true
```

Apply → next reconcile prunes the Service (`kubectl get svc -n argocd-podlab` → gone; the UI shows it disappearing from the tree). Now restore it, because the ingress needs it:

```sh
git revert --no-edit HEAD && git push
```

Watch the Service come back on its own. Note what you just did: **rollback = `git revert`**. No helm rollback, no kubectl, and the audit trail shows both the mistake and the fix.

### 4. The HPA fight — and the truce

Give the deployment an HPA that disagrees with git (git says 2 replicas; HPA insists on at least 3):

```sh
kubectl autoscale deploy podlab -n argocd-podlab --min=3 --max=6 --cpu-percent=50
kubectl get deploy podlab -n argocd-podlab -w
```

Watch the fight: HPA scales to 3 → selfHeal reverts to 2 → HPA scales to 3 → … The app flaps OutOfSync/Synced in the UI, and both controllers burn cycles undoing each other. This is not hypothetical; it's the most common ArgoCD incident writeup on the internet.

Declare the truce in the Application — requirements: ignore `spec.replicas` on the `apps/Deployment`, and make syncs respect it.

<details><summary>Solution</summary>

```yaml
spec:
  # ...existing source/destination...
  ignoreDifferences:
    - group: apps
      kind: Deployment
      jsonPointers:
        - /spec/replicas
  syncPolicy:
    automated:
      selfHeal: true
      prune: true
    syncOptions:
      - CreateNamespace=true
      - RespectIgnoreDifferences=true
```

</details>

Apply, then watch the deployment settle at 3 (HPA's floor) while the app reports **Synced** — replicas are no longer ArgoCD's business. Confirm peace: `kubectl get deploy podlab -n argocd-podlab -w` stays quiet for a minute. Commit and push the final Application state — Day 26 depends on this file being current. Then remove the HPA (it served its purpose): `kubectl delete hpa podlab -n argocd-podlab`.

### 5. Sync window demo

Block syncs as if `podlab-helm` were prod-at-night. Sync windows live on the **AppProject**:

```sh
kubectl patch appproject default -n argocd --type merge -p '
spec:
  syncWindows:
    - kind: deny
      schedule: "* * * * *"
      duration: 5m
      applications: [podlab-helm]
      manualSync: true
'
```

(`* * * * *` + 5m = a window that's always active right now — instant demo.) Push any trivial chart change: the app goes OutOfSync and *stays* there; `argocd app get podlab-helm` shows the deny window. `argocd app sync podlab-helm` still works because `manualSync: true` — the emergency override. Then remove the window:

```sh
kubectl patch appproject default -n argocd --type json \
  -p '[{"op":"remove","path":"/spec/syncWindows"}]'
```

### 6. Orphans and notifications (quick looks)

- Enable orphan warnings: `kubectl patch appproject default -n argocd --type merge -p '{"spec":{"orphanedResources":{"warn":true}}}'`, then `kubectl create configmap stray -n argocd-podlab --from-literal=a=b`. The UI now shows an orphaned-resources warning on the app — resources in a managed namespace that no Application owns. Delete the stray ConfigMap after.
- Notifications exist and are annotation-driven — e.g. `notifications.argoproj.io/subscribe.on-sync-failed.slack: my-channel` on an Application, with triggers/templates in `argocd-notifications-cm`. File it away; no Slack today.

## Verify ✅

- [ ] `kubectl get application podlab-helm -n argocd -o jsonpath='{.spec.syncPolicy.automated}'` → `{"prune":true,"selfHeal":true}`
- [ ] `kubectl scale deploy podlab -n argocd-podlab --replicas=5` → `kubectl get deploy podlab -n argocd-podlab` shows the git replica count again within ~30 s, no human involved
- [ ] During step 3, `kubectl get svc -n argocd-podlab` showed no `podlab` Service; after the revert it exists and `curl http://podlab-helm.localhost:8080/healthz` → `{"status":"ok"}`
- [ ] With the HPA present and `ignoreDifferences` in place: deployment replicas = 3, yet `argocd app get podlab-helm` → `Synced`, and the watch shows no flapping
- [ ] During step 5, a pushed change stayed `OutOfSync` until `argocd app sync` — and `argocd app get podlab-helm` mentioned the sync window
- [ ] `git -C ~/Code/k8s-gitops log --oneline -5` reads like a deploy history — including the revert

## Interview corner 💬

**"What happens if someone kubectl-edits a prod deployment under your GitOps setup?"** With selfHeal enabled, ArgoCD detects the live-state change via its watch and re-applies git within seconds — the edit evaporates, and the event is visible in the app's sync history. Without selfHeal, the app turns OutOfSync and alerts on that status; either way the drift is *visible*, which is half the value. The follow-up I'd volunteer: fields legitimately owned by other controllers (HPA replicas) must be excluded via `ignoreDifferences` + `RespectIgnoreDifferences`, or you build a controller fight.

**"How do you make an emergency manual change at 3 a.m. under GitOps?"** Preferably you don't — `git revert` of the bad commit *is* the emergency path, and auto-sync ships it in seconds. If the cluster must be touched directly (git is down, sync is broken): disable auto-sync on the app (`argocd app set --sync-policy none`) or use a sync window's manual override, make the change, and **backfill the commit immediately** so git and cluster reconverge before re-enabling automation. The discipline is: the manual change is a loan, the commit repays it.

## Stretch goals

- Break prune on purpose: with prune enabled, move a manifest to a *renamed* file and change the resource's name in one commit — watch ArgoCD create the new and prune the old. Now reason about ordering and where `PruneLast=true` would matter.
- Add `spec.revisionHistoryLimit: 3` and explore `argocd app history podlab-helm` / `argocd app rollback podlab-helm <id>` — then explain why rollback-via-CLI disables auto-sync and why `git revert` is still the better tool.
- Set the selfHeal timing knobs (`timeout.reconciliation` in `argocd-cm`) and measure detection latency with a stopwatch.
- Read your `podlab` Deployment's `metadata` and find ArgoCD's tracking label (`app.kubernetes.io/instance: podlab-helm`) — this is how prune knows what it owns.

## Cleanup

```sh
kubectl delete hpa podlab -n argocd-podlab --ignore-not-found
kubectl delete configmap stray -n argocd-podlab --ignore-not-found
```

**Keep:** ArgoCD, `podlab-helm` with its final automated+selfHeal+prune policy, and the committed `argocd/apps/podlab.yaml` — Day 26 turns that file into a managed child of a root app.
