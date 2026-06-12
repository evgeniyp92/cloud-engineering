# Day 23 — Manifest Quality: a CI-Grade Lint Gate

> **Time:** ~3 h · **Builds on:** Days 20, 21, 22

## Objectives

- Layer five different quality checks and explain what each catches that the others miss
- Validate rendered Helm and Kustomize output against real Kubernetes schemas with `kubeconform`
- Run `kube-score` against your podlab chart and **fix the legitimate findings** in the chart
- Wire it all into `make lint` + a git pre-commit hook — the quality gate your CI will run in Phase 7

## Concepts

### Shift left, or debug in production

Every YAML mistake gets caught somewhere. The only question is *where*:

```
editor → lint → schema validation → opinion checks → dry-run → admission → runtime → 3am page
└──────────────── cheap, fast, private ───────────────┘ └──── expensive, slow, public ────┘
```

You've already met the expensive end: Day 17's troubleshooting gauntlet was
runtime-stage debugging of mistakes a 50 ms linter would have caught. Today you
build the cheap end — and because your manifests now live in a Git repo
(`k8s-gitops`) that ArgoCD will deploy *automatically* in Phase 4, pre-merge
checking stops being nice-to-have: **once GitOps is on, merging IS deploying.**

### The layers, and why one tool isn't enough

| Layer | Tool | Catches | Misses |
|---|---|---|---|
| 1. Template sanity | `helm lint`, `helm template` | broken templates, bad chart structure, unparseable YAML | anything about Kubernetes itself |
| 2. Schema validation | `kubeconform` | wrong/unknown `apiVersion`/`kind`, misspelled fields, wrong types | fields that are valid but dumb |
| 3. Opinion linting | `kube-score` | valid-but-dumb: no probes, no resource limits, no NetworkPolicy, mutable image tags | your business logic |
| 4. Unit/chart tests | `helm-unittest`, `ct` | regressions in *your* template logic as the chart evolves | runtime behavior |
| 5. Deprecation scan | `pluto` | APIs that vanish in the next cluster upgrade | everything else |

`kubectl apply --dry-run=server` overlaps with layer 2 with perfect accuracy
(it asks the real API server) — but it needs a live cluster, which CI usually
doesn't have. `kubeconform` validates against published JSON schemas offline
and fast; that's the trade. The two are complements: kubeconform in CI,
server dry-run before a risky apply.

