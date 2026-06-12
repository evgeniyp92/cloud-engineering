# Day 27 — ApplicationSets: Stamping Out Environments

> **Time:** ~3 h · **Builds on:** Days 22, 26

## Objectives

- Recognize the **N×M problem** (apps × environments/clusters) and explain why neither copy-pasted Applications nor app-of-apps alone solves it.
- Deploy one **ApplicationSet** that generates dev/stage/prod Applications from your Day 22 Kustomize overlays.
- Swap the list generator for a **git directory generator** and watch a new environment appear by `mkdir` alone.
- Prove the fan-out economics: one base change updates every env; one overlay change touches one env.

## Concepts

### The N×M problem

Yesterday's `argocd/apps/` directory works at platform scale: one file per component, dozens of components, fine. Now scale the other axis. Your podlab has three environments (Day 22 built `overlays/dev|stage|prod`). Three Application files, ~95% identical — repoURL, chart of the spec, syncPolicy all the same; only `path` and `namespace` vary. Tolerable. Now imagine 20 services × 4 envs × 3 clusters: **240 nearly-identical YAML files**, and adding a syncOption to "all of them" is a 240-file PR that someone will get wrong on file 173.

The pattern is obvious: these Applications are *one template times a parameter list*. That's exactly what the **ApplicationSet** CRD is — a generator (produces parameter sets) plus a template (an Application spec with holes). The applicationset-controller (it's been sitting in your `argocd` namespace since Day 24) renders generator × template into real Application resources and keeps them in lockstep: edit the template once, all generated apps update; a generator stops emitting a parameter set, its app is deleted.

### Generators — where the parameters come from

| Generator | Emits one parameter set per… | Killer use |
|---|---|---|
| `list` | hard-coded element | small fixed env lists, per-env knobs (today) |
| `git` (directories) | directory matching a glob in a repo | **zero-maintenance envs**: new overlay dir = new app (today) |
| `git` (files) | config file (its JSON/YAML keys become params) | per-env metadata richer than a dir name |
| `cluster` | cluster registered in ArgoCD | "deploy the agent to every cluster we ever add" |
| `matrix` | cross-product of two generators | services × clusters = the full N×M, one CRD |

This is the mental model for the interview question at the end: generators turn "deploy everything, everywhere" from a YAML-farming chore into a declaration.

### Template syntax

Two modes. Legacy `{{env}}` flat substitution, and `goTemplate: true` which switches to real Go templates — `{{.env}}`, `{{.path.basename}}`, conditionals, `default`, sprig functions. Use go-template mode for anything new; with `goTemplateOptions: ["missingkey=error"]` a typo'd parameter fails loudly at render time instead of generating an Application named `podlab-<no value>`.

### ApplicationSet vs app-of-apps — rivals or layers?

Neither replaces the other:

- **app-of-apps** = *static* children, each hand-written, maximally flexible per child. Right for your platform components (metrics-server's spec shares nothing with guestbook's).
- **ApplicationSet** = *generated* children, uniform by construction. Right for the same thing repeated with parameters.

And they compose: an ApplicationSet is just a Kubernetes manifest, so it can live in `argocd/apps/` where root deploys it. Root manages the AppSet; the AppSet manages the env apps. That's exactly what you'll build — the bootstrap story from Day 26 (one `kubectl apply -f root.yaml`) now transitively includes every environment.

One behavioral difference to respect: generated Applications are **owned** by the ApplicationSet. Edit one by hand and the controller reverts it (selfHeal one level up); delete the AppSet and the generated apps go with it. The template *is* the source of truth.

### Rolling changes across environments

Automated sync on all envs means a bad base change hits prod at the same minute it hits dev. Mitigations, in increasing rigor:

1. **Per-env parameters as gates** — the list generator can carry more than a name: give prod `autosync: "false"` and template the syncPolicy conditionally. Prod apps then queue OutOfSync until a human syncs — a manual promotion gate.
2. **Track different revisions per env** — dev follows `main`, prod follows a `prod` tag/branch; promotion = moving the tag. Heavier process, very auditable.
3. **Progressive syncs** — the AppSet `strategy: RollingSync` field (alpha, gated behind a controller flag) syncs generated apps in labeled steps: dev, then stage, then prod, halting on failure. Know it exists; don't build on alpha today.

You'll wire option 1 as a snippet; Days 38–39 attack the deeper problem (progressive delivery *within* an env) with Argo Rollouts.

## Lab

### 0. Pre-flight

Day 22 left `kustomize/podlab/overlays/{dev,stage,prod}` in the repo, each setting its namespace (`podlab-dev` etc.), per-env `COLOR`, and a `configMapGenerator` with hash suffixes. Confirm they still build:

```sh
cd ~/Code/k8s-gitops
kubectl kustomize kustomize/podlab/overlays/dev | head -20
```

If the Day 22 cleanup left old copies of these resources running, don't bother deleting them — ArgoCD is about to adopt anything identical and correct anything that drifted. Free adoption practice.

### 1. The ApplicationSet — list generator

Create `argocd/apps/podlab-envs.yaml` (yes, in root's directory — root will deploy it; an ApplicationSet is just another manifest). Requirements:

- `kind: ApplicationSet`, name `podlab-envs`, ns `argocd`, `goTemplate: true`, `goTemplateOptions: ["missingkey=error"]`
- a `list` generator with elements `env: dev`, `env: stage`, `env: prod`
- template: Application named `podlab-{{.env}}`, source path `kustomize/podlab/overlays/{{.env}}`, destination namespace `podlab-{{.env}}`, finalizer on (env teardown should be total)
- syncPolicy: automated + selfHeal + prune, `CreateNamespace=true`

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: podlab-envs
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - list:
        elements:
          - env: dev
          - env: stage
          - env: prod
  template:
    metadata:
      name: 'podlab-{{.env}}'
      finalizers:
        - resources-finalizer.argocd.argoproj.io
    spec:
      project: default
      source:
        repoURL: https://github.com/<you>/k8s-gitops
        targetRevision: main
        path: 'kustomize/podlab/overlays/{{.env}}'
      destination:
        server: https://kubernetes.default.svc
        namespace: 'podlab-{{.env}}'
      syncPolicy:
        automated:
          selfHeal: true
          prune: true
        syncOptions:
          - CreateNamespace=true
```

</details>

### 2. Push and watch three apps materialize

```sh
git add argocd/apps/podlab-envs.yaml && git commit -m "podlab-envs ApplicationSet" && git push
argocd app get root --refresh         # root picks up the new file → deploys the AppSet
argocd app list | grep podlab-
```

Three new Applications — `podlab-dev`, `podlab-stage`, `podlab-prod` — created by the controller, syncing themselves. In the UI, note they're badged as generated by an ApplicationSet, and find the chain: `root` → `podlab-envs` (AppSet) → three apps. Try to vandalize one: `kubectl patch application podlab-dev -n argocd --type merge -p '{"spec":{"source":{"targetRevision":"nonsense"}}}'` — watch the controller put it back. The template owns them.

### 3. Prove env isolation

Each overlay sets its own `COLOR` and generates its own config files. Check all three (port-forward, or your overlays' ingress hosts if you wired them on Day 22):

```sh
for env in dev stage prod; do
  kubectl -n podlab-$env port-forward deploy/podlab 9999:8080 >/dev/null 2>&1 &
  PF=$!; sleep 2
  echo "== $env: $(curl -s localhost:9999/ | python3 -c 'import json,sys; print(json.load(sys.stdin)["color"])')"
  kill $PF; wait $PF 2>/dev/null
done
```

Three namespaces, three colors, three hash-suffixed ConfigMaps — one ApplicationSet, ~40 lines.

### 4. THE payoff: base vs overlay economics

**Fan-out** — change something in `kustomize/podlab/base` (e.g. a literal in the base `configMapGenerator`, or the base replica count):

```sh
# edit base, then:
git add -A && git commit -m "base: bump shared config" && git push
while sleep 5; do argocd app list | grep podlab-; echo ---; done   # Ctrl-C when all Synced
```

All three envs go OutOfSync → Synced within one reconcile. The configMapGenerator hash changes, so each env's deployment rolls — confirm the new value in each env's `/config`. One commit, every environment, no per-env work.

**Surgical strike** — change only `overlays/prod` (e.g. prod's `COLOR` patch), push, and watch: `podlab-prod` syncs, dev and stage don't so much as flicker. This asymmetry — shared by default, divergent by exception, blast radius visible in the diff — is the entire argument for the base/overlay structure you built on Day 22.

### 5. The git directory generator — zero-maintenance environments

The list generator still has a maintenance loop: new env = edit the AppSet. Erase it. Replace the generator in `podlab-envs.yaml`:

```yaml
  generators:
    - git:
        repoURL: https://github.com/<you>/k8s-gitops
        revision: main
        directories:
          - path: kustomize/podlab/overlays/*
```

…and in the template, replace every `{{.env}}` with `{{.path.basename}}` (the directory generator emits path parameters, not your custom keys). Commit, push. Root updates the AppSet; the same three apps keep running (names unchanged — `podlab-dev` is `podlab-dev` under either generator, so this swap is seamless).

Now the magic. Create a `qa` overlay — copy dev and adjust (namespace `podlab-qa`, its own COLOR, mirror whatever your dev overlay contains):

```sh
cd ~/Code/k8s-gitops/kustomize/podlab
cp -r overlays/dev overlays/qa
# edit overlays/qa/kustomization.yaml: namespace podlab-qa, COLOR=orange, etc.
git add overlays/qa && git commit -m "qa environment" && git push
```

Within a reconcile: `podlab-qa` exists, namespace created, app Synced — **and you never touched the ApplicationSet**. `mkdir` is now your environment-provisioning API. The reverse holds too: `git rm -r overlays/qa` would delete the app (and, via the finalizer, the namespace's workload). Leave qa running for now — Verify wants to see it.

### 6. The prod gate (snippet, wire if time allows)

Directory generator can't carry per-env flags, but the list generator can — which is the real trade-off between them. The manual-prod-gate version:

```yaml
  generators:
    - list:
        elements:
          - env: dev
            autosync: "true"
          - env: stage
            autosync: "true"
          - env: prod
            autosync: "false"
  template:
    # ...
    spec:
      syncPolicy:
        {{- if eq .autosync "true" }}
        automated: {selfHeal: true, prune: true}
        {{- end }}
        syncOptions: [CreateNamespace=true]
```

Prod then accumulates OutOfSync on every push until someone runs `argocd app sync podlab-prod` — a promotion gate expressed in two lines of template. (A git **file** generator gets you both worlds: per-env config files in the overlay dirs, still zero AppSet edits per env.)

## Verify ✅

- [ ] `argocd app list | grep -c podlab-` → `4` (dev, stage, prod, qa) — all from one ApplicationSet
- [ ] `kubectl get applicationset podlab-envs -n argocd -o jsonpath='{.spec.generators[0]}'` → shows the `git` generator, proving qa appeared with **no** AppSet edit
- [ ] `git -C ~/Code/k8s-gitops log --oneline -1 -- argocd/apps/podlab-envs.yaml` predates the qa commit
- [ ] Each of the four namespaces returns a different `color` from `/`, and `/config` shows that env's own config file content
- [ ] After the base change in step 4: all envs' `/config` show the new shared value; after the prod-only change, `git log` shows dev/stage apps' last-synced revision can differ from what changed (only `podlab-prod` rolled — check pod ages: `kubectl get pods -n podlab-stage` vs `-n podlab-prod`)
- [ ] `kubectl patch` a generated app's spec → the controller reverts it within seconds

## Interview corner 💬

**"200 microservices, 3 environments, 4 clusters — how do you manage the ArgoCD side?"** Not with 2,400 Application files. One ApplicationSet with a **matrix generator**: a git directory generator discovering services (one dir per service) crossed with a cluster generator (or a list of envs) — the cross-product templates every Application, and onboarding service #201 is `mkdir` plus a manifest directory, zero ArgoCD config. Per-env divergence rides on Kustomize overlays or values files inside each service's directory; promotion gates via per-env autosync flags or tracked revisions. The follow-up worth volunteering: guard the blast radius — `missingkey=error`, a non-prod cluster for AppSet changes first, and progressive-sync ordering, because a template typo now edits 2,400 apps at once.

**"What breaks if someone hand-edits a generated Application?"** Nothing, briefly — then the applicationset-controller reconciles it back to the template. The fix-it-here layer is the generator's inputs (the element, the directory, the config file) or the template, in git. Same selfHeal story as Day 25, one meta-level up.

## Stretch goals

- Convert to a **git file generator**: drop a `config.yaml` (`env: prod`, `autosync: false`, `colorOverride: …`) in each overlay dir, generate from `kustomize/podlab/overlays/**/config.yaml`, and use the file's keys in the template — zero-maintenance *and* per-env knobs.
- Build a **matrix** toy: list generator (`team: a`, `team: b`) × the directory generator, and inspect the cross-product with `kubectl get applicationset podlab-envs -n argocd -o yaml | grep -A2 'status:'` before it generates anything real.
- Register the cluster under a second name (`argocd cluster add`) and try a **cluster generator** — the multi-cluster story without a second cluster.
- Read about `preservedFields` and `applicationsSync: create-only` on ApplicationSets — the knobs for teams that want generation without full ownership.

## Cleanup

Delete the qa environment the GitOps way and watch the cascade:

```sh
cd ~/Code/k8s-gitops
git rm -r kustomize/podlab/overlays/qa
git commit -m "decommission qa" && git push
# next reconcile: podlab-qa app deleted → finalizer cascades → namespace contents gone
kubectl delete ns podlab-qa --ignore-not-found   # the namespace shell itself
```

**Keep:** the `podlab-envs` ApplicationSet and the dev/stage/prod apps — Day 28 seals a secret into this estate, and the capstone replays all of it. ArgoCD stays, as always.
