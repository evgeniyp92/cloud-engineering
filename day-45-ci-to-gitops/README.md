# Day 45 — CI to GitOps: Closing the Loop

> **Time:** ~3.5 h · **Builds on:** Days 22, 27, 41

## Objectives

- Build a real CI pipeline: tag push → GitHub Actions → multi-arch image on ghcr.io, gated by a Trivy scan
- Deploy a CI-built image into your cluster with **no `kind load`** — the registry path real clusters use
- Install ArgoCD Image Updater and watch it commit an image bump to your gitops repo automatically
- Argue the tradeoffs between human-PR, CI-writes-back, and Image Updater bump strategies

## Concepts

### The missing piece

For 44 days your deploy pipeline had a human in the middle of it: you built an image, `kind load`-ed it, edited a tag in `k8s-gitops`, and pushed. ArgoCD handled everything after the git push — but everything *before* it was you, by hand. Today you automate the front half:

```
 code change ──► git tag v1.1.0 ──► GitHub Actions ──► ghcr.io/USER/podlab:v1.1.0
                                     (build/test/scan)          │
                                                                ▼
 ArgoCD ◄── k8s-gitops repo ◄──────────── something bumps newTag: v1.1.0
   │
   ▼
 cluster pulls the image and runs it          ← hands-free, end to end
```

The two halves have names, and the boundary between them matters:

- **CI** (Continuous Integration): build, test, scan, push an artifact to a registry. CI's job **ends at the registry**. CI never talks to the cluster.
- **CD** (Continuous Delivery): get that artifact running. In GitOps, CD = *a git commit changing the desired state* + ArgoCD reconciling it. The cluster pulls; nothing pushes into it.

That separation is the answer to a classic interview question. If CI has cluster credentials, you've built a push pipeline with a YAML repo on the side — a leaked Actions token can `kubectl delete ns prod`. In GitOps, CI's blast radius is the registry and (at most) write access to one repo.

### Who writes the bump?

The only remaining manual step is editing `newTag` in the gitops repo. Three ways to automate it:

| Strategy | How | Auditability | Automation | Complexity |
|---|---|---|---|---|
| **Human PR** | Dev opens a PR bumping the tag; reviewer merges | Best — a human approved prod | None | Zero |
| **CI commits the bump** | Last CI step clones gitops repo, `sed`s the tag, commits | Good — commit author = CI bot, tied to a build | Full | Low (a deploy key + 10 lines of shell) |
| **ArgoCD Image Updater** | A controller watches the *registry*, sees a newer tag matching a strategy (semver/digest/latest), writes the bump back via git | Good — bot commits with metadata | Full | Medium (one more controller, annotations, registry auth) |

There's no universally right answer. Human PR is common for prod at regulated shops. CI-commits-the-bump is the workhorse — many teams stop there and never regret it, because the logic lives in a pipeline they already debug daily. Image Updater shines when many apps share one gitops repo and you want "any semver-newer image deploys itself" as a *platform feature* rather than per-repo pipeline code. You'll build the CI half first, then add Image Updater so you've operated both.

### Why ghcr.io

You need a real registry — `kind load` was always a local crutch. ghcr.io (GitHub Container Registry) is free for public images and, critically, GitHub Actions can push to it using the **automatic `GITHUB_TOKEN`** — no long-lived password stored as a secret. The workflow asks for `permissions: packages: write` and logs in with a token that lives only for that job.

### Tags, semver, and why discipline matters

Your gitops repo will now reference `ghcr.io/USER/podlab:v1.1.0`. Three properties make that reference trustworthy:

1. **Immutable by convention** — never rebuild and re-push an existing tag. The tag in git must always mean the same bytes, or your audit trail lies. (Truly paranoid setups pin the **digest** — `podlab@sha256:...` — which is immutable by construction; Image Updater can write digests back too.)
2. **Semver-parseable** — `vMAJOR.MINOR.PATCH`. Image Updater's `semver` strategy *sorts* your tags; anything unparseable is ignored, anything parseable participates. Your tagging habits become deploy behavior.
3. **Built by CI only** — if a laptop can push the prod tag, the Trivy gate is decoration. In real orgs this is enforced with repo tag-protection rules and registry permissions; for this course, enforce it with discipline.

### The multi-arch trap

