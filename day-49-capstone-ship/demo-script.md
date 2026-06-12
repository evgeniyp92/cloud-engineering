# Platform Demo — Run of Show

> Fill in your own timestamps, version numbers, and screenshots as you execute.
> Target length when presenting live: 12–15 minutes.

## Terminal layout

- **T1** — traffic generator (`traffic.sh`, runs the whole time)
- **T2** — `kubectl argo rollouts get rollout podlab -n podlab-prod --watch`
- **T3** — git / gh CLI (app repo + gitops repo)
- **Browser tab A** — Grafana RED dashboard (last 15 min, 10s refresh)
- **Browser tab B** — ArgoCD UI, podlab-prod app

---

## 1. Baseline (talking point: "this is steady state — everything you'll see is observable")

- [ ] T1: traffic running against the prod host; ~___ req/s
- [ ] Tab A: RED dashboard green — error rate ___%, p95 ___ms
- [ ] T2: rollout Healthy, stable image `ghcr.io/___/podlab:v___`
- [ ] Tab B: app Synced + Healthy; point out git commit SHA shown by ArgoCD
- Time started: ____:____

**Say:** every deploy you're about to see is a git commit; I will not run kubectl apply at any point.

## 2. Good release (talking point: "commit-to-prod, hands off")

- [ ] T3: tag pushed `v___._._` at ____:____
- [ ] CI green (build+test+trivy+push) at ____:____  (link to run: __________)
- [ ] Image-updater commit in gitops repo at ____:____ (SHA: ________)
- [ ] T2: canary 20% at ____:____ — AnalysisRun running; show the Prometheus query
- [ ] Tab A: both versions visible on the dashboard (build_info join / per-version panel)
- [ ] T2: 50% → 100% promoted at ____:____; old ReplicaSet scaled down
- [ ] Tab A: error rate stayed at ___% throughout; `/config` still shows sealed secret; TLS padlock still course-CA

**Say:** the analysis gate is the same Prometheus the humans look at — promotion is evidence-based, not time-based.

## 3. Bad release (talking point: "failure is a rehearsed path, not an incident")

- [ ] Failure injected (method: ___________________) at ____:____
- [ ] T2: canary at 20%, AnalysisRun **Failed** at ____:____ (success-rate measured: ___)
- [ ] T2: automatic rollback — stable untouched, degraded slice was only ~20% for ___s
- [ ] Alert `PodlabHighErrorRate` FIRING at ____:____; webhook receiver logged the POST
- [ ] Silence created in Alertmanager (comment: "known bad canary, rolled back, ticket ___") at ____:____
- [ ] Alert resolved at ____:____

**Say:** nobody was paged to *do* anything — the page is informational; the rollback already happened.

## 4. Guardrails encore (talking point: "the paved road has guardrails")

- [ ] `:latest` image deploy attempt → kyverno rejection message shown
- [ ] Unlabeled deploy attempt → rejected (require-team-label)
- [ ] `kubectl scale` against prod → selfHeal reverted in ___s (show ArgoCD event)

## 5. Wrap

- [ ] AAR timeline written (below)
- [ ] Total demo time: ___ min

---

## After-Action Report — Release Timeline

| Time | Event | Actor | Evidence |
|---|---|---|---|
| | tag v__ pushed | me (git) | |
| | image on ghcr | CI | Actions run URL |
| | gitops bump commit | image-updater | commit SHA |
| | canary 20% | Argo Rollouts | rollout history |
| | analysis passed/failed | Prometheus | AnalysisRun yaml |
| | promoted / rolled back | Argo Rollouts | |
| | alert fired / resolved | Alertmanager | webhook log line |

What auto-recovered without me: ______________________________________
What I had to do by hand: ____________________________________________
What I'd improve: ____________________________________________________
