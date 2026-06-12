# Cloud Engineer Specialization — 50-Day Course

A self-paced, hands-on course: **1 day = 1 lesson**, 2–4 hours each. Everything
runs locally on your MacBook in [kind](https://kind.sigs.k8s.io/) clusters. By
Day 50 you will have built, operated, observed, secured, and progressively
delivered a complete GitOps platform — the same stack (Kubernetes, Helm, ArgoCD,
Prometheus/Loki/Grafana, Sealed Secrets, Argo Rollouts) used by real platform
teams, and you'll be able to talk about every piece of it in an interview.

**Legend:** 📦 = uses a demo app · 🎓 = CKA/CKAD-relevant material

## Prerequisites

You should already be comfortable with Docker (images, containers, registries,
Dockerfiles). That's the only assumed knowledge.

### Install the toolbelt

```sh
brew install kind kubectl helm k9s kubectx stern
```

(More tools are installed in the lessons that introduce them — argocd, kubeseal,
kustomize, trivy, velero, vault, kubeconform, etc.)

### Docker resources

Give your Docker VM (Docker Desktop → Settings → Resources, or OrbStack
equivalent) at least **8 GB RAM and 4 CPUs**. The observability phase runs
Prometheus + Loki + Grafana next to your apps; 12 GB is more comfortable.

## How each day works

Every `day-NN-*/README.md` has the same shape:

1. **Objectives** — what you'll be able to do afterwards
2. **Concepts** — the short "lecture" (~15 min read)
3. **Lab** — hands-on steps; you write most YAML yourself
4. **Verify** ✅ — concrete commands that *prove* it worked
5. **CKA corner** 🎓 — exam-style drills, where relevant
6. **Stretch goals** — optional extras if you have energy left
7. **Cleanup** — what to delete vs. keep for later days

## The demo apps

| App | What it is | Why it exists |
|---|---|---|
| [`apps/podlab`](apps/podlab/) | Tiny Go HTTP service | Makes Kubernetes observable: `/config` proves ConfigMap/Secret mounts, `/healthz/toggle` breaks probes on demand, `/load` drives HPA, `/error` fails canary analysis, `/metrics` + JSON logs feed the observability phase |
| [`apps/guestbook`](apps/guestbook/) | API + Postgres | The stateful one: StatefulSets, NetworkPolicies, Velero backup/restore |

You build both on Day 1 and reuse them for 50 days.

## Cluster lifecycle

- **Day 1** creates the long-lived `course` cluster (1 control-plane + 2 workers,
  ingress ports mapped to localhost).
- **Day 15** deliberately *recreates* it with Cilium as the CNI (NetworkPolicies
  need a CNI that enforces them). Clusters are cattle — by then everything you
  care about is reproducible from YAML, and rebuilding is the lesson.
- **Day 48** (capstone) starts from an empty cluster on purpose: one `kubectl apply`
  bootstraps the entire platform via ArgoCD.

---

## Curriculum

### Phase 1 — Kubernetes Foundations (Days 1–10)

| Day | Folder | Lesson |
|---|---|---|
| 1 | `day-01-cluster-and-toolbelt` | First multi-node kind cluster; kubectl, **k9s**, kubectx/kubens, stern; kubeconfig & contexts; build + load the demo apps |
| 2 | `day-02-pods-deep-dive` | Pod lifecycle, init containers, sidecars, Downward API, ephemeral debug containers 📦🎓 |
| 3 | `day-03-deployments` | Deployments & ReplicaSets: rolling updates, rollbacks, revision history, deployment strategies 📦🎓 |
| 4 | `day-04-services-and-dns` | ClusterIP / NodePort / LoadBalancer (cloud-provider-kind), CoreDNS, service discovery 📦🎓 |
| 5 | `day-05-ingress` | ingress-nginx on kind, host/path routing, reaching apps on localhost 📦 |
| 6 | `day-06-configmaps` | Env vars vs mounted files, overriding files in pods, subPath, update propagation — verified via podlab `/config` 📦🎓 |
| 7 | `day-07-secrets` | Secret types, projected volumes, why base64 ≠ encryption (sets up Sealed Secrets) 📦🎓 |
| 8 | `day-08-resources-and-qos` | Requests/limits, QoS classes, metrics-server, `kubectl top`, an OOMKill lab 📦🎓 |
| 9 | `day-09-namespaces-and-labels` | Namespaces, labels, selectors, annotations, field selectors; organizing a cluster 🎓 |
| 10 | `day-10-probes-and-shutdown` | Liveness/readiness/startup probes, preStop hooks, SIGTERM & graceful shutdown 📦🎓 |

### Phase 2 — Workloads & Cluster Internals (Days 11–18)

| Day | Folder | Lesson |
|---|---|---|
| 11 | `day-11-statefulsets-and-storage` | StatefulSets, headless Services, PV/PVC, storage classes — deploy guestbook + Postgres 📦🎓 |
| 12 | `day-12-daemonsets-jobs-cronjobs` | DaemonSets, Jobs, CronJobs, restart/backoff semantics 🎓 |
| 13 | `day-13-scheduling` | nodeSelector, affinity/anti-affinity, taints & tolerations, topology spread 📦🎓 |
| 14 | `day-14-rbac` | ServiceAccounts, Roles/ClusterRoles, bindings, `kubectl auth can-i`, build a CI-bot kubeconfig 🎓 |
| 15 | `day-15-network-policies` | Recreate the cluster with **Cilium**; default-deny + allow rules, proven by curling between pods 📦🎓 |
| 16 | `day-16-cluster-internals-etcd` | Control-plane tour, static pods, **etcd backup & restore drill**, PKI certificates 🎓 |
| 17 | `day-17-troubleshooting-gauntlet-1` | Six broken-on-purpose workloads to diagnose with k9s, events, and logs 📦🎓 |
| 18 | `day-18-autoscaling` | HPA driven by podlab `/load`; VPA and cluster-autoscaling concepts 📦🎓 |

### Phase 3 — Helm & Packaging (Days 19–23)

| Day | Folder | Lesson |
|---|---|---|
| 19 | `day-19-helm-consumer` | Repos, install/upgrade/rollback, values files, `helm diff` |
| 20 | `day-20-helm-authoring` | Write a chart for podlab from scratch: templates, `_helpers.tpl`, values design 📦 |
| 21 | `day-21-helm-advanced` | Dependencies/subcharts, hooks, `helm test`, packaging, OCI registries 📦 |
| 22 | `day-22-kustomize` | Bases & overlays, patches, Helm-vs-Kustomize decision framework (sets up multi-env GitOps) 📦 |
| 23 | `day-23-manifest-quality` | helm lint, kubeconform, kube-score, chart-testing — a quality gate you'd run in CI |

### Phase 4 — GitOps with ArgoCD (Days 24–29)

| Day | Folder | Lesson |
|---|---|---|
| 24 | `day-24-argocd-install` | GitOps principles; install ArgoCD; first Application from a Git repo; UI + CLI 📦 |
| 25 | `day-25-argocd-sync-policies` | Auto-sync, self-heal, prune; hand-edit the cluster and watch drift get reverted 📦 |
| 26 | `day-26-app-of-apps` | App-of-apps pattern, sync waves & hooks: bootstrap a whole stack from one Application 📦 |
| 27 | `day-27-applicationsets` | ApplicationSets & generators: dev/stage/prod from Kustomize overlays 📦 |
| 28 | `day-28-sealed-secrets` | **Sealed Secrets**: kubeseal workflow, encrypted secrets in Git, key rotation & DR 📦 |
| 29 | `day-29-secrets-landscape` | External Secrets Operator + local Vault (dev mode); SOPS comparison; choosing a strategy 📦 |

### Phase 5 — Observability (Days 30–37)

| Day | Folder | Lesson |
|---|---|---|
| 30 | `day-30-prometheus-stack` | Prometheus architecture; kube-prometheus-stack via Helm/ArgoCD; targets & service discovery |
| 31 | `day-31-promql` | PromQL bootcamp: selectors, `rate()`, aggregations, `histogram_quantile` 📦 |
| 32 | `day-32-servicemonitors` | Scrape your own app: ServiceMonitor/PodMonitor for podlab `/metrics` 📦 |
| 33 | `day-33-alerting` | PrometheusRule, Alertmanager routing & silences; fire a real alert by breaking podlab 📦 |
| 34 | `day-34-grafana` | Dashboards, variables, and provisioning dashboards as code (GitOps'd) 📦 |
| 35 | `day-35-loki` | Loki + Alloy log pipeline; LogQL over podlab's JSON logs 📦 |
| 36 | `day-36-correlation-red` | Metrics ↔ logs correlation in Grafana; build a RED dashboard 📦 |
| 37 | `day-37-tracing-otel` | OpenTelemetry + Tempo; add a trace span to podlab 📦 |

### Phase 6 — Progressive Delivery, Security & Resilience (Days 38–44)

| Day | Folder | Lesson |
|---|---|---|
| 38 | `day-38-rollouts-bluegreen` | **Argo Rollouts** I: blue-green with preview service and manual promotion 📦 |
| 39 | `day-39-rollouts-canary` | **Argo Rollouts** II: canary steps + automated analysis from Prometheus → auto-rollback (flagship demo) 📦 |
| 40 | `day-40-cert-manager` | cert-manager: local CA, TLS on ingress, certificate lifecycle 📦 |
| 41 | `day-41-image-security` | Trivy scanning, SBOMs, Pod Security Standards, securityContext hardening 📦 |
| 42 | `day-42-kyverno` | Policy as code with Kyverno: validate, mutate, generate 📦 |
| 43 | `day-43-velero-backup` | Velero + MinIO: back up guestbook, destroy it, restore it — with its data 📦 |
| 44 | `day-44-troubleshooting-gauntlet-2` | Cluster-level breakage: DNS, certificates, RBAC, networking 🎓 |

### Phase 7 — CI/CD Integration & Capstone (Days 45–50)

| Day | Folder | Lesson |
|---|---|---|
| 45 | `day-45-ci-to-gitops` | GitHub Actions builds & pushes podlab; ArgoCD Image Updater closes the loop 📦 |
| 46 | `day-46-crds-operators` | CRDs & the operator pattern; install CloudNativePG; write a CRD by hand 📦 |
| 47 | `day-47-day2-ops` | krew & kubectl plugins, capacity review (Goldilocks), drain/cordon/upgrade drills 🎓 |
| 48 | `day-48-capstone-bootstrap` | Capstone I: empty cluster → full platform via ArgoCD app-of-apps 📦 |
| 49 | `day-49-capstone-ship` | Capstone II: ship the apps through your platform — canary, alerts, dashboards, sealed secrets, TLS, policies 📦 |
| 50 | `day-50-capstone-interview` | Capstone III: architecture diagram, interview talking points, timed CKA-style drills, gap checklist |

---

## After Day 50

You'll have a Git repo that *is* your portfolio: a bootstrappable platform with
GitOps, progressive delivery, full observability, and secrets management. Three
suggested next steps:

1. **Take the CKA.** The 🎓 drills cover a large share of the curriculum; fill
   gaps with timed practice (killer.sh).
2. **Move one piece to a real cloud** (EKS/GKE) — swap local-path storage for
   EBS/PD, ingress-nginx for a cloud LB, and feel what changes.
3. **Write it up.** A blog post walking through your Day 48–49 bootstrap is a
   better interview artifact than most certifications.
