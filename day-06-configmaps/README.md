# Day 06 — ConfigMaps

> **Time:** ~3 h · **Builds on:** Days 2, 3

## Objectives

- Create ConfigMaps four ways and pick the right one without thinking.
- Consume config as single env vars, `envFrom`, and mounted files — and prove each via podlab's `/config`.
- Demonstrate the update-propagation split: mounted files refresh live, env vars never do, `subPath` mounts never do.
- Explain why platform teams reach for immutable ConfigMaps.

## Concepts

### Config belongs outside the image

You know this from Docker: bake config into the image and every config change is a rebuild, and one image can't serve dev/staging/prod. Twelve-factor says config lives in the environment. Kubernetes' unit for that is the **ConfigMap**: a plain key→value object (values are strings, up to 1 MiB total) stored in the API, decoupled from any pod. The pod *references* it; the kubelet delivers it. Same image, different ConfigMap per environment — that separation is what Days 26+ (Helm values, Kustomize overlays, GitOps) industrialize, so get the foundation exact today.

Keys play two roles depending on consumption: as **env var names** (`LOG_LEVEL=debug`) or as **file names** (key `app-settings.yaml` becomes a file `/etc/podlab/app-settings.yaml` whose content is the value). One object, two delivery shapes.

### Four ways to create, one decision rule

| Mode | Command shape | Reach for it when |
|---|---|---|
| literals | `--from-literal=k=v` | 1–3 simple values, ad hoc |
| file | `--from-file=app.yaml` | the value *is* a config file (key = filename, value = contents) |
| env-file | `--from-env-file=app.env` | a dotenv-style `K=V` list → many literal keys at once |
| YAML manifest | `kind: ConfigMap` + `data:` | anything that lives in Git — i.e., everything real |

The imperative modes are for speed (and the CKA). Production config is YAML in a repo. A trick you'll use constantly: generate the YAML from the imperative form — `kubectl create configmap x --from-file=... --dry-run=client -o yaml`.

### Three ways to consume

1. **Single env var** — `env[].valueFrom.configMapKeyRef`: explicit, one key. Verbose but greppable.
2. **All keys as env** — `envFrom[].configMapRef`: the whole ConfigMap becomes environment. Concise; risk: a junk key like `app-settings.yaml` is not a valid env name and gets silently skipped, and key collisions are resolved invisibly.
3. **Volume mount** — the ConfigMap materializes as files in a directory. The *only* mode that handles multi-line content sanely, and the only mode that **updates live**.

### The update-propagation table (memorize this one)

| Consumption | Pod sees ConfigMap update? | Why |
|---|---|---|
| env (either form) | **never** — needs pod recreation | env is fixed at container start, by Linux itself |
| volume mount | **yes**, within ~a minute | kubelet syncs and atomically swaps a symlink |
| volume mount **with `subPath`** | **never** | subPath copies the file once; the symlink dance is bypassed |

The kubelet delivers mounted ConfigMaps as a hidden timestamped dir plus a `..data` symlink; on update it writes a new dir and flips the symlink atomically — readers never see a half-written file. `subPath` mounts a single file *into an existing directory* (e.g. dropping `nginx.conf` into `/etc/nginx/` without shadowing the rest), but it resolves the symlink at mount time — hence frozen forever. Worth saying out loud: live-updating *files* doesn't mean the *app* reloads; the app must re-read or watch the file. podlab's `/config` re-reads on every request, which makes it the perfect demonstrator.

### Immutable ConfigMaps

`immutable: true` makes the object's data unchangeable (you must delete + recreate to "edit"). Two reasons platform teams default to it: **safety** — nobody can hot-edit prod config under a running app; changes force a new ConfigMap name and therefore a visible, rollback-able rollout (the versioned-name pattern: `app-settings-v2`, flip the pod spec, the Deployment rolls). And **scale** — the kubelet stops watching immutable objects, which on thousand-node clusters meaningfully unloads the API server.

## Lab

podlab reads `CONFIG_DIR` (default `/etc/podlab`) and dumps every env var plus every file under it at `/config` — your X-ray for everything below. The Day 3 `podlab` Deployment + Service should still be running.

### 1. Create, four ways

