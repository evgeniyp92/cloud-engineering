# Day 07 — Secrets

> **Time:** ~3 h · **Builds on:** Days 2, 6

## Objectives

- Create Secrets of the right type (`Opaque`, `tls`, `dockerconfigjson`) and explain `data` vs `stringData`.
- Demonstrate in one command that base64 is encoding, not encryption — and say where real protection comes from.
- Combine a ConfigMap and a Secret into one directory with a **projected volume**, proven via podlab's `/config`.
- Argue concretely why mounted secrets beat env-var secrets, and outline the etcd/RBAC exposure surface.

## Concepts

### A Secret is a ConfigMap with a reputation

Mechanically, a Secret is almost exactly yesterday's ConfigMap: key→value object, consumed as env or files, same update-propagation rules (mounted = live updates, env = frozen, subPath = frozen), same 1 MiB cap. The differences are policy, not mechanics:

- values in `data:` are **base64-encoded** in the manifest/API (to carry arbitrary bytes — not to hide anything),
- Kubernetes treats them more carefully: mounted via `tmpfs` (RAM, never the node's disk), not shown by `kubectl describe`, supposedly watched more closely by RBAC policy,
- the `type` field lets the API validate shape for well-known formats.

The mental model to keep: **a Secret is a label that says "handle with care" — the actual care is up to you and your cluster's configuration.**

### Types

| Type | Holds | Required keys |
|---|---|---|
| `Opaque` (default) | anything | none |
| `kubernetes.io/tls` | a certificate + key | `tls.crt`, `tls.key` |
| `kubernetes.io/dockerconfigjson` | registry credentials | `.dockerconfigjson` |
| `kubernetes.io/service-account-token` | a long-lived SA token (legacy; auto-mounted *projected* tokens replaced it) | `token`, `ca.crt`, `namespace` |
| `kubernetes.io/basic-auth`, `ssh-auth` | conventions for creds | `username`/`password`, `ssh-privatekey` |

Typed secrets aren't decoration: Ingress TLS (Day 40) *requires* `kubernetes.io/tls`, and `imagePullSecrets` requires `dockerconfigjson`. The API rejects malformed ones at create time instead of at 3 a.m.

### `data` vs `stringData`

`data:` takes base64; `stringData:` takes plain text and the API server base64-encodes it on write (read-back always shows `data:`). Use `stringData` when authoring YAML by hand — but remember that *any* secret YAML on disk or in Git is plaintext-equivalent. That tension ("config belongs in Git" vs "secrets must not be in Git") has a whole day: sealed-secrets, Day 28.

### base64 ≠ encryption, and where secrets actually leak

`echo c3VwZXJzZWNyZXQ= | base64 -d` — that's the "protection". Anyone who can *read* the Secret object has the value. The real exposure surface, in order of how often it bites:

1. **RBAC** — who can `get`/`list` secrets in the namespace? `list` alone returns full objects, not just names. Also: anyone who can create a pod that mounts a secret can read it through the pod, regardless of their direct secret permissions.
2. **etcd** — by default secrets sit in etcd **base64-encoded, otherwise plaintext**. Anyone with etcd access or an etcd backup has everything. Encryption-at-rest (`EncryptionConfiguration` on the API server, or a KMS) fixes this; on Day 16 you'll exec into our etcd and look at the bytes yourself.
3. **The pod's environment** — see next.

### Why mounted > env for secrets

Env vars leak through channels files don't:

- `kubectl describe pod` / `kubectl get pod -o yaml` shows env (with secretKeyRef indirection — but plenty of teams paste literals),
- `docker inspect`/`crictl inspect` on the node prints the resolved environment,
- crash handlers, error trackers, and "dump env on startup" debug logging exfiltrate the whole environment — podlab's own `/config` endpoint happily prints every env var, which today becomes the *demonstration of the anti-pattern*,
- child processes inherit env wholesale.

A tmpfs-mounted file leaks only to processes that read that path. Default to files; use env only when a library leaves no choice.

### Projected volumes

Yesterday's problem in waiting: a `configMap` volume *owns* its mountPath — you can't also mount a `secret` volume at `/etc/podlab` (two volumes, one dir → collision; subPath would fix placement but freeze updates). A **projected** volume composes multiple sources (configMap, secret, downwardAPI, serviceAccountToken) into **one** directory, updates intact. It's how the auto-mounted service account token at `/var/run/secrets/kubernetes.io/serviceaccount` works, and it's today's core lab.

## Lab

Day 6 left the `podlab` Deployment running with `podlab-env` (envFrom) and your files ConfigMap (`podlab-files-v2`) mounted at `/etc/podlab`.

### 1. Create secrets, see the encoding

```sh
kubectl create secret generic db-creds \
  --from-literal=username=app \
  --from-literal=password='s3cr3t-hunter2'
kubectl get secret db-creds -o yaml
```

Note `type: Opaque` and base64 values. Now the one-command proof that this is not encryption:

```sh
kubectl get secret db-creds -o jsonpath='{.data.password}' | base64 -d; echo
```

`s3cr3t-hunter2`. Anyone with read access to the object has the credential — RBAC *is* the security boundary. The declarative form with `stringData` (don't commit files like this — Day 28):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: db-creds
type: Opaque
stringData:
  username: app
  password: s3cr3t-hunter2
```

### 2. Core lab: projected volume — ConfigMap + Secret, one directory

Requirements — modify the Deployment's pod template:

- replace the `configMap` volume from Day 6 with a single **projected** volume named `config`
- it combines: ConfigMap `podlab-files-v2` (as before) **and** Secret `db-creds`, with the secret's keys placed under a `creds/` subdirectory (use `items` with `path: creds/username` etc.)
- still mounted at `/etc/podlab`; everything else unchanged

<details><summary>Solution</summary>

```yaml
      volumes:
        - name: config
          projected:
            sources:
              - configMap:
                  name: podlab-files-v2
              - secret:
                  name: db-creds
                  items:
                    - key: username
                      path: creds/username
                    - key: password
                      path: creds/password
```

…and the container's mount becomes:

```yaml
          volumeMounts:
            - name: config
              mountPath: /etc/podlab
```

</details>

```sh
kubectl apply -f deploy.yaml
kubectl rollout status deployment/podlab
kubectl port-forward svc/podlab 8081:80 &
curl -s localhost:8081/config | python3 -m json.tool
```

The `files` block now shows **both** sources in one tree: `/etc/podlab/app-settings.yaml` (ConfigMap) and `/etc/podlab/creds/username`, `/etc/podlab/creds/password` (Secret, decoded — files always hold the raw value). One directory, two objects, independently updatable. Update the secret and watch it propagate like a mounted ConfigMap:

```sh
kubectl patch secret db-creds -p '{"stringData":{"password":"rotated-pw-1"}}'
watch -n 5 'curl -s localhost:8081/config | grep -o "rotated[^\"]*"'   # appears within ~1 min
```

### 3. The env anti-pattern, demonstrated

Add to the container (temporarily):

```yaml
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: db-creds
                  key: password
```

Apply, wait for rollout, then:

```sh
curl -s localhost:8081/config | grep -o '"DB_PASSWORD":"[^"]*"'
```

There's your secret, **in an HTTP response**, because one debug endpoint dumps the environment — exactly the class of leak that happens with crash reporters and verbose logging in real apps. The mounted copy under `creds/` appears too, of course — but only because podlab deliberately dumps CONFIG_DIR; a generic env-dumper catches every env secret of every library *by default*. Also check the node's view:

```sh
docker exec course-worker crictl inspect $(docker exec course-worker crictl ps --name podlab -q | head -1) | grep -A1 DB_PASSWORD
```

Anyone with node access reads it without touching the Kubernetes API. Remove the `DB_PASSWORD` env block and re-apply.

### 4. imagePullSecrets

Private registries reject anonymous pulls; pods fail with `ErrImagePull`/`ImagePullBackOff`. The fix is a `dockerconfigjson` secret referenced from the pod:

```sh
kubectl create secret docker-registry regcred \
  --docker-server=ghcr.io \
  --docker-username=youruser \
  --docker-password=fake-token-for-now \
  --docker-email=you@example.com
kubectl get secret regcred -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d; echo
```

That's literally a `~/.docker/config.json`. Wiring it (no private registry today, so this is a dry run):

```yaml
spec:
  imagePullSecrets:
    - name: regcred
  containers: [...]
```

The kubelet presents those credentials when pulling that pod's images. (Per-namespace default: attach it to the ServiceAccount — `kubectl patch serviceaccount default -p '{"imagePullSecrets":[{"name":"regcred"}]}'` — every pod using that SA inherits it.)

### 5. Who can read secrets? (RBAC preview)

```sh
kubectl auth can-i get secrets                                  # you: yes (kind admin)
kubectl auth can-i get secrets --as=system:serviceaccount:default:default
kubectl auth can-i list secrets --as=system:serviceaccount:default:default
```

`no` and `no` — the default ServiceAccount your pods run as cannot read secrets via the API, which is exactly right: pods get secrets through *mounts the pod spec requests*, not API reads. Day 13 builds full Roles/Bindings; today's takeaway is just that this boundary exists and you can interrogate it with `kubectl auth can-i --as`.

## Verify ✅

- [ ] `kubectl get secret db-creds -o jsonpath='{.data.password}' | base64 -d` → the current plaintext password
- [ ] `curl -s localhost:8081/config | python3 -m json.tool` → `files` contains `/etc/podlab/app-settings.yaml` **and** `/etc/podlab/creds/password` (projected volume works)
- [ ] After `kubectl patch secret db-creds …`: the mounted file shows the new value within ~1 min, `RESTARTS 0`
- [ ] With the env experiment reverted: `curl -s localhost:8081/config | grep DB_PASSWORD` → no match
- [ ] `kubectl get secret regcred -o jsonpath='{.type}'` → `kubernetes.io/dockerconfigjson`
- [ ] `kubectl auth can-i list secrets --as=system:serviceaccount:default:default` → `no`

## CKA corner 🎓

Exam notes:

- The three imperative forms, cold: `kubectl create secret generic`, `... secret tls NAME --cert=f.crt --key=f.key`, `... secret docker-registry NAME --docker-server= --docker-username= --docker-password=`.
- Decode fast: `kubectl get secret X -o jsonpath='{.data.KEY}' | base64 -d`. Keys with dots need escaping: `{.data.\.dockerconfigjson}`.
- Secret consumption YAML is ConfigMap YAML with `secretKeyRef`/`secretRef`/`secret:` swapped in — don't relearn it.

**Drill 1 (3 min):** Generate a self-signed cert (`openssl req -x509 -newkey rsa:2048 -keyout drill.key -out drill.crt -days 1 -nodes -subj "/CN=drill.local"`), create a TLS secret `drill-tls` from it, and prove the type and that `tls.crt` round-trips through base64.

<details><summary>Solution</summary>

```sh
openssl req -x509 -newkey rsa:2048 -keyout drill.key -out drill.crt -days 1 -nodes -subj "/CN=drill.local"
kubectl create secret tls drill-tls --cert=drill.crt --key=drill.key
kubectl get secret drill-tls -o jsonpath='{.type}'; echo                  # kubernetes.io/tls
kubectl get secret drill-tls -o jsonpath='{.data.tls\.crt}' | base64 -d | head -2
kubectl delete secret drill-tls && rm drill.key drill.crt
```

</details>

**Drill 2 (2 min):** Create docker-registry secret `pullcred` for server `registry.example.com`, user `ci-bot`, password `tok123`; then extract the server name back out of `.dockerconfigjson` with one pipeline.

<details><summary>Solution</summary>

```sh
kubectl create secret docker-registry pullcred \
  --docker-server=registry.example.com --docker-username=ci-bot --docker-password=tok123
kubectl get secret pullcred -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d | python3 -m json.tool
kubectl delete secret pullcred
```

</details>

**Drill 3 (3 min):** Pod `vault` (busybox, sleep 3600) with secret `db-creds` mounted read-only at `/secrets`. Verify the password file's content via exec, fast.

<details><summary>Solution</summary>

```sh
kubectl run vault --image=busybox --dry-run=client -o yaml --command -- sleep 3600 > vault.yaml
```

Add:

```yaml
  volumes:
    - name: s
      secret: {secretName: db-creds}
  containers:
    - ...
      volumeMounts: [{name: s, mountPath: /secrets, readOnly: true}]
```

```sh
kubectl apply -f vault.yaml && sleep 5
kubectl exec vault -- cat /secrets/password; echo
kubectl delete pod vault
```

</details>

## Stretch goals

- Inspect the auto-mounted SA token: `kubectl debug -it deploy/podlab --image=busybox --target=podlab`, then `cat /var/run/secrets/kubernetes.io/serviceaccount/token` — paste it into jwt.io's debugger and read the claims. Then set `automountServiceAccountToken: false` and confirm the directory disappears.
- Find the secret's bytes in etcd ahead of Day 16: `docker exec course-control-plane sh -c 'ETCDCTL_API=3 etcdctl --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key get /registry/secrets/default/db-creds' | strings | grep rotated` — plaintext at rest.
- Make `db-creds` immutable and attempt a patch — same mechanics as Day 6, same versioned-name escape hatch.

## Cleanup

```sh
kill %1 2>/dev/null
kubectl delete secret regcred
```

**Keep:** the `podlab` Deployment with the projected volume, and the `db-creds` secret — Day 8 adds resources/limits to this same Deployment. (`db-creds` also returns as the sealed-secrets example on Day 28.)
