# Day 22 — Kustomize: Bases, Overlays, Patches

> **Time:** ~3 h · **Builds on:** Days 6, 9, 20

## Objectives

- Explain when template-free customization beats templating (and vice versa) — and articulate the decision in an interview
- Build a `base` + `dev/stage/prod` overlays tree for podlab with patches, generators, and transformers
- Use the configMapGenerator **hash-suffix** trick to get automatic rollouts on config changes
- Run all of it with plain `kubectl` (`apply -k`, `kubectl kustomize`)

## Concepts

### A different answer to the same question

Helm asks: *"which parts of this YAML are variables?"* — and the chart author
must anticipate every knob, or users fork the chart. Kustomize asks a different
question: *"you have valid, complete YAML; what do you want to change?"* No
templates, no values, no `{{ }}`. A **base** is plain deployable YAML; an
**overlay** declares targeted modifications — *patches* — and Kustomize merges
them at build time. Anything is patchable, even fields the original author
never imagined, which is why Kustomize shines for consuming YAML you don't own
and for environment variance (dev/stage/prod), while Helm shines for
*distributing* parameterized software with lifecycle (install/upgrade/rollback —
Kustomize has none of that; it only emits YAML).

It ships **inside kubectl**: `kubectl apply -k <dir>` and `kubectl kustomize
<dir>` (render only). No new tool to install, and it's fair game on the CKA.

### kustomization.yaml anatomy

Every directory in a Kustomize tree has a `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:            # what to include: files, dirs, other kustomizations, URLs
  - ../../base
  - namespace.yaml
namespace: podlab-dev # transformer: set metadata.namespace on everything
labels:               # transformer: add labels (replaces deprecated commonLabels)
  - pairs: {app.kubernetes.io/name: podlab}
    includeSelectors: true
replicas:             # transformer: override replica counts by name
  - {name: podlab, count: 1}
images:               # transformer: retarget images without touching specs
  - {name: podlab, newTag: v2}
patches:              # surgical edits (two flavors, below)
  - path: patch-color.yaml
configMapGenerator:   # generator: build ConfigMaps from files/literals
  - name: app-settings
    files: [app-settings.yaml]
```

Mental model: **resources** are collected, **generators** add objects,
**transformers** rewrite everything, in a defined order. An overlay's
`resources:` pointing at `../../base` is what makes it an overlay.

### The two patch flavors

| | Strategic merge patch | JSON6902 patch |
|---|---|---|
| Looks like | A partial copy of the object | A list of operations (`op/path/value`) |
| Merging | Schema-aware: lists with merge keys (e.g. `env` by `name`) merge, not clobber | Mechanical: exact paths, including array indexes |
| Use for | "Same shape" changes: env vars, resources, labels | Surgical edits: one field deep in a list, deletions |

Strategic merge is what `kubectl apply` does internally (Day 3); JSON6902 is
the scalpel when merge semantics fight you.

### The killer feature: generated names with hash suffixes

`configMapGenerator` doesn't just create a ConfigMap — it names it
`app-settings-<hash-of-content>` and **rewrites every reference** to it in the
build output. Change the file → new hash → new ConfigMap name → the Deployment's
pod template changes → Kubernetes rolls the pods. Config changes become
rollouts *with rollback history*, with zero annotations or operators. (On Day 20
you hand-built this with a `checksum/config` annotation; Kustomize gives it to
you for free. Old hash-named ConfigMaps linger as garbage — prune is ArgoCD's
job in Phase 4.)

### Helm vs Kustomize — the decision

| Situation | Reach for |
|---|---|
| Distributing an app to strangers with knobs + docs | Helm |
| Needing install/upgrade/rollback/hooks/tests lifecycle | Helm |
| dev/stage/prod variance of YAML **you** own | Kustomize |
| Patching third-party YAML you can't edit (or a chart's gaps) | Kustomize |
| Complex conditionals, loops, generated config files | Helm |
| "I want to read exactly what ships, no indirection" | Kustomize |

