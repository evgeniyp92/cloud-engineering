# Day 48 — Capstone I: Burn It Down, Boot It Back

> **Time:** ~4 h · **Builds on:** Days 15, 26, 28, 45

## Objectives

- Destroy the `course` cluster on purpose and rebuild the entire platform from git with one script
- Solve the two chicken-and-egg problems: ArgoCD managing itself, and the sealed-secrets key that must exist *before* the controller
- Turn every "wait, that was installed by hand" discovery into a git commit, not a kubectl command
- Produce `BOOTSTRAP.md` — a runbook you could hand a stranger (or an interviewer)

## Concepts

### The thesis

Forty-seven days ago a cluster was precious. Today you prove it's disposable. The claim under test:

> **A cluster is cattle. The platform is: a git repo + one command + one key file.**

If that's true, "our cluster died" is a 30-minute inconvenience, region migration is a bootstrap with different flags, and staging-identical-to-prod is free. If it's false — if your platform secretly depends on commands someone once typed — today is where you find out, which is precisely why we do it before the course ends rather than at 3am after it.

### Bootstrap order: the dependency spine

Almost everything is "apply the root app and wait," but a strict spine of four steps must happen first, in order:

```
kind create ──► CNI (Cilium) ──► sealed-secrets KEY ──► ArgoCD ──► root.yaml ──► waves do the rest
   (1)             (2)                 (3)                (4)         (5)
```

1. **Cluster** — your Day 15 config has `disableDefaultCNI: true`, so nodes come up `NotReady` and pods sit `Pending`. Expected, not broken.
2. **CNI** — nothing schedules until pod networking exists. Cilium must be installed imperatively (helm) because ArgoCD itself can't run without it. The very bottom of the stack can't be GitOps'd from inside the cluster.
3. **The key** — see below; the ordering trap of the day.
4. **ArgoCD** — also imperative, once.
5. **Root app** — `argocd/root.yaml` points at `argocd/apps/`, and your Day 26 sync waves order everything else: sealed-secrets controller (wave 0), then monitoring/cert-manager/rollouts/kyverno, then the apps. One `kubectl apply` of ~20 lines, then you watch.

### Chicken-and-egg #1: who installs the installer?

ArgoCD can't sync itself into existence. The standard pattern is **imperative seed, then GitOps adopts itself**: install ArgoCD from the upstream manifest once, and (ideally) have an Application *in git* that points back at ArgoCD's own manifests/chart. From then on ArgoCD manages ArgoCD — config changes, upgrades — like any other app, and the imperative install only ever happens at bootstrap. If you never created that self-managing Application, today you'll feel the gap: any ArgoCD settings you hand-tweaked since Day 24 are gone. That's a finding, not a failure — fix it in git.

### Chicken-and-egg #2: the key that predates its controller