```sh
kubectl create configmap demo-lit --from-literal=LOG_LEVEL=debug --from-literal=FEATURE_X=on

printf 'greeting: hello\nretries: 3\n' > app-settings.yaml
kubectl create configmap demo-file --from-file=app-settings.yaml

printf 'LOG_LEVEL=info\nTIMEOUT_MS=2500\n' > app.env
kubectl create configmap demo-env --from-env-file=app.env

kubectl get cm demo-lit demo-file demo-env -o yaml | less   # compare data: blocks
kubectl delete cm demo-lit demo-file demo-env
```

Note how `--from-file` produced one key named after the file, value = whole file content, while `--from-env-file` exploded into many keys. Now the real ones, declaratively — `config.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: podlab-env
data:
  LOG_LEVEL: debug
  GREETING_MODE: friendly
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: podlab-files
data:
  app-settings.yaml: |
    greeting: hello from a configmap
    retries: 3
    flags:
      shiny-feature: true
```

```sh
kubectl apply -f config.yaml
```

### 2. Core lab: mount files into CONFIG_DIR

Requirements — update Day 3's `deploy.yaml` (don't start over):

- mount ConfigMap `podlab-files` as a volume at `/etc/podlab`
- consume **all** of `podlab-env` via `envFrom`
- keep everything from Day 3 (3 replicas, VERSION env, Downward API envs)

<details><summary>Solution</summary>

The changed parts of the pod template:

```yaml
    spec:
      volumes:
        - name: settings
          configMap:
            name: podlab-files
      containers:
        - name: podlab
          image: podlab:v1
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: podlab-env
          env:
            - name: VERSION
              value: "2.0.0"
            - name: POD_IP
              valueFrom: { fieldRef: { fieldPath: status.podIP } }
            - name: NODE_NAME
              valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
            - name: POD_NAMESPACE
              valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
          volumeMounts:
            - name: settings
              mountPath: /etc/podlab
```

</details>

```sh
kubectl apply -f deploy.yaml
kubectl rollout status deployment/podlab
kubectl port-forward svc/podlab 8081:80 &
curl -s localhost:8081/config | python3 -m json.tool
```

Read the proof in the JSON: under `files`, `/etc/podlab/app-settings.yaml` with your exact YAML content (plus the `..data` symlink internals); under `env`, `LOG_LEVEL=debug` and `GREETING_MODE=friendly`. Both ConfigMaps, both delivery modes, one screen.

### 3. The update split — files move, env doesn't

Edit `config.yaml`: in `podlab-files`, change `retries: 3` → `retries: 99`; in `podlab-env`, change `LOG_LEVEL: debug` → `LOG_LEVEL: warn`. Then:

```sh
kubectl apply -f config.yaml
date    # mark the time
watch -n 5 'curl -s localhost:8081/config | grep -o "retries: [0-9]*"'
```

Within ~a minute: `retries: 99` — **no pod restart, no rollout** (confirm: `kubectl get pods -l app=podlab` shows old AGE, `RESTARTS 0`). Now the env side:

```sh
curl -s localhost:8081/config | grep -o '"LOG_LEVEL":"[^"]*"'    # still "debug"!
```

The file updated; the env var didn't and never will — until the container is recreated:

```sh
kubectl rollout restart deployment/podlab && kubectl rollout status deployment/podlab
kill %1; kubectl port-forward svc/podlab 8081:80 &
curl -s localhost:8081/config | grep -o '"LOG_LEVEL":"[^"]*"'    # "warn"
```

This asymmetry causes real outages ("we updated the ConfigMap, why is prod still broken?"). The blunt production answer: treat *all* config changes as rollouts (checksum annotations / Helm does this — Day 26), and enjoy live file updates only when the app explicitly hot-reloads.

### 4. subPath: surgical placement, frozen content

Sometimes you need *one* file inside a directory that must keep its other contents. Add a second mount to the container (same volume!):

```yaml
            - name: settings
              mountPath: /etc/podlab-static/app-settings.yaml
              subPath: app-settings.yaml
```

Apply, wait for the rollout, then bump `retries` to `100` in `config.yaml` and apply. Compare after ~90 s:

```sh
curl -s localhost:8081/config | python3 -m json.tool | grep -A1 'retries'
```

The `/etc/podlab/` copy says `100`; the `/etc/podlab-static/` copy is frozen at `99`. Same volume, same ConfigMap — `subPath` alone breaks the update chain. Remove the subPath mount and apply again before moving on.

### 5. Immutable + the versioned-name pattern

```sh
kubectl patch cm podlab-files -p '{"immutable": true}'
kubectl patch cm podlab-files --type=merge -p '{"data":{"app-settings.yaml":"hacked: true"}}'
# Error ... field is immutable when `immutable` is set
```