Your Mac is arm64. GitHub's runners are amd64. kind on Apple Silicon runs **arm64 node containers**, so an image built naively on a runner is amd64-only and your pods die with `exec format error` — one of the most common "works in CI, crashes in my cluster" mysteries. The fix is a **multi-arch manifest**: buildx + QEMU builds both `linux/amd64` and `linux/arm64`, pushes one tag, and each node pulls the layer set matching its architecture. Real clusters (mostly amd64, increasingly Graviton arm64) get the same benefit. Always check `docker manifest inspect` when an image mysteriously won't start.

## Lab

### Part 1 — CI builds the image

#### 1. Give podlab its own repo

CI wants an app repo with the Dockerfile at the root. Copy podlab out of the course repo:

```sh
cp -r ~/Code/cloud-engineer-course/apps/podlab ~/Code/podlab-app
cd ~/Code/podlab-app
git init && git add . && git commit -m "podlab: initial import"
gh repo create podlab-app --public --source=. --push    # or create on github.com and push
```

(Public repo → public ghcr images → no imagePullSecrets needed. Keep it public for this course.)

#### 2. The workflow

Create `.github/workflows/build-and-push.yaml`. Requirements — write it yourself first:

- Trigger: `on: push: tags: v*`
- `permissions: packages: write` so `GITHUB_TOKEN` can push to ghcr
- Steps: checkout → setup-qemu + setup-buildx → `docker/login-action` against `ghcr.io` with `${{ github.actor }}` / `${{ secrets.GITHUB_TOKEN }}` → run `go vet` + `go test` → build a local single-arch image → **Trivy scan, `exit-code: 1` on `CRITICAL`** (the Day 41 gate, now blocking the pipeline) → build + push `linux/amd64,linux/arm64` tagged `ghcr.io/USER/podlab:<tag>`

The complete reference is in this folder: [`build-and-push.yaml`](build-and-push.yaml). Compare yours against it, then commit:

```sh
mkdir -p .github/workflows
cp ~/Code/cloud-engineer-course/day-45-ci-to-gitops/build-and-push.yaml .github/workflows/
git add .github/workflows && git commit -m "ci: build, scan, push to ghcr"
git push
```

#### 3. Ship a tag

```sh
git tag v1.1.0
git push origin v1.1.0
gh run watch          # or watch the Actions tab in the browser
```

Read the log while it runs: the Trivy table (distroless base → it should be clean), then the multi-arch build pushing two platform variants plus a manifest list. ~3–5 minutes.

While you wait, read the workflow's two-build structure again: the single-arch `load: true` build exists *only* so Trivy can scan locally before anything is pushed — a failed scan means the registry never sees the image. Buildx layer caching makes the second (multi-arch) build cheap. If the Trivy step does fail on a base-image CVE someday: bump the base image in the Dockerfile, don't loosen the gate.

#### 4. Make the package public

ghcr packages default to **private** even from public repos — the #1 "ImagePullBackOff after my first CI push" cause. On github.com: your profile → Packages → `podlab` → Package settings → Change visibility → Public. Then prove the artifact is real:

```sh
docker manifest inspect ghcr.io/USER/podlab:v1.1.0 | grep -A2 platform
# or, without docker:
gh api /user/packages/container/podlab --jq .visibility    # → "public"
```

You should see **both** `amd64` and `arm64` entries. That manifest list is what makes the same tag work on your Mac's kind cluster and on an x86 EKS cluster.

#### 5. Deploy it — no kind load

Point the prod overlay at the registry. In `~/Code/k8s-gitops`, edit `kustomize/podlab/overlays/prod/kustomization.yaml`:

```yaml
images:
  - name: podlab                      # whatever name the base uses
    newName: ghcr.io/USER/podlab      # ← now a real registry path
    newTag: v1.1.0
```

```sh
cd ~/Code/k8s-gitops
git add . && git commit -m "podlab prod: ghcr.io image v1.1.0" && git push
kubectl get pods -n podlab-prod -w
```

Watch the new ReplicaSet pull from ghcr (`kubectl describe pod` → Events shows `Pulling image "ghcr.io/..."`). For the first time in this course, a pod is running code that **never touched your laptop's Docker daemon**: GitHub's runner built it, the registry stored it, the kubelet pulled it. This is the real pipeline. (dev/stage overlays still use the local `podlab` image + `kind load` — fine; prod is the one that must be honest.)

