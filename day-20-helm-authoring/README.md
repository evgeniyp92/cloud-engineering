# Day 20 — Helm Authoring: a Chart for podlab

> **Time:** ~3.5 h · **Builds on:** Days 6, 10, 19

## Objectives

- Read and write Go template syntax: pipelines, `default`, `quote`, `toYaml | nindent`, `if`/`with`/`range`, `include`
- Structure a chart the standard way: `Chart.yaml`, `values.yaml`, `templates/`, `_helpers.tpl`, `NOTES.txt`
- Design `values.yaml` as the chart's public API — including a `config:` dict that renders into mounted files
- Start your **platform repo** (`~/Code/k8s-gitops`) — the Git repo ArgoCD will deploy from in Phase 4

## Concepts

### A second repo, on purpose

Today you create `~/Code/k8s-gitops`. The course repo is *courseware*;
`k8s-gitops` is *your platform* — the charts, overlays, and (from Phase 4)
ArgoCD definitions describing everything running in your cluster. On Day 24
you push it to GitHub and point ArgoCD at it; from then on **Git becomes the
deploy mechanism**, which only works if the repo contains nothing but
deployable config — hence the separation. By Day 50 it *is* your portfolio.

### Chart anatomy

```
charts/podlab/
├── Chart.yaml          # name, version (chart), appVersion (app) — metadata
├── values.yaml         # DEFAULTS + documentation = the chart's API
├── .helmignore         # files to exclude from the package
└── templates/
    ├── _helpers.tpl    # named templates (underscore = renders no output)
    ├── deployment.yaml
    ├── service.yaml
    ├── ingress.yaml
    ├── configmap.yaml
    └── NOTES.txt       # printed after install/upgrade
```

Two versions live in `Chart.yaml`: `version` is the **chart's** semver (bump
when templates change); `appVersion` is the **application's** (informational).
CI in Phase 7 bumps them independently.

### Go templates, the 20% you need