Every `SealedSecret` in your repo is ciphertext **encrypted to the public key of your old controller**. A fresh sealed-secrets controller generates a *brand-new* keypair on first boot — under a new key, every SealedSecret in git is undecryptable garbage and your apps boot without their secrets. The fix is ordering: apply the Day 28 key backup (`sealed-secrets-key-backup.yaml` — the Secret with the `sealedsecrets.bitnami.com/sealed-secrets-key: active` label, in the controller's namespace) **before** the controller's first start. On startup the controller adopts existing active keys and your old ciphertext decrypts like nothing happened. This is why Day 28 made you take that backup and why it lives **outside git** — it's the one true secret of the whole platform. Lose it and the recovery path is re-sealing every secret from source values (documented below — annoying, survivable).

### What's allowed to be imperative

A short, principled list: the bootstrap script itself (can live *in* git — it contains no secrets), the CNI install, the ArgoCD seed install, applying the key backup, applying root.yaml. Everything else — every app, dashboard, policy, cert issuer, CRD — must come from the repo. And one honest exclusion: **data**. GitOps reconciles *desired state*; your guestbook entries are not desired state, they're state-state. Database contents survive cluster death via backups (Velero, Day 43) or by living outside the cluster entirely — never via git.

## Lab

### 0. Pre-flight — do not skip

```sh
ls -la ~/sealed-secrets-key-backup.yaml   # wherever you stored it on Day 28
grep -m1 "sealed-secrets-key" ~/sealed-secrets-key-backup.yaml
```

**If it's missing, stop.** Recovery without it: after bootstrap, re-create each secret from its source values and re-seal with `kubeseal` against the *new* controller key, committing the new SealedSecrets. Tedious but bounded — and write that procedure into BOOTSTRAP.md either way, because someone on your future team will lose a key.

Commit all drift:

```sh
cd ~/Code/k8s-gitops && git status        # must be clean; commit or stash everything
argocd app list                            # everything Synced/Healthy? OutOfSync now = mystery later
```

Optional but recommended — save the guestbook data (Day 43), making the "data is not GitOps's job" lesson concrete:

```sh
velero backup create pre-capstone --include-namespaces guestbook --wait   # if Velero is still installed
```

(No Velero anymore? Quick alternative: `kubectl exec -n guestbook guestbook-db-0 -- pg_dump -U guestbook guestbook > ~/guestbook-precapstone.sql`.) If you skip this, the entries die with the cluster — which is also a fine lesson, just a deliberate one.

Finally, note the time. The claim is "full platform in under 30 minutes" — hold yourself to it.

### 1. Destruction

Before the trigger pull, take a manifest of what you're destroying — it doubles as your completeness check later:

```sh
kubectl get applications -n argocd -o name | sort > ~/pre-destroy-apps.txt
kubectl get crds -o name | wc -l    # note the number; you'll compare after rebuild
```

Then:

```sh
kind delete cluster --name course
docker ps        # gone. 47 days of work, deleted in 4 seconds.
```

Sit with that for a second, then notice you're calm, because:

```sh
ls ~/Code/k8s-gitops/argocd/apps/   # the platform is right there
```

### 2. The script

Write `~/Code/k8s-gitops/bootstrap.sh` yourself first. Requirements:

- `set -euo pipefail`; fail fast if the key backup file is missing
- `kind create cluster --name course --config <path to day-15 kind-config-cilium.yaml>`
- Helm-install Cilium (same version/values as Day 15), wait for nodes `Ready`
- Create the sealed-secrets controller's namespace and `kubectl apply` the key backup — **before anything else ArgoCD-related**
- Install ArgoCD from the upstream stable manifest into `argocd`; wait for `argocd-server` Available
- `kubectl apply -f argocd/root.yaml` — then the script's job is done

<details><summary>Solution</summary>

Complete commented reference: [`bootstrap.sh`](bootstrap.sh) in this folder. Copy it into `k8s-gitops`, then **adapt it**: your Cilium version/values from Day 15, your sealed-secrets namespace (the controller chart may use `kube-system` or `sealed-secrets` depending on how you installed it — the key backup must land in the same namespace the wave-0 Application installs the controller into), your key backup path.

```sh
cp ~/Code/cloud-engineer-course/day-48-capstone-bootstrap/bootstrap.sh ~/Code/k8s-gitops/
chmod +x ~/Code/k8s-gitops/bootstrap.sh
cd ~/Code/k8s-gitops && git add bootstrap.sh && git commit -m "bootstrap: cluster from zero" && git push
```

</details>

### 3. Run it

```sh
cd ~/Code/k8s-gitops
./bootstrap.sh
```

Then walk away from the keyboard — seriously. The discipline of *not* helping is the test. Watch instead:

```sh
kubectl get applications -n argocd -w
```

and in a second terminal, the ArgoCD UI:

```sh
kubectl port-forward svc/argocd-server -n argocd 8083:443
# password: kubectl get secret argocd-initial-admin-secret -n argocd -o jsonpath='{.data.password}' | base64 -d
```

### 4. Watch the waves converge

The order you designed on Day 26 plays out live. Roughly what the next ~20 minutes look like:

```
t+0m   root app Synced; child Applications appear (all OutOfSync/Missing)
t+1m   wave 0: sealed-secrets controller up — adopts your restored key
t+2m   wave 1: kube-prometheus-stack, loki, alloy, tempo, cert-manager,
       argo-rollouts, kyverno all Progressing (big charts; CRDs install first)
t+8m   wave 1 mostly Healthy; course-ca ClusterIssuer Ready
t+10m  app waves: podlab ApplicationSet stamps dev/stage/prod; guestbook syncs
t+12m  prod pulls ghcr image and goes Healthy; dev/stage wait on kind load (below)
t+20m  everything Synced/Healthy — or you have a findings list (step 5)
```

The single most important early check — did the controller adopt the **old** key instead of minting a new one:

```sh
kubectl logs -n <sealed-secrets-ns> deploy/sealed-secrets-controller | grep -i key
# want: "Existing key found" / the fingerprint of your backup — NOT "New key written"... only
```

If it minted a fresh key, your ordering slipped: fix the script, re-apply the backup, delete the controller pod (it re-reads keys on start), and note the finding.

**Images:** podlab-prod pulls `ghcr.io/USER/podlab` — Day 45 means the registry serves it, no action needed. But dev/stage overlays (and guestbook) still reference local images that died with the old cluster's nodes:

```sh
kind load docker-image podlab:v1 podlab:v2-traced guestbook:v1 --name course
```

Note that asymmetry in BOOTSTRAP.md: envs on a real registry bootstrap themselves; `kind load` envs are a manual step. (Stretch: push guestbook to ghcr too and delete the manual step.)

### 5. Reconciliation debugging — the real lesson

Something **will** be degraded. Every platform that hasn't rehearsed bootstrap has gaps; finding yours is the point of today. The rule: **every fix goes into git, never into the cluster** — a kubectl fix evaporates next bootstrap, and selfHeal will fight you anyway. Typical findings and their shapes:

| Symptom | Likely cause | Git fix |
|---|---|---|
| App `Failed`: "no matches for kind ServiceMonitor/Rollout/..." | CRD race — app in the same/earlier wave than the operator providing its CRDs | Later sync-wave on the consumer, or `SkipDryRunOnMissingResource=true` annotation, or a retry in syncPolicy |
| A namespace/issuer/secret an app expects doesn't exist | You created it by hand sometime in the last month | Add it to the repo (manifest in the right app, or a kyverno generate rule) |
| Pods `ImagePullBackOff` | Local-only image (see step 4) or ghcr package went private | `kind load` (document it) / fix package visibility |
| SealedSecret won't decrypt | Key applied after controller start, or wrong namespace | Re-check script ordering; delete the controller pod *after* fixing the key (it re-reads keys on start) |
| Some ArgoCD setting you remember is gone | ArgoCD config was hand-edited, never committed | Add an Application managing argocd itself (or its ConfigMaps) in git |
| `velero`/`minio` missing entirely | Day 43 was a helm install outside git | Decide: add as Applications, or declare them out of scope in BOOTSTRAP.md |

Work the list until `kubectl get applications -n argocd` is all `Synced`/`Healthy`. For each finding, one line in the runbook (next step) and one commit. These commits are the most valuable ones in the repo: each is a 3am page that now can't happen.

### 6. BOOTSTRAP.md — the deliverable

Write `~/Code/k8s-gitops/BOOTSTRAP.md`. Structure:

```markdown
# Platform Bootstrap Runbook
## Prerequisites          (tools + the key backup: where it lives, who has it, what to do if lost)
## Procedure              (./bootstrap.sh; expected duration; what "done" looks like)
## Post-bootstrap manual steps   (kind load list, anything you consciously left manual)
## Data restore           (velero restore / pg_restore — separate from platform bootstrap, on purpose)
## Findings log           ("2026-06-12: course-ca issuer was hand-applied → moved to cert-manager app, commit abc123")
## Re-seal procedure      (if the key is ever lost: per-secret source + kubeseal steps)
```

Commit and push. Then restore the data, proving the platform/data split:

```sh
velero restore create --from-backup pre-capstone --wait    # or: kubectl exec -i ... psql < ~/guestbook-precapstone.sql
curl -s http://guestbook.localhost:8080/entries             # the old entries return
```

Check the clock. Under 30 minutes of wall time for the platform (excluding your debugging detours)? That number goes in the runbook — and in interviews.

## Verify ✅

The full-platform checklist — every line must pass:

- [ ] `kubectl get applications -n argocd` → **all** Applications `Synced` + `Healthy`
- [ ] `diff ~/pre-destroy-apps.txt <(kubectl get applications -n argocd -o name | sort)` → empty (nothing silently lost)
- [ ] `kubectl get nodes` → 3 nodes `Ready`; `kubectl get pods -n kube-system -l k8s-app=cilium` running
- [ ] `curl -s http://podlab.prod.localhost:8080/config` → shows the secret value that only the **old** sealed-secrets key could decrypt — the single most important line on this list
- [ ] `curl -sk https://podlab.prod.localhost:8443/ -v 2>&1 | grep issuer` → your course CA issued the cert (cert-manager + ClusterIssuer came back from git)
- [ ] Grafana RED dashboard exists and receives data (provisioned from git, datasources up): generate a few requests, watch the panels move
- [ ] `kubectl get rollout podlab -n podlab-prod` → `Healthy` — argo-rollouts + AnalysisTemplates restored
- [ ] `kubectl create deploy bad --image=nginx -n podlab-dev` → **rejected** by kyverno (require-team-label) — policies are back and enforcing
- [ ] `kubectl get netpol -n <any new ns>` → kyverno's generated default-deny appears (create a throwaway ns to test)
- [ ] `curl -s http://guestbook.localhost:8080/entries` → serving; entries present **iff** you restored data (exactly as predicted)
- [ ] `git -C ~/Code/k8s-gitops log --oneline --since=today` → every fix from step 5 is a commit; BOOTSTRAP.md exists and is pushed

## Interview corner 💬

**"Your cluster burned down at 3am. Walk me through recovery."** The model answer is literally today, narrated:

> "Our platform is designed for this: everything is in a gitops repo reconciled by ArgoCD, and we rehearse the rebuild. I'd provision a fresh cluster, run our bootstrap script — it installs the CNI, restores the sealed-secrets private key from our offline backup *before* the controller starts so every encrypted secret in git remains valid, seeds ArgoCD, and applies one root Application. ArgoCD's sync waves bring back the platform in dependency order — secrets controller, then monitoring, cert-manager, policy engine, then the apps — about 25 minutes hands-off. Application data is deliberately separate: databases come back from Velero/pg backups, not git, because GitOps owns desired state, not data. We know this works because we've run it, our BOOTSTRAP.md logs every gap we found, and each gap became a commit. The honest risks: the key backup is a single point of failure — it's stored offline with a documented re-seal fallback — and anything ever fixed by hand instead of in git. Which is why we rehearse."

**"What belongs in the gitops repo, and what must not be in it?"**

> Strong answer: "In: every manifest, chart values, policies, dashboards, alert rules, the Application definitions themselves, the bootstrap script, the runbook — anything that's desired state and not secret plaintext. Encrypted secrets (SealedSecrets) are in; their decryption key is emphatically *out* — it's the one artifact that breaks the 'repo is public-safe' property and lives in offline backup. Also out: data (backups own that), generated/status fields, and real credentials of any kind, which we'd audit for with git history greps before ever making the repo public."

## Stretch goals

- **GitOps-adopts-ArgoCD**: add an Application that manages ArgoCD itself (the official chart with your values, or the install manifest via kustomize). Next bootstrap, ArgoCD upgrades/configures itself from git after the seed install.
- Push `guestbook` to ghcr with a Day 45-style workflow and delete the `kind load` step from BOOTSTRAP.md entirely.
- Run the whole day **again**, timed, zero debugging allowed. A runbook you've executed twice is a runbook; once is an anecdote.
- Add Velero + MinIO as wave-1 Applications so even your backup tooling is bootstrap-included — then think about the recursion: where do the backups live if MinIO dies with the cluster? (Answer: real DR needs the object store *outside* the cluster.)

## Cleanup

- Nothing to delete — the rebuilt platform **is** the artifact, and Days 49–50 run on it.
- Re-verify the sealed-secrets key backup is still where BOOTSTRAP.md says it is, still outside git: `git -C ~/Code/k8s-gitops log --all -p | grep -c "sealed-secrets-key"` should be `0`.
- If you restored guestbook data, delete the local SQL dump if you made one (`rm ~/guestbook-precapstone.sql`) — it's plaintext data lying around.