> If the rollout is managed by Argo Rollouts (Day 39), the canary steps run against your CI-built image. Let it promote — or watch the analysis pass on a real registry image, which is a nice preview of Day 49.

### Part 2 — Image Updater closes the loop

You still edited a YAML file by hand in step 5. Remove yourself.

#### 6. Install argocd-image-updater

It runs in the `argocd` namespace and shares ArgoCD's view of Applications:

```sh
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj-labs/argocd-image-updater/stable/config/install.yaml
kubectl rollout status deploy/argocd-image-updater -n argocd
```

(GitOps purist move: add it as an ArgoCD Application in `argocd/apps/` instead, sync-wave alongside argo-rollouts. Do that if you have the time — Day 48 will thank you.)

#### 7. Git write-back credentials

Image Updater needs to push to `k8s-gitops`. Give it a token via a secret (fine-grained PAT with `contents: read/write` on just that repo):

```sh
kubectl create secret generic git-creds -n argocd \
  --from-literal=username=USER \
  --from-literal=password=<github-PAT>
```

#### 8. Annotate the prod Application

Image Updater is configured **per Application via annotations**. Your podlab apps come from the Day 27 ApplicationSet, so add the annotations in the ApplicationSet's template (or, if you templated per-env metadata, only for prod) in `~/Code/k8s-gitops/argocd/apps/podlab-appset.yaml`:

```yaml
metadata:
  annotations:
    argocd-image-updater.argoproj.io/image-list: podlab=ghcr.io/USER/podlab
    argocd-image-updater.argoproj.io/podlab.update-strategy: semver
    argocd-image-updater.argoproj.io/write-back-method: git:secret:argocd/git-creds
    argocd-image-updater.argoproj.io/git-branch: main
```

`update-strategy: semver` means: any tag parsing as a higher semver wins. `write-back-method: git` means: commit the change to the repo (Kustomize apps get a `.argocd-source-<app>.yaml` override file, or the kustomization image field is updated) — the alternative, writing back to Application annotations only, skips git and loses the audit trail; don't use it. Commit and push; let ArgoCD sync the annotation change.

#### 9. The hands-free release

```sh
cd ~/Code/podlab-app
git commit --allow-empty -m "release v1.1.1"
git tag v1.1.1 && git push origin main v1.1.1
```

Now touch nothing and watch the chain:

```sh
gh run watch                                                  # 1. CI builds + pushes v1.1.1
kubectl logs -n argocd deploy/argocd-image-updater -f         # 2. "Setting new image to ghcr.io/USER/podlab:v1.1.1"
cd ~/Code/k8s-gitops && git pull && git log -1 -p             # 3. THE BOT'S COMMIT
kubectl get pods -n podlab-prod -w                            # 4. ArgoCD syncs, pods roll
```

Step 3 is the payoff. Read that commit: author `argocd-image-updater`, a diff showing exactly which tag changed, when, in which app. `git log` **is** your deploy history — `git revert` is your rollback. Total flow: `git tag` → running in prod, zero kubectl, zero YAML edits. (Default registry poll interval is 2 min; be patient or set `--interval` lower.)

#### 10. Failure modes — read before you trust it

- **Registry rate limits**: Image Updater polls every registry on its interval. Docker Hub's anonymous pull/metadata limits will throttle you in real orgs; ghcr is gentler, but caching and longer intervals matter at scale.
- **Semver discipline is load-bearing**: `update-strategy: semver` deploys *anything* that parses higher. A fat-fingered `v11.0.0` tag goes straight to prod. Tag protection rules on the app repo are now a prod control.
- **Two writers, one repo**: you and the bot both push to `k8s-gitops` — expect occasional non-fast-forward retries in the bot logs (it retries; still, know why).
- **Why many teams stop at CI-commits-the-bump**: the same audit trail, no extra controller watching every registry, and the bump logic sits in the pipeline devs already own. Image Updater earns its keep at "dozens of apps, one platform team" scale.

### Troubleshooting quick reference

