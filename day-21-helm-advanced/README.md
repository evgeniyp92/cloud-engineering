# Day 21 — Helm Advanced: Dependencies, Hooks, Tests, OCI

> **Time:** ~4 h · **Builds on:** Days 11, 19, 20

## Objectives

- Build a `guestbook` chart that pulls in Postgres as a **chart dependency**, with values passthrough, `condition`, and `alias` understood
- Use lifecycle **hooks** (a pre-install/pre-upgrade Job) and know their ordering pitfalls
- Ship smoke tests inside the chart and run them with `helm test`
- Package a chart and push/install it via an **OCI registry** (ttl.sh)

## Concepts

### Two ways to get a database into your chart

Your guestbook API needs Postgres. As chart author you have two options:

| | Own minimal template | Community chart as dependency |
|---|---|---|
| You write | StatefulSet + Service + Secret yourself (you did this raw on Day 11) | ~10 lines of `Chart.yaml` + values |
| You get | Exactly what you wrote, nothing more | Replication, metrics, backups, init scripts… for free |
| You risk | Reinventing wheels, missing edge cases | Upstream churn, a values surface bigger than your app |
| Upgrades | Your problem | `helm dependency update` + read the changelog |

Today you implement the **dependency route** — it's what real teams do for
commodity infrastructure, and the failure modes are instructive (see below).
The DIY route you already know from Day 11.

### How dependencies work

Dependencies are declared in `Chart.yaml`:

```yaml
dependencies:
  - name: postgresql
    version: "16.7.27"
    repository: oci://registry-1.docker.io/bitnamicharts
    condition: postgresql.enabled
```

`helm dependency update` resolves them, writes a `Chart.lock` (commit it — it's
your reproducibility guarantee, like `go.sum`), and vendors the packaged chart
into `charts/` (gitignore the `.tgz` files; `helm dependency build` re-fetches
from the lock). At install time the subchart renders as part of *your* release.

**Values passthrough:** anything under a top-level key matching the dependency
name flows into the subchart as *its* `.Values` — `postgresql.auth.username` in
your values becomes `auth.username` inside the subchart. Two extras:

- `global:` — visible to parent **and** all subcharts; for cross-cutting values.
- `alias:` — install the same chart twice (`alias: primarydb`, `alias: replicadb`),
  each configured under its alias key.
- `condition:` — a values path; if false, the subchart doesn't render at all.
  Convention: `<name>.enabled`. (`tags:` does the same for groups of deps.)

### A real-world supply-chain lesson: Bitnami

For a decade "just use the Bitnami chart" was the default answer. In **August
2025 Broadcom froze the public Bitnami catalog**: charts remain available as
OCI artifacts at `oci://registry-1.docker.io/bitnamicharts` but no longer get
updates, and most container images moved to the unsupported
`docker.io/bitnamilegacy` namespace. Result: the chart installs, but its
default image reference 404s unless you override it — and recent chart
versions additionally refuse substituted images unless you set
`global.security.allowInsecureImages: true`.

We use it anyway, deliberately: pinning a dependency, overriding subchart
values, and absorbing upstream churn without touching templates **is the whole
skill**. (Production answer in 2026: an operator like CloudNativePG — Day 46 —
or a maintained fork.)

### Hooks

Annotate any template and Helm runs it at a lifecycle point instead of with
the regular resources:

```yaml
annotations:
  "helm.sh/hook": pre-install,pre-upgrade
  "helm.sh/hook-weight": "0"            # lower runs first
  "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
```

Classic use: a DB migration Job that must finish before the new app version
starts. **The ordering pitfall:** `pre-install` hooks run *before* your regular
resources exist — on a fresh install the database your migration wants to reach
**isn't there yet**. Real charts handle this (skip when absent, init containers,
or post-install hooks); today's lab makes you hit it on purpose. Delete
policies matter too: without one, completed hook Jobs pile up forever;
`before-hook-creation` is the usual choice. Foreshadow: ArgoCD (Phase 4)
translates Helm hooks into its own sync-phase hooks, so this mental model
transfers directly.

`helm test` is just another hook (`"helm.sh/hook": test`): a pod that exits 0
on success, shipped under `templates/tests/`, run on demand against a live
release — the cheapest smoke test you'll ever write.

### Charts as OCI artifacts

Chart repos (Day 19) are static HTTP indexes. The modern alternative: store
charts in any **OCI registry**, same as images — `helm push` / `helm install
oci://...`. One registry, one auth system, one replication story for images
*and* charts; ArgoCD and Flux consume OCI charts natively. For the lab we use
**ttl.sh**, an anonymous ephemeral registry (uploads live up to 24 h) — real
OCI mechanics, zero accounts. Mention for later: `helm package --sign` creates
a `.prov` provenance file verifiable with `helm verify` — table stakes for
publishing public charts.

### Library charts (one paragraph)

