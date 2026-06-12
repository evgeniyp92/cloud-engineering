# Day 19 — Helm as a Consumer

> **Time:** ~2.5 h · **Builds on:** Days 3, 6

## Objectives

- Explain what a chart, a release, a repo, and a values file are — and why Helm exists at all
- Install, upgrade, and roll back a third-party chart (`podinfo`) driven by a values file
- Inspect any chart **before** installing it (`helm show values`, `helm template`, `--dry-run`)
- Make `helm diff` a reflex before every upgrade

## Concepts

### Why Helm exists

You've spent 18 days writing raw YAML and applying it with `kubectl apply -f dir/`.
That works until three things happen, and in any real team all three happen:

1. **Parameterization.** Dev wants 1 replica, prod wants 5. The image tag changes
   every release. With raw YAML you either copy the directory per environment
   (drift guaranteed) or sed your manifests in CI (crimes).
2. **Lifecycle.** `kubectl apply -f dir/` has no concept of "everything that
   belongs to this app, as installed on March 3rd". You can't roll *the app*
   back — only individual Deployments. And if you delete a file from the dir,
   `apply` won't delete the object from the cluster.
3. **Distribution.** How does someone else install your app, with *their*
   settings, without reading every line of your YAML? How do you install
   Prometheus (hundreds of objects) without authoring any of it?

Helm answers all three: a **chart** is a versioned bundle of templated manifests
with a documented settings surface, and every install is tracked as a
**release** that can be upgraded, diffed, and rolled back as a unit. In Phase 5
you'll install the entire Prometheus stack with one Helm command — that's the
payoff.

### Vocabulary

| Term | What it is | Analogy (Docker) |
|---|---|---|
| **Chart** | Versioned package of templates + default values | Image |
| **Release** | A chart installed into a namespace under a name | Container |
| **Repository** | An HTTP index of packaged charts | Registry |
| **Values** | The parameters a chart exposes; your `-f values.yaml` overrides defaults | `docker run -e ...` |
| **Revision** | One version of a release's history; every upgrade/rollback increments it | — |

One chart → many releases. You can install `podinfo` five times under five
names; each is an independent release with independent history.

### Where Helm stores state

Helm has no server component (Tiller died with Helm 2). Release state lives
**in the cluster**, as Secrets of type `helm.sh/release.v1` in the release's
namespace — one Secret per revision. That's why `helm list` needs a kubeconfig,
why RBAC on Secrets affects Helm, and why "where is my release history?" has a
kubectl answer.

### The golden rule: never blind-install

A chart is arbitrary YAML written by a stranger. Before any install:

1. `helm show values <chart>` — the full settings surface with defaults.
2. `helm template <chart>` — render locally and read what would be created.
3. `helm install --dry-run` — render **server-side** (real capabilities, real
   validation), still creating nothing.

`helm template` runs entirely client-side: fast, no cluster needed, ideal for
CI (Day 23 builds a quality gate on it). `--dry-run=server` talks to the API
server, so functions like `lookup` work and the manifests are validated against
your actual cluster version. Use `template` for pipelines, `--dry-run` for
"will this install on *this* cluster".

### Upgrades, history, rollbacks

`helm upgrade` computes a new manifest set, applies the diff, and writes
revision N+1. `helm rollback <release> <rev>` doesn't rewind history — it
creates a *new* revision whose content equals the old one (just like
`kubectl rollout undo` on Day 3). `helm history` shows the whole trail.

Two ways to pass values, both used today:

| Mechanism | Use for |
|---|---|
| `-f values.yaml` | Everything you want versioned in Git (the normal case) |
| `--set key=value` | One-off experiments, CI-injected image tags |

`--set` wins over `-f` when both set the same key. Beware: by default an
upgrade **reuses nothing** — values come from defaults + what you pass *this
time*. Keep your values file authoritative and always pass it.

### helm diff