The change is rejected at the API — no race, no hot-edit. To "change" it now, version the name: copy the ConfigMap in `config.yaml` to `podlab-files-v2` (with new content), point the Deployment's volume at `podlab-files-v2`, apply. The Deployment rolls (template changed → new ReplicaSet → Day 3 machinery), and `rollout undo` now rolls back config too — that's the operational win over in-place edits. Note `immutable: true` is also *itself* irreversible on the object; delete `podlab-files` once nothing mounts it.

## Verify ✅

- [ ] `curl -s localhost:8081/config | python3 -m json.tool` → `files` contains key ending `app-settings.yaml` with your current content; `env` contains `GREETING_MODE`
- [ ] After step 3's file edit: `curl …/config | grep retries` shows the new value with `kubectl get pods -l app=podlab` showing `RESTARTS 0` and unchanged AGE
- [ ] After step 3's env edit but before the restart: `LOG_LEVEL` still shows the old value; after `rollout restart`: the new one
- [ ] Step 4: subPath copy differs from the directory-mount copy after an update
- [ ] `kubectl patch cm podlab-files --type=merge -p '{"data":{"x":"y"}}'` → rejected with `field is immutable`

## CKA corner 🎓

Exam notes:

- Imperative creation speed matters: `kubectl create cm name --from-literal=a=b --from-file=f.txt --from-env-file=e.env` — know all three flags without thinking.
- `kubectl explain pod.spec.containers.env.valueFrom.configMapKeyRef` and `...volumes.configMap` when the structure escapes you.
- If a pod references a missing ConfigMap: env-consuming pods stay `CreateContainerConfigError`, volume-consuming pods hang in `ContainerCreating` — recognize both in `describe`.

**Drill 1 (3 min):** Create ConfigMap `webcfg` with key `index.html` = `<h1>drill</h1>`. Run an nginx pod serving it at `/usr/share/nginx/html/index.html` via a volume (not subPath). Curl it through a port-forward.

<details><summary>Solution</summary>

```sh
kubectl create configmap webcfg --from-literal=index.html='<h1>drill</h1>'
kubectl run web --image=nginx --dry-run=client -o yaml > web.yaml
```

Add to the spec:

```yaml
  volumes:
    - name: html
      configMap: {name: webcfg}
  containers:
    - name: web
      image: nginx
      volumeMounts: [{name: html, mountPath: /usr/share/nginx/html}]
```

```sh
kubectl apply -f web.yaml
kubectl port-forward pod/web 8082:80 & sleep 2; curl -s localhost:8082; kill %1
kubectl delete pod web cm/webcfg
```

</details>

**Drill 2 (2 min):** Pod `envy` (busybox, `sleep 3600`) must expose key `color` of ConfigMap `palette` as env var `PRIMARY_COLOR`. Create both; verify with `kubectl exec envy -- env`.

<details><summary>Solution</summary>

```sh
kubectl create cm palette --from-literal=color=teal
kubectl run envy --image=busybox --dry-run=client -o yaml --command -- sleep 3600 > envy.yaml
```

Add under the container:

```yaml
      env:
        - name: PRIMARY_COLOR
          valueFrom:
            configMapKeyRef: {name: palette, key: color}
```

```sh
kubectl apply -f envy.yaml && sleep 5
kubectl exec envy -- env | grep PRIMARY_COLOR   # PRIMARY_COLOR=teal
kubectl delete pod envy cm/palette
```

</details>

## Stretch goals

- Mount only *some* keys with `volumes[].configMap.items` (and rename the file via `items[].path`).
- Make a key `optional: true` and prove the pod starts even when the ConfigMap is absent.
- Watch the symlink swap live: `kubectl exec` won't work (distroless), so use `kubectl debug -it deploy/podlab --image=busybox --target=podlab` and `ls -la /etc/podlab` before/after an update — watch `..data` retarget.
- Binary config: `binaryData:` field — base64 in the manifest, raw bytes in the mounted file.

## Cleanup

```sh
kill %1 2>/dev/null                       # port-forward
kubectl delete cm podlab-files            # the immutable original, once unmounted
rm -f app-settings.yaml app.env
```

**Keep:** the `podlab` Deployment/Service with `podlab-env` + your current files ConfigMap mounted — Day 7 adds a Secret into the *same* directory via a projected volume.
