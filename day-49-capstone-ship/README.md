# Day 49 — Capstone II: Ship It Through the Platform

> **Time:** ~3.5 h · **Builds on:** Days 33, 39, 42, 45, 48

## Objectives

- Execute one release end-to-end with every platform muscle firing: CI → registry → git bump → canary → analysis → promote, hands off the cluster
- Ship a deliberately bad release and watch the platform contain it: failed analysis, auto-rollback, alert fired and silenced like an on-call would
- Demonstrate the guardrails saying no: Kyverno rejections and selfHeal reverting manual drift
- Produce a demo script + After-Action Report — the artifacts you'll actually show in interviews

## Concepts

### What "platform" means to an interviewer

When a platform team says they built a **paved road**, they mean: developers ship by pushing to git, and everything else — building, scanning, deploying, canarying, observing, rolling back, alerting — is machinery with guardrails. The dev never holds cluster credentials; the safe path is also the easy path. Yesterday proved your platform is *reproducible*. Today proves it *works as a product*: you'll run one good release and one bad one, touching the cluster with kubectl only to observe, never to act. That's the purity test: **if you have to `kubectl apply` anything today, the platform has a gap.**

The second idea today: **rehearse the demo**. Engineers who can show a live auto-rollback narrate it well in interviews because they've watched it with the dashboard open, know the timing ("analysis fails about 90 seconds in"), and have the timeline written down. The deliverable is a filled-in run-of-show ([`demo-script.md`](demo-script.md)) plus an After-Action Report — the difference between "we had canary deploys" and "let me walk you through the release on March 3rd where the canary caught a 40% error rate at the 20% step and rolled back in under two minutes; here's the AnalysisRun."

### The release scenario, precisely

Define it before you run it:

> **Scenario:** podlab-prod runs `vX.Y.Z` (your Day 45 result, e.g. v1.1.1). You release `vX.Y.Z+1` (good) and then `vX.Y.Z+2` (bad — elevated error rate). Good must reach 100% with no human action after `git push --tags`. Bad must never exceed the 20% canary slice, roll back automatically, and page (then be silenced with a comment, like an on-call would).

Mechanics inventory, all built on previous days — today composes, it doesn't build: CI + image updater (45), canary steps 20→50→100 with a Prometheus `success-rate` AnalysisTemplate on `podlab_http_requests_total` (39), RED dashboard + `podlab_build_info` (34/36), `PodlabHighErrorRate` → webhook (33), kyverno policies (42), selfHeal (25).

The demo in five acts:

| Act | What the audience sees | What it proves | ~Time |
|---|---|---|---|
| 1. Baseline | Green dashboard, Healthy rollout, Synced app | Steady state is observable | 2 min |
| 2. Good release | tag → CI → bot commit → canary → 100%, hands off | Commit-to-prod automation | 6 min |
| 3. Bad release | Analysis fails at 20% → auto-rollback → alert → silence | Failure is contained, rehearsed | 5 min |
| 4. Guardrails | Kyverno denials, selfHeal revert | The platform says no politely | 2 min |
| 5. AAR | The timeline with evidence links | You operate it, not just run it | — |

## Lab

### 1. Set the stage

Copy the run-of-show skeleton and keep it open — you'll fill it as you go:

```sh
cp ~/Code/cloud-engineer-course/day-49-capstone-ship/demo-script.md ~/Code/k8s-gitops/demo-script.md
```

Terminal layout (per the skeleton): **T1** traffic, **T2** rollout watch, **T3** git, **browser** Grafana RED dashboard (15-min window, 10s refresh) + ArgoCD UI side by side. Get every window up *before* anything moves:

```sh
# browser tabs (background these port-forwards in a scratch terminal):
kubectl port-forward svc/argocd-server -n argocd 8083:443 &
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80 &          # adjust names to your release
kubectl port-forward -n monitoring svc/kube-prometheus-stack-alertmanager 9093:9093 &   # you'll need it in act 3
# T2 needs the rollouts kubectl plugin from Day 38:
kubectl argo rollouts version
```