Everything inside `{{ }}` is Go template syntax plus the
[Sprig](https://masterminds.github.io/sprig/) function library. The built-in
objects:

| Object | Contains | Example |
|---|---|---|
| `.Values` | merged values (defaults + `-f` + `--set`) | `.Values.image.tag` |
| `.Release` | install-time facts | `.Release.Name`, `.Release.Namespace` |
| `.Chart` | Chart.yaml fields | `.Chart.Name`, `.Chart.Version` |
| `.Capabilities` | cluster facts (API versions) | rarely needed locally |

The constructs you'll actually write:

```yaml
# pipelines: value flows left → right; default supplies a fallback
image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
# quote: YAML treats true, 1.0, on as non-strings — always quote env values
value: {{ .Values.color | quote }}
# toYaml | nindent: re-emit a whole values subtree as YAML, correctly indented
resources: {{- toYaml .Values.resources | nindent 2 }}
# control flow
{{- if .Values.ingress.enabled }} ... {{- end }}
{{- range $name, $content := .Values.config }} ... {{- end }}
```

Three syntax gotchas that cause 90% of beginner pain:

1. **Whitespace.** `{{-` eats whitespace (including the newline) *before* the
   tag, `-}}` after. Wrong trimming = invalid YAML indentation. When confused,
   `helm template` and look.
2. **`indent` vs `nindent`.** `indent 4` prefixes every line with 4 spaces;
   `nindent 4` also adds a **leading newline**, so the tag can sit flush after
   a key (`resources: {{- toYaml ... | nindent 2 }}`). Use `nindent`.
3. **`include` vs `template`.** Both call a named template, but `template` is
   a statement whose output can't be piped; `include` is a function:
   `{{ include "podlab.labels" . | nindent 4 }}`. **Always use `include`.**
   The trailing `.` passes the current context into the template.

### `_helpers.tpl` and the labels pattern

Named templates (`{{- define "podlab.fullname" -}}...{{- end }}`) live in
`_helpers.tpl`. Every serious chart defines the same trio:

- `podlab.fullname` — release-qualified resource name (`podlab-dev-podlab` →
  deduplicated to `podlab-dev` when the release name contains the chart name)
- `podlab.labels` — full label set, applied to every object's `metadata.labels`
- `podlab.selectorLabels` — the *minimal stable subset* used in
  `spec.selector` and pod labels

Why two label templates? Deployment selectors are **immutable**: if
`helm.sh/chart: podlab-0.1.0` were in the selector, every chart version bump
would try to mutate it and fail. So the selector gets only
`app.kubernetes.io/name` + `instance`; everything else rides in the
non-selector labels (the Day 9 conventions, applied consistently by helpers).

### values.yaml is an API — design it

Whoever installs your chart reads `values.yaml` first. Treat it like a public
interface: group related knobs, comment every one, ship working defaults.
Today's signature feature: a `config:` dict where **each key becomes a file
under `/etc/podlab`** — podlab's `/config` endpoint (Day 6) *proves* the
rendered ConfigMap landed. Paired with a `checksum/config` pod annotation,
changing config in values rolls the Deployment — a pattern you'll reuse forever.

## Lab

### 1. Create the platform repo

```sh
mkdir -p ~/Code/k8s-gitops && cd ~/Code/k8s-gitops
git init
printf '# k8s-gitops\n\nMy platform repo: Helm charts + Kustomize overlays.\nArgoCD deploys this repo from Phase 4 of the course.\n' > README.md
git add README.md && git commit -m "init platform repo"
```

### 2. Scaffold, tour, then gut

```sh
mkdir charts
helm create charts/podlab
```

Tour the scaffold — note how much machinery it generates (serviceaccount, hpa,
tests, a very abstract ingress). Scaffolds are for reading, not shipping. Keep
`Chart.yaml` and `.helmignore`, delete the rest so you own every line:

```sh
rm -rf charts/podlab/templates/* charts/podlab/charts
rm charts/podlab/values.yaml
```

### 3. Write the chart

This is the day's core artifact. Requirements:

- **`values.yaml`** (commented!): `replicaCount`; `image.repository/tag/pullPolicy`
  (pullPolicy `IfNotPresent` — the image was `kind load`-ed, a pull would fail);
  `version` and `color` (→ `VERSION`/`COLOR` env); `probes.initialDelaySeconds/
  periodSeconds/failureThreshold`; `resources`; `service.port`;
  `ingress.enabled/className/host`; `config:` dict (each key = a filename
  rendered under `/etc/podlab`)
- **`_helpers.tpl`**: `podlab.name`, `podlab.fullname`, `podlab.labels`,
  `podlab.selectorLabels` (the standard `app.kubernetes.io/*` set)
- **`deployment.yaml`**: image from values; env `VERSION`, `COLOR`,
  `CONFIG_DIR=/etc/podlab` from values; Downward API env `POD_IP`, `NODE_NAME`,
  `POD_NAMESPACE` (as on Day 2); liveness + readiness probes on `/healthz`
  with thresholds from values; `resources` via `toYaml`; ConfigMap mounted at
  `/etc/podlab`; a `checksum/config` pod annotation hashing the rendered
  ConfigMap
- **`configmap.yaml`**: `range` over `.Values.config`
- **`service.yaml`**: port from values → named target port `http`
- **`ingress.yaml`**: only rendered `if .Values.ingress.enabled`
- **`NOTES.txt`**: prints the exact curl command to reach the release

Write it yourself, `helm lint charts/podlab` + `helm template charts/podlab`
after every file. Then compare:

<details><summary>Solution</summary>

`charts/podlab/Chart.yaml`:

```yaml
apiVersion: v2
name: podlab
description: The course demo app, properly packaged
type: application
version: 0.1.0
appVersion: "1.0.0"
```

`charts/podlab/values.yaml`:

```yaml
# -- Number of pod replicas
replicaCount: 1

image:
  repository: podlab
  tag: v1
  # The image is loaded into kind with `kind load docker-image` — never pull.
  pullPolicy: IfNotPresent

# -- Reported by GET / and podlab_build_info; becomes the VERSION env var
version: "1.0.0"
# -- Becomes the COLOR env var (blue/green & canary demos later)
color: "blue"

service:
  port: 80

# -- Probe tuning; both probes hit GET /healthz
probes:
  initialDelaySeconds: 2
  periodSeconds: 5
  failureThreshold: 3

resources:
  requests: {cpu: 50m, memory: 32Mi}
  limits: {cpu: 250m, memory: 64Mi}

ingress:
  enabled: false
  className: nginx
  # With the course kind cluster, *.localhost:8080 reaches ingress-nginx
  host: podlab.localhost

# -- Each key becomes a file under /etc/podlab (verify via GET /config)
config:
  app.properties: |
    greeting=hello from helm
  flags.yaml: |
    newUI: false
```

`charts/podlab/templates/_helpers.tpl`:

```yaml
{{- define "podlab.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "podlab.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "podlab.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{ include "podlab.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "podlab.selectorLabels" -}}
app.kubernetes.io/name: {{ include "podlab.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
```

`charts/podlab/templates/configmap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "podlab.fullname" . }}-config
  labels:
    {{- include "podlab.labels" . | nindent 4 }}
data:
{{- range $name, $content := .Values.config }}
  {{ $name }}: |-
    {{- $content | trim | nindent 4 }}
{{- end }}
```

`charts/podlab/templates/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "podlab.fullname" . }}
  labels:
    {{- include "podlab.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "podlab.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "podlab.selectorLabels" . | nindent 8 }}
      annotations:
        # Changing any config value changes this hash → pods roll automatically
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
    spec:
      containers:
        - name: podlab
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: VERSION
              value: {{ .Values.version | quote }}
            - name: COLOR
              value: {{ .Values.color | quote }}
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
            initialDelaySeconds: {{ .Values.probes.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.periodSeconds }}
            failureThreshold: {{ .Values.probes.failureThreshold }}
          livenessProbe:
            httpGet: {path: /healthz, port: http}
            initialDelaySeconds: {{ .Values.probes.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.periodSeconds }}
            failureThreshold: {{ .Values.probes.failureThreshold }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          volumeMounts:
            - name: config
              mountPath: /etc/podlab
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: {{ include "podlab.fullname" . }}-config
```

`charts/podlab/templates/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "podlab.fullname" . }}
  labels:
    {{- include "podlab.labels" . | nindent 4 }}
spec:
  selector:
    {{- include "podlab.selectorLabels" . | nindent 4 }}
  ports:
    - name: http
      port: {{ .Values.service.port }}
      targetPort: http
```

`charts/podlab/templates/ingress.yaml`:

```yaml
{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "podlab.fullname" . }}
  labels:
    {{- include "podlab.labels" . | nindent 4 }}
spec:
  ingressClassName: {{ .Values.ingress.className }}
  rules:
    - host: {{ .Values.ingress.host }}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {{ include "podlab.fullname" . }}
                port:
                  name: http
{{- end }}
```

`charts/podlab/templates/NOTES.txt`:

```
{{ .Chart.Name }} {{ .Chart.Version }} installed as release "{{ .Release.Name }}".

Reach it:
{{- if .Values.ingress.enabled }}
  curl http://{{ .Values.ingress.host }}:8080/
{{- else }}
  kubectl -n {{ .Release.Namespace }} port-forward svc/{{ include "podlab.fullname" . }} 8080:{{ .Values.service.port }}
  curl localhost:8080/config
{{- end }}
```

</details>

### 4. Lint, render, install

```sh
helm lint charts/podlab
helm template podlab-dev charts/podlab | less     # read EVERYTHING once
helm install podlab-dev charts/podlab -n helm-lab
```

Follow your own NOTES.txt:

```sh
kubectl -n helm-lab port-forward svc/podlab-dev 8080:80 &
curl -s localhost:8080/ | jq '{version, color}'
curl -s localhost:8080/config | jq .files
```

`files` must show `app.properties` and `flags.yaml` with your values content —
the values→ConfigMap→mount→`/config` chain, proven end to end.

### 5. Upgrade with changed config

Change `config["app.properties"]` to `greeting=hello from revision 2` and
`color` to `green` in `values.yaml`, then:

```sh
helm diff upgrade podlab-dev charts/podlab -n helm-lab   # habit from Day 19
helm upgrade podlab-dev charts/podlab -n helm-lab
kubectl get pods -n helm-lab -w    # pods ROLL — the checksum annotation did that
curl -s localhost:8080/config | jq -r '.files["app.properties"]'
helm history podlab-dev -n helm-lab
```

Without the checksum annotation, only the ConfigMap would update and pods
would pick it up lazily (Day 6); the hash makes config deploy like code.

### 6. Commit

```sh
cd ~/Code/k8s-gitops
git add charts/podlab
git commit -m "podlab chart 0.1.0"
```

## Verify ✅

- [ ] `helm lint charts/podlab` → `1 chart(s) linted, 0 chart(s) failed`
- [ ] `helm template podlab-dev charts/podlab` renders with no errors and every object carries `app.kubernetes.io/name: podlab` + `app.kubernetes.io/instance: podlab-dev`
- [ ] `curl -s localhost:8080/config | jq .files` shows file contents that came from `values.yaml`
- [ ] After the upgrade, the same curl shows `revision 2` content and `curl -s localhost:8080/ | jq .color` says `green`
- [ ] `helm history podlab-dev -n helm-lab` shows revisions 1 and 2, both `deployed`/`superseded`
- [ ] `git -C ~/Code/k8s-gitops log --oneline` shows the chart commit

## Stretch goals

- Upgrade with `--set ingress.enabled=true` and hit `http://podlab.localhost:8080/` — your `if` block in action.
- Add `fullnameOverride` support to `podlab.fullname` (look at what `helm create` generated before you deleted it).
- Install a second release `podlab-blue` with `color=blue` next to `podlab-dev` — one chart, two releases, zero conflicts (thanks, fullname + selectorLabels).

## Cleanup

```sh
kill %1 2>/dev/null   # stop the port-forward
```

**Keep:**
- `~/Code/k8s-gitops` with the committed chart — Days 21–23 build on it, Phase 4 deploys it
- The `podlab-dev` release in `helm-lab` (1 small pod) — Day 23 improves this chart and you'll upgrade in place; uninstall it only if you're tight on resources
- `guestbook` namespace untouched