A chart with `type: library` exports *only* named templates — no installable
resources. Teams with 30 microservices write one `common` library chart
(deployment/service/labels helpers), and each app chart becomes ~20 lines that
`include` them. You'll recognize the idea from `_helpers.tpl`; a library chart
is `_helpers.tpl` promoted to a versioned dependency:

```yaml
# app's Chart.yaml
dependencies: [{name: common, version: 1.x.x, repository: oci://...}]
# app's templates/deployment.yaml
{{ include "common.deployment" . }}
```

## Lab

### 1. Scaffold guestbook and declare the dependency

```sh
cd ~/Code/k8s-gitops
helm create charts/guestbook
rm -rf charts/guestbook/templates/* charts/guestbook/charts charts/guestbook/values.yaml
```

Discover the latest available Postgres chart version, then pin it:

```sh
helm show chart oci://registry-1.docker.io/bitnamicharts/postgresql | grep ^version
```

Core artifact part 1 — requirements:

- `Chart.yaml`: dependency on `postgresql` from `oci://registry-1.docker.io/bitnamicharts`, exact pinned version, `condition: postgresql.enabled`
- `values.yaml`: guestbook image/replicas/service.port; `global.security.allowInsecureImages: true`; `postgresql:` block with `enabled: true`, `image.repository: bitnamilegacy/postgresql`, `auth` (user/password/db all `guestbook`), `primary.persistence.size: 1Gi`
- `_helpers.tpl`: same four helpers as Day 20, renamed `guestbook.*`
- `deployment.yaml`: `guestbook:v1`, `DATABASE_URL` assembled with `printf` from `.Release.Name` + the `postgresql.auth.*` values (subchart's Service is named `<release>-postgresql`), readiness `/readyz`, liveness `/healthz`
- `service.yaml`: port from values

<details><summary>Solution</summary>

`charts/guestbook/Chart.yaml`:

```yaml
apiVersion: v2
name: guestbook
description: Guestbook API with a Postgres dependency
type: application
version: 0.1.0
appVersion: "1.0"
dependencies:
  - name: postgresql
    version: "16.7.27"   # pin whatever `helm show chart` reported
    repository: oci://registry-1.docker.io/bitnamicharts
    condition: postgresql.enabled
```

`charts/guestbook/values.yaml`:

```yaml
replicaCount: 1

image:
  repository: guestbook
  tag: v1
  pullPolicy: IfNotPresent

service:
  port: 8080

global:
  security:
    # Required since the 2025 Bitnami freeze: we substitute the (gone)
    # docker.io/bitnami image with the bitnamilegacy mirror below.
    allowInsecureImages: true

# Everything under this key is passed through to the postgresql subchart.
postgresql:
  enabled: true
  image:
    repository: bitnamilegacy/postgresql
  auth:
    username: guestbook
    password: guestbook   # lab only — Day 28 (Sealed Secrets) fixes this properly
    database: guestbook
  primary:
    persistence:
      size: 1Gi
```

`charts/guestbook/templates/_helpers.tpl` — copy Day 20's, replacing `podlab`
with `guestbook` (and `app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}`
since this chart has no `version` value).

`charts/guestbook/templates/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "guestbook.fullname" . }}
  labels:
    {{- include "guestbook.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "guestbook.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "guestbook.selectorLabels" . | nindent 8 }}
    spec:
      containers:
        - name: guestbook
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: DATABASE_URL
              value: {{ printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=disable"
                          .Values.postgresql.auth.username
                          .Values.postgresql.auth.password
                          .Release.Name
                          .Values.postgresql.auth.database | quote }}
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          resources:
            requests:
              cpu: 50m
              memory: 32Mi
            limits:
              cpu: 250m
              memory: 128Mi
```

(Nicer pattern for real life: mount the subchart's generated Secret
`{{ .Release.Name }}-postgresql`, key `password`, via `secretKeyRef` instead of
templating the password into the pod spec. Try it as a stretch goal.)

`charts/guestbook/templates/service.yaml` — same as Day 20's with `guestbook.*`
helpers and `port: {{ .Values.service.port }}`.

</details>

### 2. Resolve the dependency

```sh
cat >> .gitignore <<'EOF'
charts/**/charts/*.tgz
*.tgz
EOF
helm dependency update charts/guestbook
ls charts/guestbook/charts/        # postgresql-16.x.x.tgz vendored
cat charts/guestbook/Chart.lock    # commit this
```

### 3. Add the hook Job

Core artifact part 2: `templates/db-ready-hook.yaml` — a `pre-install,pre-upgrade`
Job (weight `"0"`, delete-policy `before-hook-creation,hook-succeeded`) running
`busybox:1.36` that checks TCP to `<release>-postgresql:5432` with `nc`, prints
"running migrations" if reachable and "fresh install — DB not up yet, skipping"
if not, **always exiting 0** (now you know why: pre-install hooks run before
the DB exists).

<details><summary>Solution</summary>

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "guestbook.fullname" . }}-db-ready
  labels:
    {{- include "guestbook.labels" . | nindent 4 }}
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "0"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: db-check
          image: busybox:1.36
          command: ["sh", "-c"]
          args:
            - |
              if nc -z -w 2 {{ .Release.Name }}-postgresql 5432; then
                echo "database reachable — this is where migrations would run"
              else
                echo "database not reachable — fresh install, hooks run before regular resources; skipping"
              fi