```sh
# T1 — steady traffic against the prod host (your Day 31/39 script; minimal version:)
while true; do curl -s -o /dev/null http://podlab.prod.localhost:8080/; sleep 0.2; done

# T2
kubectl argo rollouts get rollout podlab -n podlab-prod --watch
```

Record the baseline in the demo script: current version (`curl -s http://podlab.prod.localhost:8080/ | grep version` — should match your prod tag), error rate ~0%, rollout `Healthy`, app `Synced`. Pre-flight the analysis dependencies — 30 seconds now saves a confusing failure mid-demo:

```sh
kubectl get analysistemplate -n podlab-prod        # success-rate template from Day 39
kubectl get servicemonitor -n podlab-prod          # Prometheus is scraping podlab
```

### 2. The good release

In the podlab app repo, make a visible change and ship it the only way you're allowed to — git:

```sh
cd ~/Code/podlab-app
# make the change visible: e.g. set a default COLOR, or just an empty release commit
git commit --allow-empty -m "release: good" && git tag v1.2.0 && git push origin main v1.2.0
```

Now narrate the chain as each link fires (timestamps into the demo script):

1. **CI** (`gh run watch`): build → test → trivy gate → multi-arch push. ~4 min.
2. **The bump commit**: `cd ~/Code/k8s-gitops && git pull && git log -1` — image-updater's commit appears. The deploy *is* this commit.
3. **ArgoCD** notices, syncs; **T2** shows the canary begin: new ReplicaSet at **20%**, paused on analysis.
4. **The AnalysisRun, live**:

```sh
kubectl get analysisrun -n podlab-prod -w
kubectl describe analysisrun -n podlab-prod $(kubectl get analysisrun -n podlab-prod -o name | tail -1 | cut -d/ -f2) | tail -20
```

Read the measured values — real Prometheus answers about real traffic. 5. **Grafana**: requests split across two version labels; both healthy. If your RED dashboard doesn't already have a per-version panel, add one now (Explore is fine for the demo) — the Day 36 join:

```promql
sum by (version) (
  rate(podlab_http_requests_total[1m])
  * on (pod) group_left(version) podlab_build_info
)
```

Two lines, one per version, traffic mixing 80/20 — the single best visual of the day; screenshot it for the AAR. 6. Analysis passes → **50%** → **100%**; old ReplicaSet drains; the old version's line on the panel goes to zero.

Seal the "config survives releases" point:

```sh
curl -s http://podlab.prod.localhost:8080/config | grep -i <your-sealed-secret-key>   # still mounted
curl -skv https://podlab.prod.localhost:8443/ 2>&1 | grep -i issuer                    # still course CA
```

**You never touched the cluster.** Check the box in the demo script.

### 3. The bad release

Ship the Day 39 failure mode. The simplest reliable method: release a version, then point a slice of T1's traffic at `/error?rate=0.5` so the canary pods (which serve ~20% of requests) drive the success-rate below the analysis threshold. If you did Day 39's per-version-query stretch, even better — inject errors only via the canary and let the per-version query catch it; describe your method in the AAR either way.

```sh
cd ~/Code/podlab-app
git commit --allow-empty -m "release: bad (demo)" && git tag v1.2.1 && git push origin main v1.2.1
```

When T2 shows the canary at 20%, start the poison traffic in another pane:

```sh
while true; do curl -s -o /dev/null "http://podlab.prod.localhost:8080/error?rate=0.5"; sleep 0.3; done
```

Watch the containment sequence:

1. **T2**: AnalysisRun goes `Failed` — describe it and capture the measured success-rate for the AAR.
2. **Auto-rollback**: the Rollout aborts, canary ReplicaSet scales to 0, stable (v1.2.0) takes 100% again. Status shows `Degraded` with the abort reason; the *stable* version never served the bad code — only the 20% slice degraded, briefly. That's the entire value proposition of canary + analysis in one screen.
3. **The page**: kill the poison-traffic loop. Within the alert's `for:` window, `PodlabHighErrorRate` fires:

```sh
# Alertmanager UI (port-forward as in Day 33) shows FIRING; the webhook receiver logged the POST:
kubectl logs -n monitoring deploy/<your-webhook-receiver> --tail=20   # the JSON alert payload
```

4. **Be the on-call**: silence it properly — Alertmanager UI → Silence the alert, comment: `bad canary v1.2.1, auto-rolled-back, see AAR` — duration 2h. Silencing-with-a-comment is the professional move: the alert did its job, the system already recovered, the silence documents that a human acknowledged it. Watch it resolve.

Clean up the failed state in git, like a real team would (revert is the rollback):

```sh
cd ~/Code/k8s-gitops && git revert --no-edit HEAD && git push   # revert the v1.2.1 bump commit
# (also delete the bad tag upstream so image-updater can't re-deploy it: )
cd ~/Code/podlab-app && git push origin :refs/tags/v1.2.1
```

ArgoCD syncs back to v1.2.0; rollout returns `Healthy`.

### 4. The guardrails encore

Three quick attempts at doing the wrong thing — the platform says no politely:

```sh
# 1. :latest image → kyverno disallow-latest
kubectl run sneaky --image=nginx:latest -n podlab-prod
# Error from server: admission webhook denied: ... disallow-latest ...

# 2. unlabeled deployment → require-team-label
kubectl create deploy unlabeled --image=nginx:1.27 -n podlab-prod
# denied: label 'team' is required

# 3. manual change in prod → selfHeal
kubectl scale rollout/podlab -n podlab-prod --replicas=10   # rollouts have a scale subresource (Day 46!)
kubectl get rollout podlab -n podlab-prod -w                # watch ArgoCD revert to git's replica count in seconds
# (any drift works: scale guestbook's deploy, edit an env var — selfHeal reverts them all)
```

Capture each rejection message in the demo script — admission denials make great screenshots.

**Capture the evidence before it ages out** — the AnalysisRun is your proof and history limits will eventually garbage-collect it:

```sh
mkdir -p ~/Code/k8s-gitops/evidence
kubectl get analysisrun -n podlab-prod -o name | tail -1 | xargs -I{} \
  kubectl get {} -n podlab-prod -o yaml | kubectl neat > ~/Code/k8s-gitops/evidence/failed-analysisrun.yaml
kubectl argo rollouts get rollout podlab -n podlab-prod > ~/Code/k8s-gitops/evidence/rollout-after-abort.txt
```

#### Resetting between rehearsals

You'll run acts 2–3 more than once. The reset to baseline, in order: stop poison traffic → remove Alertmanager silences → revert any unreverted bump commits in k8s-gitops (`git revert`, push) → delete bad tags upstream (`git push origin :refs/tags/vX.Y.Z`) → wait for the rollout to show `Healthy` on the stable tag → confirm the error-rate panel is flat again. Two minutes; skipping it is how demos start in a dirty state and confuse everyone, you included.

### 5. Run it again — as a performance