**The CRD gap:** kubeconform's default schemas cover only built-in APIs. Your
cluster is full of CRDs (Cilium today; ArgoCD, Prometheus, Rollouts soon), and
their manifests would fail with "schema not found". The community fix is the
[datreeio CRDs-catalog](https://github.com/datreeio/CRDs-catalog) — pre-converted
JSON schemas for hundreds of CRDs, plugged in via a second `-schema-location`
with a URL template. You'll wire it in today so the gate doesn't fall over the
moment Phase 4 adds ArgoCD `Application` manifests to the repo.

### Opinion linting is a conversation, not a verdict

`kube-score` encodes SRE opinions: every pod should have probes, limits, a
NetworkPolicy, an immutable image tag. Some findings are *legit and you fix
them* (today: securityContext, NetworkPolicy). Some are *wrong for your
context and you suppress them with a documented reason* (today:
`ImagePullPolicy: Always` — correct for clouds, actively harmful on kind where
images are side-loaded). A team that blindly fixes every finding ships
cargo-cult YAML; a team that ignores the tool ships 3am pages. The skill is
the triage.

### Testing templates like code

Charts are code; code regresses. [helm-unittest](https://github.com/helm-unittest/helm-unittest)
renders templates with given values and asserts on the output — pure
client-side, milliseconds, perfect for "does `image.tag` still land where I
think it does after I refactor `_helpers.tpl`?". For chart *repositories*,
[chart-testing](https://github.com/helm/chart-testing) (`ct lint`, `ct install`)
is the standard CI harness: it detects which charts changed against a target
branch, lints them (version-bump enforcement, maintainers, values schema) and
optionally install-tests them in a throwaway kind cluster. You'll meet `ct` in
the wild in every serious chart repo; today it's an overview, your Makefile is
the lab.

## Lab

### 1. Install the toolchain

```sh
brew install kubeconform kube-score fairwindsops/tap/pluto
helm plugin install https://github.com/helm-unittest/helm-unittest
```

### 2. Layer 1+2: lint and schema-validate everything

All commands run from `~/Code/k8s-gitops`:

```sh
helm lint charts/podlab charts/guestbook
helm dependency build charts/guestbook    # ensure the dep tarball exists locally

helm template podlab-dev charts/podlab | kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

kubectl kustomize kustomize/podlab/overlays/prod | kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
```

The `{{.Group}}/{{.ResourceKind}}...` URL is a kubeconform template — it builds
a schema URL per resource kind and falls back across locations in order. Both
runs should end `Invalid: 0, Errors: 0`.

### 3. Layer 3: kube-score, then actually improve the chart

```sh
helm template podlab-dev charts/podlab | kube-score score -
```

Read every finding. Expected triage for your Day 20 chart:

| Finding | Verdict |
|---|---|
| Container has no `securityContext` (readonly rootfs, runAsNonRoot, …) | **Fix** — podlab is distroless and already runs as nonroot; declare it |
| Pod has no matching NetworkPolicy | **Fix** — add one to the chart (you know netpol from Day 15) |
| `ImagePullPolicy` is not `Always` | **Suppress with reason** — images are `kind load`-ed |
| Probes: readiness identical to liveness | **Accept** — podlab exposes one health endpoint by design; note that guestbook does this right (`/readyz` vs `/healthz`) |
| Resources/probes missing | should NOT fire — Day 20 did this right; if it fires, fix your chart |

Now make the fixes in `charts/podlab` (this is the improvement loop — the
linter found real gaps in yesterday's "finished" chart). Requirements: a
hardened container `securityContext`, and a `templates/networkpolicy.yaml`
gated by `networkPolicy.enabled` (default `true`) allowing ingress to the pods
on 8080 from anywhere in-cluster.

<details><summary>Solution</summary>

In `deployment.yaml`, inside the container spec:

```yaml
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532        # distroless "nonroot"
            runAsGroup: 65532
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
```

`charts/podlab/templates/networkpolicy.yaml`:

```yaml
{{- if .Values.networkPolicy.enabled }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "podlab.fullname" . }}
  labels:
    {{- include "podlab.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "podlab.selectorLabels" . | nindent 6 }}
  policyTypes: ["Ingress"]
  ingress:
    - from:
        - namespaceSelector: {}
      ports:
        - port: 8080
{{- end }}
```

In `values.yaml`:

```yaml
networkPolicy:
  enabled: true
```

Bump `version:` in `Chart.yaml` to `0.2.0` — templates changed.

</details>

Re-score with the documented suppression:

```sh
helm template podlab-dev charts/podlab | kube-score score - \
  --ignore-test container-image-pull-policy
```

Far cleaner. If you still have the `podlab-dev` release from Day 20, upgrade it
(`helm diff` first!) and confirm `/healthz` still answers — hardening that
breaks the app isn't hardening. (`kube-score score --help` lists test names for
`--ignore-test`; in-manifest alternative: a `kube-score/ignore` annotation.)

### 4. Layer 4: unit tests for the chart

Create `charts/podlab/tests/deployment_test.yaml` with two tests: (1) setting
`image.tag` changes the rendered image; (2) setting `color` lands in the
`COLOR` env var.

<details><summary>Solution</summary>

```yaml
suite: deployment
templates:
  - deployment.yaml
tests:
  - it: renders the image from values
    set:
      image.tag: v9
    asserts:
      - equal:
          path: spec.template.spec.containers[0].image
          value: podlab:v9
  - it: puts color into the COLOR env var
    set:
      color: purple
    asserts:
      - contains:
          path: spec.template.spec.containers[0].env
          content:
            name: COLOR
            value: purple
```

</details>

```sh
helm unittest charts/podlab    # Passed: 2
```

`ct` overview, for when you maintain a charts repo: `ct lint` would run
`helm lint` + version-bump checks + values-schema validation on every chart
that changed vs `main` — config lives in a `ct.yaml`. Overkill for one repo and
two charts; remember it exists.

### 5. Layer 5: deprecation scan (mention-grade)

```sh
pluto detect-files -d kustomize/
```

Silence is success: nothing you wrote uses an API deprecated in upcoming
Kubernetes versions. Pluto earns its keep before cluster upgrades (Day 47
territory) — `pluto detect-helm -A` scans live releases the same way.

### 6. Wire it together: the gate

Core artifact: a `Makefile` at the **root of `~/Code/k8s-gitops`** with targets
`lint-helm`, `lint-kustomize`, `score`, `test`, and an umbrella `lint` running
all four — plus a git pre-commit hook that runs `make lint`. This exact target
is what GitHub Actions will run in Phase 7.

<details><summary>Solution</summary>

`~/Code/k8s-gitops/Makefile` (recipe lines start with a **tab**):

```make
SCHEMAS := -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
KUBECONFORM := kubeconform -strict -summary $(SCHEMAS)
OVERLAYS := dev stage prod

.PHONY: lint lint-helm lint-kustomize score test

lint: lint-helm lint-kustomize score test
	@echo "✓ all gates passed"

lint-helm:
	helm lint charts/podlab charts/guestbook
	helm dependency build charts/guestbook
	helm template podlab-dev charts/podlab | $(KUBECONFORM)
	helm template gb charts/guestbook | $(KUBECONFORM)

lint-kustomize:
	for o in $(OVERLAYS); do \
	  kubectl kustomize kustomize/podlab/overlays/$$o | $(KUBECONFORM) || exit 1; \
	done

score:
	helm template podlab-dev charts/podlab | \
	  kube-score score - --ignore-test container-image-pull-policy

test:
	helm unittest charts/podlab
```

`.git/hooks/pre-commit`:

```sh
#!/bin/sh
make lint
```

```sh
chmod +x .git/hooks/pre-commit
```

(Hooks aren't committed with the repo; the shareable version is the
[pre-commit](https://pre-commit.com) framework with a `.pre-commit-config.yaml`
calling `make lint` — same idea, installable by teammates with `pre-commit
install`. Either is fine here.)

</details>

```sh
make lint   # must pass clean
```

### 7. The sabotage drill

Prove you know what each layer actually sees. Edit
`kustomize/podlab/base/deployment.yaml` and set
`apiVersion: apps/v1beta1` (a Deployment API removed back in 1.16). Run the
layers one by one and record what happens:

```sh
kubectl kustomize kustomize/podlab/overlays/dev | head -5        # renders happily
make lint                                                        # which target fails?
kubectl apply -k kustomize/podlab/overlays/dev --dry-run=server  # server's opinion
pluto detect-files -d kustomize/
git checkout -- kustomize/podlab/base/deployment.yaml            # undo the crime
```

Do the same once in `charts/podlab/templates/deployment.yaml` and run
`helm lint charts/podlab` — note that it **passes**. Fill in your own table and
compare:

| Layer | apps/v1beta1 Deployment | Why |
|---|---|---|
| `helm lint` / `kubectl kustomize` | ✗ passes | syntax-only; knows no Kubernetes APIs |
| `kubeconform` (via `make lint`) | ✓ fails — no schema found for `apps/v1beta1` | schema catalog has no such API |
| `kube-score` | ✓ errors (unparseable object) | can't map it to a known kind |
| `--dry-run=server` | ✓ fails — `no matches for kind "Deployment" in version "apps/v1beta1"` | the API server is the ground truth |
| `pluto detect-files` | ✓ flags it as removed in 1.16 | that's its one job |

One layer passing means nothing; the *stack* is the gate. Revert both edits
before committing.

### 8. Commit

```sh
cd ~/Code/k8s-gitops
git add Makefile charts/podlab
git commit -m "quality gate: make lint (kubeconform, kube-score, unittest) + chart hardening"
# the pre-commit hook just ran your whole gate — that's the point
```

## Verify ✅

- [ ] `make lint` exits 0 and prints `✓ all gates passed`
- [ ] `helm unittest charts/podlab` → `Passed: 2`
- [ ] `helm template podlab-dev charts/podlab | kube-score score - --ignore-test container-image-pull-policy` shows no CRITICAL for securityContext or NetworkPolicy
- [ ] Sabotage drill: kubeconform and server dry-run both rejected `apps/v1beta1`; `helm lint` did not — and you can say why
- [ ] A commit attempt with the sabotaged file in place is blocked by the pre-commit hook
- [ ] `git log --oneline` shows the gate commit; `git status` is clean

## Stretch goals

- Add a `values.schema.json` to `charts/podlab` (JSON Schema for values) — `helm install` then rejects typo'd values like `replicaCont: 3` *before* templating. The fourth gate nobody uses and everybody should.
- Run `helm template gb charts/guestbook | kube-score score -` and triage the Bitnami subchart's findings — someone else's opinions about someone else's chart; decide what's actionable.
- Write a third unit test asserting the NetworkPolicy is absent when `networkPolicy.enabled=false` (`templates: [networkpolicy.yaml]` + the `hasDocuments` assert with `count: 0`).
- Install `ct` (`brew install chart-testing`) and get `ct lint --charts charts/podlab` passing — it will demand a chart version bump discipline you'll appreciate later.

## Cleanup

Nothing ran in the cluster today — the entire gate is client-side (that's the
point). **Keep everything:** the Makefile, the hook, the hardened chart 0.2.0,
the unit tests. Phase 4 (Days 24–29) pushes this repo to GitHub and ArgoCD
starts deploying it; Phase 7 (Day 45) lifts `make lint` straight into GitHub
Actions. Your gate is now load-bearing.