```

</details>

### 4. Add a helm test

`templates/tests/test-readyz.yaml`: a pod with `"helm.sh/hook": test` and
`"helm.sh/hook-delete-policy": before-hook-creation`, image
`curlimages/curl:8.8.0`, that runs
`curl -fsS http://<fullname>:<service.port>/readyz` (write it with the helpers —
it's four lines of spec).

### 5. Install and exercise it

```sh
helm lint charts/guestbook
helm install gb charts/guestbook -n guestbook-helm --create-namespace
kubectl get pods -n guestbook-helm -w   # hook job first (check its logs!), then postgres + api
helm test gb -n guestbook-helm

kubectl -n guestbook-helm port-forward svc/gb-guestbook 8081:8080 &
curl -s -X POST localhost:8081/entries -d '{"message":"installed via dependency"}'
curl -s localhost:8081/entries | jq .
kill %1
```

Watch the hook behavior across the lifecycle: `kubectl logs job/gb-guestbook-db-ready -n guestbook-helm`
during install says "skipping" (DB didn't exist yet); run
`helm upgrade gb charts/guestbook -n guestbook-helm` and the new hook run says
"database reachable" — the pre-install pitfall, observed live.

> Note: do **not** install into the `guestbook` namespace — your hand-rolled
> Day 11 stack and its NetworkPolicies live there. This is the same app,
> packaged; both coexist happily in different namespaces.

### 6. Package and push to an OCI registry

```sh
helm package charts/guestbook          # → guestbook-0.1.0.tgz
UUID=$(uuidgen | tr '[:upper:]' '[:lower:]')
helm push guestbook-0.1.0.tgz oci://ttl.sh/$UUID
helm show chart oci://ttl.sh/$UUID/guestbook --version 0.1.0   # readable back
```

Install from the registry — no local files involved:

```sh
helm install gb-oci oci://ttl.sh/$UUID/guestbook --version 0.1.0 \
  -n guestbook-oci --create-namespace \
  --set postgresql.primary.persistence.enabled=false   # throwaway, skip the PVC
kubectl get pods -n guestbook-oci
helm uninstall gb-oci -n guestbook-oci && kubectl delete ns guestbook-oci
rm guestbook-0.1.0.tgz
```

ttl.sh artifacts expire on their own (≤24 h) — perfect for labs, obviously not
for production. Day 45's CI pushes to a real registry the same way.

### 7. Commit

```sh
git add charts/guestbook .gitignore
git commit -m "guestbook chart: postgresql dependency, hook, test"
```

## Verify ✅

- [ ] `helm dependency list charts/guestbook` shows postgresql with status `ok`
- [ ] `kubectl logs job/...-db-ready` said "skipping" on install and "reachable" after an upgrade
- [ ] `helm test gb -n guestbook-helm` → `Phase: Succeeded`
- [ ] `curl -s localhost:8081/entries | jq .` returns your POSTed entry — served through the **dependency-installed** Postgres (`kubectl get statefulset -n guestbook-helm` shows `gb-postgresql`)
- [ ] `helm show chart oci://ttl.sh/$UUID/guestbook --version 0.1.0` works, and the `gb-oci` install came up before you removed it
- [ ] `Chart.lock` is committed; no `.tgz` files are (`git status` clean of them)

## Stretch goals

- Switch the password to a `secretKeyRef` against `{{ .Release.Name }}-postgresql` (key `password`) and confirm entries still work.
- Set `postgresql.enabled=false` and point `DATABASE_URL` at your Day 11 Postgres instead — that's what `condition:` is for (note the NetworkPolicies in `guestbook` will block you; fixing that is Day 15 knowledge).
- Add `alias: maindb` to the dependency and see what breaks (service names, your values key) — instructive five minutes.
- `helm package --sign` with a throwaway GPG key, then `helm verify` the tarball.

## Cleanup

```sh
helm uninstall gb -n guestbook-helm
kubectl delete pvc -n guestbook-helm --all   # StatefulSet PVCs outlive the release — Day 11 lesson
kubectl delete ns guestbook-helm
```

**Keep:**
- `charts/guestbook` committed in `~/Code/k8s-gitops` — ArgoCD installs it from Git in Phase 4
- The original `guestbook` namespace, untouched as always
- `charts/podlab` and (optionally) the `podlab-dev` release from Day 20