And **"both"** is a first-class answer: `helm template ... | kustomize` (or
Kustomize's built-in `helmCharts:` field) renders a chart and patches the
output — common for "the chart doesn't expose the knob I need". ArgoCD supports
Helm and Kustomize natively; **Day 27 points an ApplicationSet at the exact
overlays you build today** to fan out dev/stage/prod automatically. That's why
this tree lives in `~/Code/k8s-gitops`.

Also worth knowing (no lab): **components** (`kind: Component`) are reusable
opt-in patch bundles (e.g. "add HA bits") that multiple overlays can include,
and patches apply across *multi-document* bases just fine — targets are matched
by group/kind/name, not file layout.

## Lab

### 1. The base

Core artifact: in `~/Code/k8s-gitops/kustomize/podlab/`, a `base/` containing a
plain podlab `deployment.yaml` + `service.yaml` + `kustomization.yaml`.
Requirements:

- Deployment `podlab`, 1 replica, image `podlab:v1` (`imagePullPolicy: IfNotPresent`), env `VERSION=1.0.0`, `COLOR=base`, `CONFIG_DIR=/etc/podlab`, the three Downward API envs, probes on `/healthz`, modest resources, and a volume mounting ConfigMap **`app-settings`** at `/etc/podlab` (the generator in each overlay will provide it — the base alone is intentionally not deployable, that's normal for bases)
- Service `podlab`, port 80 → targetPort `http`
- `kustomization.yaml`: lists the two resources; adds `app.kubernetes.io/name: podlab` via the `labels:` field with `includeSelectors: true`

<details><summary>Solution</summary>

`kustomize/podlab/base/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podlab
spec:
  replicas: 1
  selector:
    matchLabels:
      app: podlab
  template:
    metadata:
      labels:
        app: podlab
    spec:
      containers:
        - name: podlab
          image: podlab:v1
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: VERSION
              value: "1.0.0"
            - name: COLOR
              value: base
            - name: CONFIG_DIR
              value: /etc/podlab
            - name: POD_IP
              valueFrom: {fieldRef: {fieldPath: status.podIP}}
            - name: NODE_NAME
              valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
            - name: POD_NAMESPACE
              valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
          readinessProbe:
            httpGet: {path: /healthz, port: http}
            periodSeconds: 5
            failureThreshold: 3
          livenessProbe:
            httpGet: {path: /healthz, port: http}
            periodSeconds: 5
            failureThreshold: 3
          resources:
            requests: {cpu: 50m, memory: 32Mi}
            limits: {cpu: 250m, memory: 64Mi}
          volumeMounts:
            - name: config
              mountPath: /etc/podlab
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: app-settings
```

`kustomize/podlab/base/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: podlab
spec:
  selector:
    app: podlab
  ports:
    - name: http
      port: 80
      targetPort: http
```

`kustomize/podlab/base/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - service.yaml
labels:
  - pairs:
      app.kubernetes.io/name: podlab
    includeSelectors: true
```

</details>

### 2. Three overlays

Requirements — `overlays/dev`, `overlays/stage`, `overlays/prod`, each with:

- a `namespace.yaml` (Namespace `podlab-dev`/`podlab-stage`/`podlab-prod`) in `resources:` **and** the `namespace:` field (the field stamps `metadata.namespace` onto namespaced objects; it does not create the Namespace — you need both)
- `replicas:` → 1 / 2 / 3
- a strategic-merge patch setting `COLOR` to `dev`/`stage`/`prod` (note how the `env` list *merges by name* — your other envs survive)
- `configMapGenerator` named `app-settings` from a local `app-settings.yaml` whose content differs per env
- **prod only:** a JSON6902 patch raising the liveness probe's `failureThreshold` to 5 (surgical: one field, deep in a list), and an `images:` transformer pinning `newTag: v2`

Build podlab v2 first (same binary, new tag — 30 seconds):

```sh
cd ~/Code/cloud-engineer-course
docker build -t podlab:v2 apps/podlab
kind load docker-image podlab:v2 --name course
```

<details><summary>Solution</summary>

`kustomize/podlab/overlays/dev/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: podlab-dev
resources:
  - ../../base
  - namespace.yaml
replicas:
  - name: podlab
    count: 1
patches:
  - path: patch-color.yaml
configMapGenerator:
  - name: app-settings
    files:
      - app-settings.yaml
```

`kustomize/podlab/overlays/dev/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: podlab-dev
```

`kustomize/podlab/overlays/dev/patch-color.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podlab
spec:
  template:
    spec:
      containers:
        - name: podlab
          env:
            - name: COLOR
              value: dev
```

`kustomize/podlab/overlays/dev/app-settings.yaml`:

```yaml
environment: dev
debug: true
```

`stage/` is identical with `dev` → `stage`, `count: 2`, `debug: false`.

`kustomize/podlab/overlays/prod/kustomization.yaml` (the extras):

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: podlab-prod
resources:
  - ../../base
  - namespace.yaml
replicas:
  - name: podlab
    count: 3
images:
  - name: podlab
    newTag: v2
patches:
  - path: patch-color.yaml
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: podlab
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/livenessProbe/failureThreshold
        value: 5
configMapGenerator:
  - name: app-settings
    files:
      - app-settings.yaml
```

`prod/app-settings.yaml`: `environment: prod`, `debug: false`.

</details>

### 3. Inspect, then apply

**Always render before applying** — same discipline as `helm template`:

```sh
cd ~/Code/k8s-gitops/kustomize/podlab
kubectl kustomize overlays/dev | less
```

Find in the output: the namespace stamped everywhere, merged labels in
selectors, `COLOR: dev` *alongside* the other envs, and `app-settings-<hash>`
both as the ConfigMap name and in the Deployment's volume. Then:

```sh
kubectl apply -k overlays/dev
kubectl apply -k overlays/stage
kubectl apply -k overlays/prod
kubectl get pods -A -l app.kubernetes.io/name=podlab
```

1 + 2 + 3 pods across three namespaces, from one base. Check prod runs `v2`:

```sh
kubectl get deploy podlab -n podlab-prod -o jsonpath='{.spec.template.spec.containers[0].image}'
```

### 4. Prove each env is distinct

```sh
for env in dev stage prod; do
  kubectl -n podlab-$env port-forward svc/podlab 8080:80 >/dev/null 2>&1 &
  sleep 2
  echo "== $env =="
  curl -s localhost:8080/ | jq '{color, namespace: .namespace}'
  curl -s localhost:8080/config | jq -r '.files["app-settings.yaml"]'
  kill %1; wait 2>/dev/null
done
```

Each env must report its own `color` and its own `app-settings.yaml` content.

### 5. The hash-suffix rollout

```sh
kubectl get cm -n podlab-dev                      # note app-settings-<hash>
# now change overlays/dev/app-settings.yaml (debug: true → false), then:
kubectl apply -k overlays/dev
kubectl get pods -n podlab-dev -w                 # pods roll automatically
kubectl get cm -n podlab-dev                      # NEW hash; old CM still there (un-pruned)
```

No annotation, no restart command — the name change *is* the rollout trigger.
Compare with Day 6, where a mounted ConfigMap edit updated lazily and nothing
rolled.

### 6. Commit

```sh
cd ~/Code/k8s-gitops
git add kustomize
git commit -m "podlab kustomize tree: base + dev/stage/prod overlays"
```

## Verify ✅

- [ ] `kubectl get deploy -A -l app.kubernetes.io/name=podlab` shows 3 deployments in `podlab-dev/stage/prod` with 1/2/3 ready replicas
- [ ] Step 4's loop printed three different `color` values and three different `app-settings.yaml` contents
- [ ] `kubectl get pod -n podlab-prod -o jsonpath='{.items[0].spec.volumes[0].configMap.name}'` shows a hash-suffixed name like `app-settings-7g9c4tm5bk`
- [ ] prod image is `podlab:v2`; prod's liveness `failureThreshold` is `5` (`kubectl get deploy podlab -n podlab-prod -o yaml | grep -A1 failureThreshold`)
- [ ] Editing dev's `app-settings.yaml` + re-apply caused a visible pod rollout
- [ ] Tree committed in `~/Code/k8s-gitops/kustomize/podlab/`

## CKA corner 🎓

Kustomize is built into kubectl and shows up on the exam as tooling. Drill:

1. **Render vs apply:** `kubectl kustomize <dir>` (print) vs `kubectl apply -k <dir>` (apply) vs `kubectl delete -k <dir>`. Know all three cold.
2. **Speed drill (target: 6 min):** given any deployment+service YAML, build a `base/` and one overlay that changes namespace and replicas. From memory — `kustomization.yaml` boilerplate (`apiVersion: kustomize.config.k8s.io/v1beta1`, `kind: Kustomization`, `resources:`) must be in your fingers.
3. Remember `-k` ≠ `-f`: pointing `kubectl apply -f` at a kustomize dir applies the `kustomization.yaml` *as if it were a resource* and fails. Seen it cost exam minutes.

## Stretch goals

- Add a `components/` directory with a Component that adds the ingress (host `podlab-<env>.localhost`) and include it only in dev — then `curl http://podlab-dev.localhost:8080`.
- The "both" workflow: `helm template podlab-dev ../../charts/podlab > rendered.yaml`, then a kustomization listing `rendered.yaml` as a resource and patching something the chart doesn't expose (e.g. add a `topologySpreadConstraint`). Helm renders, Kustomize patches.
- Run `kubectl kustomize base` and read the error-free but undeployable output (dangling `app-settings` reference) — internalize why bases aren't applied directly.
- `kubectl apply -k overlays/dev --prune` is the old answer to leftover hashed ConfigMaps; read `kubectl apply --help` on why it's risky (ArgoCD does this properly in Phase 4).

## Cleanup

```sh
kubectl delete ns podlab-stage podlab-prod   # save resources
```

**Keep:**
- The **committed kustomize tree** — Day 27's ApplicationSet deploys these exact overlays from Git (it will recreate stage/prod)
- `podlab-dev` namespace (1 pod) if you want a running reference; deleting it is also fine
- `guestbook` namespace untouched