You've now executed every act once, with debugging detours. Do one clean run-through (good release only is fine — reuse v1.2.0 by deleting/re-tagging or bump to v1.2.2) while narrating *out loud* to an empty room, demo script in hand, timing each act against the table above. The things you'll discover: which terminal you forgot to set up, how long the silences are while CI builds (have talking points ready — that's when you explain the trivy gate and multi-arch), and whether your story actually fits in 15 minutes. This rehearsal is the difference between a demo and a live debugging session with an audience.

### 6. The AAR

Fill the After-Action Report table in `demo-script.md`: timestamped rows from tag-push to alert-resolved for both releases, with evidence links (Actions run URL, bump-commit SHA, AnalysisRun name, webhook log line). Then the three honest lines: what auto-recovered, what needed hands, what you'd improve. Commit it:

```sh
cd ~/Code/k8s-gitops && git add demo-script.md && git commit -m "capstone: release demo AAR" && git push
```

This document is Day 50's raw material — and the thing you'll screen-share when an interviewer says "tell me about your deployment pipeline."

## Verify ✅

- [ ] Good release: `curl -s http://podlab.prod.localhost:8080/ | grep v1.2.0` — and your shell history shows **zero** `kubectl apply/edit/scale` between tag push and 100% (the purity test: `history | grep kubectl` — observation commands only)
- [ ] `kubectl argo rollouts get rollout podlab -n podlab-prod` → `Healthy`, stable image `...:v1.2.0`
- [ ] Bad release: an AnalysisRun with `Phase: Failed` exists (`kubectl get analysisrun -n podlab-prod`) and the rollout history shows the abort — keep that AnalysisRun's YAML as evidence
- [ ] Stable never degraded: Grafana error-rate panel for the stable version stayed flat through the bad release
- [ ] Alertmanager shows `PodlabHighErrorRate` fired AND resolved; webhook receiver log contains the POST; a silence with your comment existed
- [ ] Both kyverno attempts produced admission denials (messages captured)
- [ ] `kubectl scale` drift was reverted by selfHeal without your help
- [ ] `demo-script.md` fully checked off + AAR table filled, committed to k8s-gitops
- [ ] `evidence/failed-analysisrun.yaml` captured and committed — `grep -i phase` on it shows `Failed` with the measured value

## Interview corner 💬

**"Show me how a deploy fails safely in your platform."**

> "I'll describe a real run. We shipped a bad version — elevated error rate. It went out as a canary: Argo Rollouts shifted 20% of traffic to it and started an AnalysisRun, a Prometheus query measuring that the success rate stays above threshold. About 90 seconds in, the analysis failed — measured success around 70% — and the Rollout automatically aborted: canary pods scaled to zero, the stable version took back 100%. Blast radius: a fifth of traffic for under two minutes; the stable version never served bad code. Our error-rate alert fired and hit the on-call webhook, but it was informational — recovery had already happened; the on-call silenced it with a comment and we reverted the version-bump commit in the gitops repo. The key design point: the rollback decision is made by the same metrics humans watch, so it happens at machine speed at 2am, and every step left evidence — the AnalysisRun object, the alert, the git history."

**"What does 'platform engineering' mean, concretely?"**

> "Concretely: developers in my setup ship by pushing a git tag — that's their entire interface. The platform turns it into a scanned multi-arch image, a gitops commit, a canary rollout gated on live metrics, and either a promotion or an automatic rollback with an alert. Guardrails are automatic, not procedural: policy-as-code rejects `:latest` images and unlabeled workloads at admission; manual cluster changes are reverted by self-heal; secrets only exist encrypted in git. And it's all reproducible — one script rebuilds the whole thing from the repo. So 'platform engineering' = building that paved road and operating it as a product: the safe path is the easy path, the unsafe paths are blocked by machinery rather than by a wiki page."

**"Why did your alert fire if the rollback was automatic — isn't that noise?"**

> Strong answer: "Deliberate. The rollback handles the *immediate* incident; the alert ensures a *human knows* a bad version reached production — that's a process failure worth investigating even when contained. The on-call acknowledges with a silence + comment instead of acting. If those pages got frequent, that's a signal to fix the release process (better pre-prod testing), not to delete the alert."

## Stretch goals

- Record the demo: screen-capture the good+bad releases (QuickTime is fine), trim to ~10 minutes. A link to that video in your repo README outperforms most resumes.
- Do the per-version analysis stretch from Day 39 if you skipped it — failing the canary *by version label* rather than via aggregate error rate makes the bad-release demo crisper and survives traffic-mix questions.
- Add a Rollout notification (ArgoCD notifications or Argo Rollouts notifications) posting promote/abort events to the webhook receiver — release events alongside alert events.
- Run the bad release again but with the analysis threshold loosened so it *passes*, and watch the alert become your only safety net — then put the threshold back and write a sentence in the AAR about defense in depth.

## Cleanup

- Stop traffic loops (T1 and the poison loop). Remove the silence in Alertmanager if it hasn't expired.
- Confirm no stray `analysisruns` accumulating (`kubectl get analysisrun -n podlab-prod` — old ones are kept by history limits; fine) and prod is `Healthy` on v1.2.0.
- Keep everything else — the AAR, demo script, AnalysisRun evidence YAML, and the rebuilt platform are Day 50's inputs.
