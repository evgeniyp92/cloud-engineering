# Day 29 — The Secrets Landscape: ESO, Vault, SOPS, and Choosing

> **Time:** ~3.5 h · **Builds on:** Days 7, 28

## Objectives

- Map the four families of Kubernetes secret management and the decision dimensions that separate them — rotation ownership, key-loss blast radius, multi-cluster, audit, dev ergonomics.
- Run **External Secrets Operator** against a dev-mode **Vault** and prove a secret rotates into podlab *without a git commit*.
- Encrypt and decrypt a values file with **SOPS + age**, and articulate why its ArgoCD integration is the friction point.
- Pick a strategy for a given team/cloud and defend it — the actual interview skill.

## Concepts

### Four families

Yesterday you mastered one point in a larger design space. The whole map:

| Family | Examples | Where the secret lives | What's in git |
|---|---|---|---|
| 1. Encrypted-in-git | sealed-secrets, SOPS | git (ciphertext) | the ciphertext itself |
| 2. Reference-an-external-store | External Secrets Operator, secrets-store CSI driver | Vault / AWS SM / GCP SM / Azure KV | a *pointer* (store + path + key) |
| 3. Cloud-native managers | AWS Secrets Manager + IRSA, GCP SM + Workload Identity, Azure KV | the cloud service | a pointer; auth is *ambient* (pod identity → cloud IAM, no credential to bootstrap) |
| 4. Runtime injection | Vault Agent injector/sidecar, Vault CSI | Vault | an annotation; secret goes straight into the pod filesystem, **never becomes a k8s Secret** |

Family 1 you know. Family 2 inverts it: git holds no secret material at all, just a reference; an in-cluster operator fetches the real value from a store and materializes a normal k8s Secret, re-fetching on an interval. Family 3 is family 2 with the store being your cloud's manager and authentication solved by workload identity instead of a stored token — the production default on EKS/GKE/AKS. Family 4 bypasses k8s Secrets entirely (they're only base64-in-etcd, remember), trading app-transparency for the strongest runtime story: short-lived dynamic credentials, in-memory delivery.

### The decision dimensions