The single best Helm habit: the [helm-diff plugin](https://github.com/databus23/helm-diff)
renders what an upgrade *would* change and shows a colored diff against the
live release. Before every upgrade, diff. ArgoCD (Phase 4) gives you the same
idea as a UI; `helm diff` is the CLI version.

## Lab

### 1. Add repositories

`podinfo` is a tiny demo web app maintained by the Flux project — a perfect
practice chart. Add the prometheus-community repo now too; Phase 5 uses it.

```sh
helm repo add podinfo https://stefanprodan.github.io/podinfo
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm search repo podinfo
helm search repo prometheus-community/kube-prometheus-stack
```

`helm search repo <name> -l` lists all chart versions, not just the latest.

### 2. Inspect before installing

```sh
helm show chart podinfo/podinfo          # metadata: version, appVersion, sources
helm show values podinfo/podinfo | less  # the full settings surface
helm template podinfo podinfo/podinfo | less   # what YAML would be created
```

Find these keys in the values output: `replicaCount`, `ui.color`, `ui.message`.
Those are your knobs for today.

### 3. Install with a values file

Your core artifact: a values file that sets **2 replicas**, a **UI color** of
`#3498db`, and a **message** identifying revision 1. Write it yourself from
what `helm show values` told you, then compare.

<details><summary>Solution</summary>

`podinfo-values.yaml` (keep it in this day's folder — it's lab scratch, not
platform config):

```yaml
replicaCount: 2
ui:
  color: "#3498db"
  message: "helm revision 1"
```

</details>

```sh
helm install podinfo podinfo/podinfo \
  -n helm-lab --create-namespace \
  -f podinfo-values.yaml
```

Read the NOTES that print after install — charts use them to tell you what to
do next.

### 4. Explore the release

```sh
helm list -n helm-lab
helm status podinfo -n helm-lab
helm get values podinfo -n helm-lab          # only YOUR overrides
helm get values podinfo -n helm-lab --all    # merged with defaults
helm get manifest podinfo -n helm-lab | head -40
kubectl get secret -n helm-lab -l owner=helm  # release state lives here
```

Open k9s, namespace `helm-lab`: two podinfo pods, a Service, a Deployment —
all created by Helm, all normal objects.

### 5. Prove the values landed

```sh
kubectl -n helm-lab port-forward svc/podinfo 9898:9898 &
curl -s localhost:9898 | grep -E 'color|message'
```

You should see `"color": "#3498db"` and your message. (Open
http://localhost:9898 in a browser if you want to *see* the color.)

### 6. Install the diff plugin, then upgrade

```sh
helm plugin install https://github.com/databus23/helm-diff
```

Change the color in `podinfo-values.yaml` to `#2ecc71` and the message to
`helm revision 2`. **Diff before you touch anything:**

```sh
helm diff upgrade podinfo podinfo/podinfo -n helm-lab -f podinfo-values.yaml
```

Read the diff: only the Deployment's env changes. Now apply it, and try a
`--set` on top to see precedence:

```sh
helm upgrade podinfo podinfo/podinfo -n helm-lab -f podinfo-values.yaml
helm list -n helm-lab        # REVISION is now 2
curl -s localhost:9898 | grep color   # new color served

helm upgrade podinfo podinfo/podinfo -n helm-lab \
  -f podinfo-values.yaml --set replicaCount=3
kubectl get pods -n helm-lab           # 3 pods; --set beat the file
```

### 7. History and rollback

```sh
helm history podinfo -n helm-lab
helm rollback podinfo 1 -n helm-lab
helm history podinfo -n helm-lab      # revision 4: "Rollback to 1"
curl -s localhost:9898 | grep color   # original #3498db is back
kubectl get pods -n helm-lab          # and 2 replicas again
```

Note rollback created revision 4 — history only moves forward.

### 8. Dry-run vs template

```sh
helm template podinfo podinfo/podinfo -f podinfo-values.yaml > /tmp/rendered.yaml
helm upgrade podinfo podinfo/podinfo -n helm-lab -f podinfo-values.yaml \
  --dry-run=server | head -30
```

Same output shape, different trust level: `template` never asked the cluster
anything; `--dry-run=server` validated against your live API server.

### 9. Uninstall (keeping history)

```sh
helm uninstall podinfo -n helm-lab --keep-history
helm list -n helm-lab -a              # status: uninstalled
helm history podinfo -n helm-lab      # trail preserved
helm rollback podinfo 1 -n helm-lab   # resurrect it!
helm uninstall podinfo -n helm-lab    # now for real
```

## Verify ✅

- [ ] `helm repo list` shows `podinfo` and `prometheus-community`
- [ ] `helm history podinfo -n helm-lab` showed ≥4 revisions before final uninstall, including a `Rollback to 1` line
- [ ] `curl -s localhost:9898 | grep color` returned `#2ecc71` after the upgrade and `#3498db` after the rollback
- [ ] `kubectl get secret -n helm-lab -l owner=helm` listed one `sh.helm.release.v1.podinfo.vN` Secret per revision
- [ ] `helm diff upgrade ...` printed a colored diff touching only the env vars
- [ ] `helm plugin list` shows `diff`

## CKA corner 🎓

Helm operations are in the CKA "Workloads & Scheduling / tooling" domain. Drill
this loop until it's under 5 minutes, from memory:

```sh
helm repo add bitnami-labs https://bitnami-labs.github.io/sealed-secrets   # any repo works
helm repo update
helm search repo sealed-secrets -l | head -5        # find available versions
helm install ss bitnami-labs/sealed-secrets -n drill --create-namespace --version <pick-one>
helm upgrade ss bitnami-labs/sealed-secrets -n drill --set fullnameOverride=ss-drill
helm history ss -n drill
helm rollback ss 1 -n drill
helm uninstall ss -n drill && kubectl delete ns drill
```

Exam tips: `helm --help` is allowed and good; `-n` is **not** sticky — every
helm command needs it; `helm template ... > x.yaml` is a fast way to generate
YAML you then edit by hand.

## Stretch goals

- Re-install podinfo with `ingress.enabled=true` and a host of
  `podinfo.localhost` (find the right values keys with `helm show values`),
  then `curl http://podinfo.localhost:8080` — no port-forward needed thanks to
  your kind port mappings from Day 5.
- Run `kubectl get secret sh.helm.release.v1.podinfo.v1 -n helm-lab -o jsonpath='{.data.release}' | base64 -d | base64 -d | gzip -d | jq keys` (before the final uninstall) — see exactly what Helm stores.
- Skim `helm get hooks` output for podinfo's tests — Day 21 covers hooks.

## Cleanup

```sh
helm uninstall podinfo -n helm-lab 2>/dev/null || true
```

**Keep:**
- The `helm-lab` namespace — Day 20 installs your own chart there
- Both Helm repos (`podinfo`, `prometheus-community`) — Phase 5 needs the latter
- The `diff` plugin — you'll use it for the rest of the course
- The `guestbook` namespace untouched, as always