| Symptom | Cause | Fix |
|---|---|---|
| Pod: `exec /podlab: exec format error` | Single-arch (amd64) image on arm64 kind | `platforms:` line missing in workflow; check `docker manifest inspect` |
| `ImagePullBackOff` + `denied` | ghcr package is private | Package settings → visibility Public (step 4) |
| CI push step: `403 Forbidden` | Workflow lacks `permissions: packages: write` | Add the permissions block at job level |
| Image Updater logs: nothing happens | Annotation typo, or tag doesn't parse as semver | `kubectl logs deploy/argocd-image-updater -n argocd` shows the image list it sees per app |
| Updater error: push rejected | PAT lacks write, or branch protection on main | Fine-grained PAT contents:rw; allow the bot or use a PR-based flow |

## Verify ✅

- [ ] `gh run list --workflow=build-and-push.yaml` → two successful runs (v1.1.0, v1.1.1)
- [ ] `docker manifest inspect ghcr.io/USER/podlab:v1.1.1 | grep architecture` → shows both `amd64` and `arm64`
- [ ] `kubectl get pods -n podlab-prod -o jsonpath='{.items[*].spec.containers[*].image}'` → `ghcr.io/USER/podlab:v1.1.1`
- [ ] `kubectl describe pod -n podlab-prod -l app=podlab | grep -A2 Events` history shows image pulled from ghcr, not `kind load`
- [ ] `cd ~/Code/k8s-gitops && git log --oneline -3` → contains a commit authored by the image updater bumping v1.1.0 → v1.1.1
- [ ] `kubectl logs -n argocd deploy/argocd-image-updater | grep -i "successfully updated"` → at least one hit

## Interview corner 💬

**"Walk me through what happens between a commit and production in your platform."** This is *the* question this course answers — have a 90-second version cold:

> "A dev merges to the app repo and tags a release. GitHub Actions builds a multi-arch image, runs tests and a Trivy scan that blocks on critical CVEs, and pushes to ghcr — CI ends at the registry, it has no cluster credentials. ArgoCD Image Updater notices the new semver tag and commits the version bump to our gitops repo, so the deploy itself is a git commit with a full audit trail. ArgoCD detects the drift between git and cluster and syncs. Because podlab is an Argo Rollout, the sync starts a canary: 20% of traffic, a Prometheus analysis on error rate, automatic promotion to 100% — or automatic rollback if the analysis fails, with an alert to the team. Rollback at any point is `git revert`. Nothing in that chain pushes into the cluster; the cluster pulls from git and the registry."

**"How do you keep CI from deploying? Why does that separation matter?"**

> Strong answer: CI's credentials reach the registry and (optionally) the gitops repo — never the cluster. Deployment is exclusively ArgoCD reconciling git. Benefits: a compromised pipeline can't touch the cluster directly; every deploy is a reviewable, revertible commit; the cluster can be rebuilt from the repo (Day 48); and "who deployed what when" is `git log`, not a CI dashboard that ages out.

**"Image Updater vs CI committing the bump — which would you pick?"**

> Strong answer names the tradeoff rather than a winner: CI-commits-the-bump for a handful of apps — simpler, logic lives where devs look. Image Updater when a platform team manages many apps in one repo and wants registry-driven updates as a feature, accepting another controller, registry polling costs, and strict tag hygiene. Human PR gate for regulated prod. Bonus points for "we'd start with CI write-back and revisit."

## Stretch goals

- Add `:v1.1` and `:v1` floating tags in the push step (`docker/metadata-action` does this well) and discuss why floating tags and GitOps are an awkward pair (Kyverno's disallow-latest exists for a reason).
- Generate an SBOM in CI (`anchore/sbom-action` or `trivy sbom`) and attach it to the GitHub release — Day 41's supply-chain story, automated.
- Move the image-updater install into `argocd/apps/` as a proper Application with a sync-wave, so Day 48's bootstrap brings it back automatically.
- Try `update-strategy: digest` on the dev overlay with a `latest` tag and compare the bot's commits — mutable-tag tracking for dev, semver for prod is a common real-world split.

## Cleanup

- Keep **everything**: the podlab-app repo, the workflow, the ghcr image, Image Updater, and the annotations. Days 48–49 depend on prod pulling from ghcr.
- If the PAT you minted is broader than `contents:rw` on `k8s-gitops`, re-issue it narrower now — it's a prod credential from today on.