- **Who owns rotation?** Sealed-secrets: a human, via re-seal + commit (audited, but pull-by-human — a 90-day-rotation policy means a human remembers, or CI does). ESO: the *store* owns the value; rotation propagates automatically within `refreshInterval`. Vault injector: Vault can mint *dynamic* short-lived credentials — rotation stops being an event at all.
- **Blast radius of key loss.** Sealed-secrets: lose the sealing key → repo ciphertext is dead (Day 28's drill). SOPS: lose the age/KMS key → same. ESO: nothing to lose in-cluster — the store is the source of truth and has its own DR; but lose the *store* and every cluster goes down with it.
- **Multi-cluster.** Sealed-secrets keys are per-cluster — N clusters means N keys, N backups, N seals per secret (or risky key-sharing). ESO: N clusters point at one store; secrets are written once. This dimension alone pushes most multi-cluster shops to family 2/3.
- **Audit.** Encrypted-in-git: *who changed what* is `git log` — superb; *who read it* is invisible. Vault/cloud managers: every read is an access-log line — superb; the change story lives outside git.
- **Dev ergonomics / bootstrap.** Sealed-secrets: one controller, one CLI, works on a laptop kind cluster offline. ESO needs a store to exist (bootstrap problem: where does the store's own credential live? In cloud: ambient identity. Self-hosted Vault: you've adopted a *second* stateful HA system with its own unseal/DR story — substantial operational weight).

### Where SOPS sits

[SOPS](https://github.com/getsops/sops) encrypts *values inside* a YAML/JSON file, leaving keys readable — so an encrypted values file still diffs meaningfully in PRs (you see *which* key changed, not what it became). Keys can be age (modern, simple file keys), PGP, or cloud KMS (the team-friendly option: decryption right = KMS IAM permission, no key file to pass around). It's beloved for Helm values and Terraform vars. Its weakness in *this* course's architecture: ArgoCD has no native SOPS support — decryption must hook into the render step via a Config Management Plugin (KSOPS for Kustomize, helm-secrets for Helm), which means patching the repo-server with sidecars/plugins and giving it the private key. Works, widely used (Flux, by contrast, supports SOPS natively), but it's the kind of glue you should *choose*, not stumble into.

### The contrast you'll prove today

The single most clarifying difference between families 1 and 2:

> **Sealed-secrets: rotation is a git commit. ESO: rotation is a write to the store — no commit, git never changes, the cluster updates itself.**

Both are "GitOps-compatible," but they draw the source-of-truth boundary differently: family 1 says git holds *everything*; family 2 says git holds *topology* and the store holds *values*. Neither is wrong; the table at the end is how you choose.

## Lab

### Part 1 — External Secrets Operator + Vault

#### 1. Install Vault (dev mode) and ESO

Dev mode runs Vault in-memory, auto-unsealed, root token `root` — perfect for a lab, a firing offense in prod (no persistence, no seal, known token). Helm-direct installs are fine here; both *could* be `argocd/apps/` files, and the stretch goal does exactly that.

```sh
helm repo add hashicorp https://helm.releases.hashicorp.com
helm repo add external-secrets https://charts.external-secrets.io
helm repo update

helm install vault hashicorp/vault -n vault --create-namespace \
  --set server.dev.enabled=true
helm install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace

kubectl get pods -n vault; kubectl get pods -n external-secrets
```

Wait for `vault-0` (a StatefulSet — Vault is stateful even when dev mode throws the state away) and the three ESO deployments (controller, webhook, cert-controller).

#### 2. Put a secret in Vault

```sh
kubectl exec -n vault vault-0 -- vault kv put secret/podlab api-key=from-vault-123
kubectl exec -n vault vault-0 -- vault kv get secret/podlab
```

Dev mode pre-mounts a KV **v2** engine at `secret/`. That value now exists *only* in Vault's memory — it will never touch git.

#### 3. A namespace, a token, a SecretStore

Today's demo lives in a disposable namespace, outside ArgoCD — partly to keep it self-contained, partly because hand-editing the ArgoCD-managed podlab would just get selfHealed away (Day 25 taught you why):

```sh
kubectl create namespace secrets-lab
kubectl create secret generic vault-token -n secrets-lab --from-literal=token=root
```

(Yes — the store's *own* credential is a plain k8s Secret. That's the family-2 bootstrap problem in miniature; in cloud, workload identity dissolves it, and with in-cluster Vault you'd use its Kubernetes auth method instead of a token. Stretch goal.)

Now write `secretstore.yaml` — the "how to reach the store" half. Requirements: `kind: SecretStore` (namespaced flavor; `ClusterSecretStore` is the shared-across-namespaces variant), name `vault`, ns `secrets-lab`, provider vault with `server: http://vault.vault:8200`, `path: secret`, `version: v2`, token auth from the `vault-token` Secret.

<details><summary>Solution</summary>

```yaml
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: vault
  namespace: secrets-lab
spec:
  provider:
    vault:
      server: http://vault.vault:8200
      path: secret
      version: v2
      auth:
        tokenSecretRef:
          name: vault-token
          key: token
```

(If your ESO version rejects `external-secrets.io/v1`, it's older — use `external-secrets.io/v1beta1`.)

</details>

```sh
kubectl apply -f secretstore.yaml
kubectl get secretstore -n secrets-lab    # STATUS: Valid, READY: True
```

#### 4. The ExternalSecret

The "what to fetch" half. Write `externalsecret.yaml`: name `podlab-vault`, ns `secrets-lab`, `refreshInterval: 15s` (lab-fast; minutes in real life), store ref `vault`, target Secret name `podlab-vault`, one data entry mapping secretKey `api-key` ← remoteRef key `podlab`, property `api-key`.

<details><summary>Solution</summary>

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: podlab-vault
  namespace: secrets-lab
spec:
  refreshInterval: 15s
  secretStoreRef:
    name: vault
    kind: SecretStore
  target:
    name: podlab-vault
  data:
    - secretKey: api-key
      remoteRef:
        key: podlab
        property: api-key
```

</details>

```sh
kubectl apply -f externalsecret.yaml
kubectl get externalsecret -n secrets-lab          # SecretSynced / True
kubectl get secret podlab-vault -n secrets-lab -o jsonpath='{.data.api-key}' | base64 -d; echo
```

`from-vault-123` — ESO fetched from Vault and materialized an ordinary Secret. The two YAMLs you just wrote contain **zero secret material**; both could sit in your public repo right now.

#### 5. Mount it into podlab and prove rotation-without-commit

Run a standalone podlab in `secrets-lab` mounting that Secret at `/etc/podlab` (plain Deployment, image `podlab:v1`, volume from secret `podlab-vault` — you've written this deployment shape ten times, no solution block), plus a quick port-forward:

```sh
kubectl apply -f podlab-eso.yaml
kubectl port-forward -n secrets-lab deploy/podlab-eso 8089:8080 &
curl -s localhost:8089/config | python3 -m json.tool | grep -A1 api-key
```

`/etc/podlab/api-key` → `from-vault-123`. Now the headline demo — change the value **in Vault only**:

```sh
kubectl exec -n vault vault-0 -- vault kv put secret/podlab api-key=rotated-in-vault-456
date    # start your stopwatch
```

Within `refreshInterval` (15s) ESO rewrites the Secret; within the kubelet's sync period (~1 min) the mounted file updates; then:

```sh
while sleep 5; do curl -s localhost:8089/config | grep -o 'rotated-in-vault-456' && break; echo waiting...; done
```

When it flips: **no git commit happened.** `git -C ~/Code/k8s-gitops status` is clean, `git log` has no new entry — compare with Day 28 step 6, where the same rotation *was* a commit. You've now felt both sides of the source-of-truth boundary. (Also note what you gave up: yesterday's rotation had a reviewable diff and a revert button; this one has Vault's audit log and no PR.) Kill the port-forward: `kill %1`.

### Part 2 — A taste of SOPS

#### 6. Keys and encryption

```sh
brew install sops age
mkdir -p ~/.config/sops/age && age-keygen -o ~/.config/sops/age/keys.txt
grep 'public key' ~/.config/sops/age/keys.txt    # age1...
```

Make a plausible secret values file and encrypt it (use *your* age1… key):

```sh
cd /tmp
cat > prod-secrets.yaml <<'EOF'
apiKey: sk-prod-deadbeef
database:
  password: hunter2
  host: db.internal.example.com
EOF
sops --encrypt --age age1YOURKEY prod-secrets.yaml > prod-secrets.enc.yaml
cat prod-secrets.enc.yaml
```

Study the output — this is SOPS's whole pitch in one screen: `apiKey:` and `database.password:` **keys still readable**, every value replaced by `ENC[AES256_GCM,...]`, plus a `sops:` metadata block recording the age recipient and a MAC (tamper detection). A PR diff of this file shows *which* secret changed — something neither a SealedSecret blob nor a Vault write gives you. Note `host` got encrypted too: SOPS encrypts all values by default (`--encrypted-regex '^(apiKey|password)$'` for selective encryption via `.sops.yaml` rules).

#### 7. Decrypt, and weigh the ArgoCD friction

```sh
sops --decrypt prod-secrets.enc.yaml          # finds the key in ~/.config/sops/age automatically
sops prod-secrets.enc.yaml                     # opens $EDITOR decrypted; re-encrypts on save
rm prod-secrets*.yaml
```

That `sops file.enc.yaml` edit-in-place flow is why developers love it. Now the catch for *your* stack: who decrypts at deploy time? ArgoCD's repo-server runs `helm template`/`kustomize build` — neither speaks SOPS. You'd bolt on a Config Management Plugin (KSOPS for Kustomize, helm-secrets for Helm), patch the repo-server pod with the plugin sidecar, and hand it the age private key. Compare: sealed-secrets needed nothing (the controller watches a CRD) and ESO needed nothing (it runs beside ArgoCD, not inside it). SOPS shines where the renderer is yours — Flux (native support), CI pipelines, Terraform — and costs real glue in stock ArgoCD.

### Part 3 — Choose

#### 8. The comparison table

| | sealed-secrets | SOPS (+age/KMS) | ESO (+store) | Vault injector |
|---|---|---|---|---|
| Value rotation | human re-seals + commits | human re-edits + commits | store-side, auto-propagates (`refreshInterval`) | dynamic/short-lived — rotation built-in |
| Git visibility | opaque blob per secret | keys visible, values ENC — meaningful diffs | reference only — no secret material at all | annotation only |
| DR / key loss | lose sealing key ⇒ repo ciphertext dead; **you** back up the key | lose age/KMS key ⇒ same; KMS makes it the cloud's problem | nothing in-cluster to lose; store's DR is the story | Vault's DR (raft snapshots, unseal keys) — serious ops |
| Multi-cluster | per-cluster keys, per-cluster seals | one key, any cluster | one store, N clusters — the clean answer | one Vault, N clusters |
| ArgoCD friction | none (CRD + controller) | **high** (repo-server plugin + key) | none (independent operator) | none for ArgoCD, but app pods get sidecars/annotations |
| Becomes a k8s Secret? | yes | yes (after render) | yes | **no** (tmpfs in pod) — strongest runtime posture |
| Operational weight | one tiny controller | a CLI + key custody | operator + **a store you must run or buy** | a stateful HA Vault cluster |

#### 9. A recommendation framework

- **Solo dev / small team, single cluster, no cloud:** sealed-secrets. One controller, offline-friendly, rotation-by-commit is fine at this scale. *(This course's choice — and Day 48 relies on it.)*
- **Small team on one cloud:** ESO + the cloud's secrets manager, auth via workload identity. Nothing self-hosted to babysit, rotation and read-audit come from the cloud, multi-cluster is free when you grow.
- **Platform team, many clusters/tenants:** ESO + central store (cloud SM or managed Vault). Per-cluster sealing keys don't scale; write-once-deploy-everywhere does.
- **Compliance-heavy / dynamic credentials (DB creds, short-lived tokens):** Vault with injector/CSI — the only family where credentials can be *born* short-lived. Budget a person for Vault itself.
- **SOPS** is the right answer when the secret consumer is *not* the cluster (Terraform, CI, app config delivered by Flux) or when meaningful PR diffs of secret files matter more than ArgoCD-nativeness.

## Verify ✅

- [ ] `kubectl get secretstore,externalsecret -n secrets-lab` → both Ready/SecretSynced `True`
- [ ] `curl -s localhost:8089/config | grep rotated-in-vault-456` (port-forward up) → match, **and** `git -C ~/Code/k8s-gitops log --oneline -1` is still yesterday's commit — rotation happened with no commit
- [ ] `kubectl exec -n vault vault-0 -- vault kv get -field=api-key secret/podlab` → `rotated-in-vault-456` — the store is the source of truth
- [ ] `sops --decrypt` round-trips your test file, and you can point at the `sops:` metadata block and the readable keys in the encrypted version
- [ ] You can reproduce the comparison table's rotation row and ArgoCD-friction row from memory — those two rows decide most real choices
- [ ] Day 28's sealed-secrets path still works: `curl -s http://podlab-helm.localhost:8080/config | grep r0tated`

## Interview corner 💬

**"Compare sealed-secrets and External Secrets Operator — when would you pick each?"** They differ on where truth lives. Sealed-secrets keeps the encrypted value *in git*: zero extra infrastructure, perfect change-audit via commits, but rotation is manual, keys are per-cluster, and you own backing up the sealing key or your repo's ciphertext dies with the cluster. ESO keeps values in an external store and git holds only references: rotation propagates automatically on a refresh interval, multi-cluster is write-once, read-audit comes from the store — but now you run or rent the store, and you've split your source of truth (git for topology, store for values). My rule: single cluster + small team + no cloud account → sealed-secrets; anything multi-cluster or already-on-cloud → ESO with the cloud's secret manager and workload identity. I'd also flag the failure modes inverted: sealed-secrets' nightmare is key loss, ESO's is the store being down or the bootstrap credential leaking.

**"Your auditor asks who accessed the prod DB password last quarter. Which architectures can answer?"** Encrypted-in-git can't — git logs writes, not reads, and once unsealed it's a k8s Secret readable by anything RBAC allows. Vault and cloud secret managers log every read with an identity. If read-audit is a hard requirement, you're in family 2/3/4 by elimination — strongest in family 4, where the secret never even becomes a k8s Secret.

## Stretch goals

- GitOps-ify part 1: move Vault and ESO under `argocd/apps/` (wave 0) — Helm-chart-type Applications exactly like sealed-secrets on Day 28. Decide whether `secrets-lab`'s SecretStore/ExternalSecret belong in git too (they're secret-free — yes).
- Replace token auth with Vault's **Kubernetes auth method**: enable it in vault-0, create a role bound to a ServiceAccount in `secrets-lab`, and point the SecretStore's `auth.kubernetes` at it — the bootstrap-credential problem dissolved properly.
- Try a `ClusterSecretStore` plus an ExternalSecret in a second namespace — the multi-tenant shape.
- Wire KSOPS into ArgoCD's repo-server for real (patch in the sidecar, mount the age key) and deploy one SOPS-encrypted Kustomize secret — then decide whether the table's "high friction" rating was fair.
- Read about ESO `PushSecret` — syncing the *other* direction, cluster → store.

## Cleanup

```sh
kill %1 2>/dev/null                                   # any stray port-forward
kubectl delete namespace secrets-lab
helm uninstall vault -n vault && kubectl delete ns vault
helm uninstall external-secrets -n external-secrets && kubectl delete ns external-secrets
rm -f /tmp/prod-secrets*.yaml
```

(Or keep ESO+Vault if you did the GitOps-ify stretch — they're light.) **What MUST stay: ArgoCD, the whole `argocd/apps/` tree, the sealed-secrets controller, and `~/sealed-secrets-key-backup.yaml`** — sealed-secrets is the course's primary mechanism and the Day 48 bootstrap decrypts with that backed-up key. The age key in `~/.config/sops/age/` can stay; it's 100 bytes. Phase 4 complete — Day 30 starts wiring eyes onto all of this with Prometheus.
