# Day 28 — Sealed Secrets: Encrypted Secrets in a Public Repo

> **Time:** ~3 h · **Builds on:** Days 7, 24–26

## Objectives

- State the GitOps secrets problem precisely and explain how sealed-secrets' asymmetric-crypto design lets ciphertext live in a **public** repo.
- Install the controller **via ArgoCD** (wave 0 — GitOps manages its own secret machinery) and seal a real secret with `kubeseal`.
- Ship a secret to podlab through git and prove via `/config` that the plaintext never existed anywhere but your terminal and the cluster.
- Run the **DR drill** everyone skips: back up the sealing key, and explain exactly what dies without it.

## Concepts

### The hole in GitOps

Day 7's punchline: a Kubernetes Secret is base64, not encryption — `echo <blob> | base64 -d` is the whole attack. For three weeks that was survivable because secrets stayed in the cluster. But the GitOps contract you've built since Day 24 says *everything* is in git, and your repo is public. Commit a Secret manifest and you've published the credential; even in a private repo, everyone with read access, every laptop clone, and every CI cache now holds it, forever (git never forgets — scrubbing history is an incident-response procedure, not an undo button).

So GitOps teams need one of two strategies: **encrypt secrets so git can hold them safely** (today), or **keep secrets elsewhere and store only references in git** (Day 29). Sealed-secrets is the canonical implementation of the first.

### How sealed-secrets works

Asymmetric crypto with the key pair living where the secrets are used:

```
 your laptop                              cluster
 ───────────                              ───────
 secret.yaml (plaintext, NEVER committed)
      │
      ▼
 kubeseal ──fetches──────────────▶ controller (kube-system)
      │       PUBLIC cert                holds PRIVATE key
      ▼
 SealedSecret YAML  ──git──▶ ArgoCD ──▶ SealedSecret resource
   (ciphertext, safe                          │ controller decrypts
    anywhere, even                            ▼
    a public repo)                       ordinary Secret ──▶ pod mounts it
```

- `kubeseal` encrypts each value with the controller's **public** certificate. Encryption needs no privileges and can run anywhere — laptops, CI.
- Only the in-cluster controller holds the **private** key. The SealedSecret CRD instance is harmless in transit and at rest; the moment it lands in the cluster, the controller unseals it into a normal Secret (owned by the SealedSecret, so deleting the SealedSecret deletes the Secret).
- Pods are oblivious — they mount a plain Secret. No SDK, no sidecar, no app changes. That's the feature that makes sealed-secrets the lowest-friction option on Day 29's comparison table.

The private key is therefore the crown jewel *and* the single point of failure — hold that thought for the DR section.

### Scopes: why ciphertext is bound to name + namespace

If ciphertext were freely reusable, anyone with `kubectl create` rights in *any* namespace could replay your prod SealedSecret there, mount the resulting Secret, and read it — no key theft needed. Sealed-secrets blocks this by mixing scope into the encryption:

| Scope | Ciphertext valid for | Use when |
|---|---|---|
| **strict** (default) | exactly this name **and** namespace | almost always — replay anywhere else fails to decrypt |
| `namespace-wide` | any name within one namespace | secret needs renaming flexibility |
| `cluster-wide` | anywhere in the cluster | shared infra secrets; weakest — any namespace admin can replay it |

Strict means re-sealing when a secret moves namespaces — mildly annoying, deliberately so. Default to strict; treat the others as documented exceptions.

### Key rotation and the two meanings of "rotate"

Two different things people call rotation:

1. **Rotating the secret value** (new API key): create the new plaintext, re-seal, commit. Rotation = a git commit, fully audited — but note it's *pull-by-human*: nothing automates it (Day 29's ESO contrast).
2. **Sealing-key renewal**: the controller mints a *new* key pair every 30 days. Old keys are kept and still decrypt old SealedSecrets; new sealing uses the new cert. `kubeseal --re-encrypt` rewrites existing files under the newest key without exposing plaintext — run it occasionally so ancient keys can eventually be retired.

## Lab

### 1. Install the controller — as an ArgoCD app, wave 0

Secret machinery is platform infrastructure: other apps depend on it, so it belongs in wave 0 next to metrics-server. Find the current chart version first:

```sh
helm repo add sealed-secrets https://bitnami-labs.github.io/sealed-secrets
helm repo update && helm search repo sealed-secrets/sealed-secrets
```

Write `~/Code/k8s-gitops/argocd/apps/sealed-secrets.yaml`. Requirements:

- Application `sealed-secrets`, ns `argocd`, sync-wave `"0"`, **no finalizer** (same infra-protection logic as metrics-server — a mis-prune must not delete the thing that decrypts your secrets)
- source: `repoURL: https://bitnami-labs.github.io/sealed-secrets`, `chart: sealed-secrets`, `targetRevision:` pinned to the version you just found
- `helm.valuesObject` with `fullnameOverride: sealed-secrets-controller` — the chart's default name is `sealed-secrets`, but the `kubeseal` CLI looks for a controller named `sealed-secrets-controller`; the override saves you typing `--controller-name` forever
- destination ns `kube-system`; automated + selfHeal

<details><summary>Solution</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: sealed-secrets
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: default
  source:
    repoURL: https://bitnami-labs.github.io/sealed-secrets
    chart: sealed-secrets
    targetRevision: 2.17.x        # ← pin what `helm search repo` showed
    helm:
      valuesObject:
        fullnameOverride: sealed-secrets-controller
  destination:
    server: https://kubernetes.default.svc
    namespace: kube-system
  syncPolicy:
    automated:
      selfHeal: true
```

</details>

```sh
cd ~/Code/k8s-gitops
git add argocd/apps/sealed-secrets.yaml && git commit -m "sealed-secrets controller (wave 0)" && git push
argocd app get root --refresh && argocd app list | grep sealed
kubectl get pods -n kube-system -l app.kubernetes.io/name=sealed-secrets
```

Appreciate what just happened: you added secret-decryption infrastructure to the cluster with a `git push`. On Day 48 this file is why the rebuilt cluster can decrypt anything.

### 2. Install kubeseal and meet the key

```sh
brew install kubeseal
kubeseal --fetch-cert    # the PUBLIC cert, fetched from the controller — this is all kubeseal needs
kubectl get secret -n kube-system -l sealedsecrets.bitnami.com/sealed-secrets-key
```

That labeled Secret is the private key. Look at it now so the DR drill in step 6 isn't abstract.

### 3. Seal a real secret

Build the plaintext Secret **offline** — `--dry-run=client` never sends it to the cluster. Two payloads, matching how podlab consumes config: a key-value and a file:

```sh
cd ~/Code/k8s-gitops
cat > /tmp/secret.conf <<'EOF'
db_password = hunter2-but-sealed
feature_flag_token = ff-9c1a
EOF
kubectl create secret generic podlab-secrets -n argocd-podlab \
  --from-literal=api-key=sk-live-c0ffee1234 \
  --from-file=secret.conf=/tmp/secret.conf \
  --dry-run=client -o yaml > podlab-secrets-unsealed.yaml
```

The `-unsealed` suffix is the convention guarding you: confirm `.gitignore` covers `*-unsealed.yaml` (add it if your repo predates the rule) — `git check-ignore podlab-secrets-unsealed.yaml` must print the filename. Note the `-n argocd-podlab` on a dry-run: the namespace ends up *in the YAML*, and strict scope will bake it into the ciphertext — seal for the wrong namespace and the controller can't unseal it.

Now seal, straight into the podlab chart:

```sh
kubeseal --format yaml \
  < podlab-secrets-unsealed.yaml \
  > charts/podlab/templates/podlab-sealedsecret.yaml
rm podlab-secrets-unsealed.yaml /tmp/secret.conf       # plaintext: terminated
cat charts/podlab/templates/podlab-sealedsecret.yaml
```

Read it: `kind: SealedSecret`, your key names visible, values replaced by ~300 chars of ciphertext. This file is safe in a public repo. (It contains no `{{ }}`, so Helm passes it through untouched — fine to live in `templates/`. The encoded namespace also means this chart's SealedSecret only works for the `podlab-helm` app, not the Day 27 env apps — strict scope doing its job; sealing per-env copies is the pattern for those.)

### 4. Mount it into podlab

Edit your chart's deployment template: add a volume for Secret `podlab-secrets` mounted at **`/etc/podlab/secret`** — a subdirectory of `CONFIG_DIR`, because podlab's `/config` walks that tree recursively, which makes the proof trivial. (Mounting one volume inside another volume's mountPath is fine — they're separate mounts.)

<details><summary>Solution (adapt names to your Day 20 chart)</summary>

```yaml
# in the pod spec:
      volumes:
        # ...your existing config volume...
        - name: sealed
          secret:
            secretName: podlab-secrets
# in the container:
        volumeMounts:
          # ...existing mount at /etc/podlab...
          - name: sealed
            mountPath: /etc/podlab/secret
            readOnly: true
```

</details>

Ship it:

```sh
git add charts/podlab && git commit -m "podlab: sealed secret mounted at /etc/podlab/secret" && git push
argocd app get podlab-helm --refresh   # wait for Synced/Healthy (pods roll for the new mount)
```

### 5. The proof

```sh
kubectl get sealedsecret,secret -n argocd-podlab | grep podlab-secrets
curl -s http://podlab-helm.localhost:8080/config | python3 -m json.tool
```

`/config` now lists `/etc/podlab/secret/api-key` → `sk-live-c0ffee1234` and `/etc/podlab/secret/secret.conf` with the full file. Sit with this: that value travelled laptop → git (public!) → ArgoCD → cluster, and was plaintext only at the two ends. Then verify git's cleanliness — history included:

```sh
cd ~/Code/k8s-gitops
git grep sk-live $(git rev-list --all) || echo "plaintext never committed ✔"
git log --all --diff-filter=A --name-only | grep -i unsealed || echo "no unsealed files ever ✔"
```

### 6. Rotate a value end-to-end

The api-key leaked (hypothetically). Rotate it — note this is the full procedure, there's no shortcut around re-sealing:

```sh
kubectl create secret generic podlab-secrets -n argocd-podlab \
  --from-literal=api-key=sk-live-r0tated99 \
  --from-literal=secret.conf="db_password = hunter3" \
  --dry-run=client -o yaml \
  | kubeseal --format yaml > charts/podlab/templates/podlab-sealedsecret.yaml
git add -A && git commit -m "rotate podlab api-key" && git push
```

After sync, `/config` shows the new value (Secret *mounts* propagate in ~a minute without a pod restart; ArgoCD may roll pods anyway depending on your chart). Audit trail: the rotation is a commit — diffable ciphertext, reviewable, revertable. Also know `kubeseal --re-encrypt < sealed.yaml > sealed2.yaml`: same plaintext, newest sealing key, no decryption on your machine — run it after key renewals.

### 7. ⚠️ The DR drill — do not skip

Question: your kind cluster dies tonight. The repo has everything… except the **private key lives only in the cluster**. New cluster + new controller = new key pair = every SealedSecret in git is permanently undecryptable, and you re-seal everything from plaintext you hopefully still have. The backup is one command:

```sh
kubectl get secret -n kube-system -l sealedsecrets.bitnami.com/sealed-secrets-key \
  -o yaml > ~/sealed-secrets-key-backup.yaml
grep -c 'tls.key' ~/sealed-secrets-key-backup.yaml   # ≥1 — the private key(s) are in there
```

**Store this file OUTSIDE git** — it *is* the plaintext of every secret you'll ever seal; committing it would undo the entire day. `~` on your Mac is fine for the course; real teams put it in a vault/KMS with break-glass access. **Keep this exact file: Day 48 rebuilds the cluster from scratch and restores this key** (`kubectl apply -f` the backup *before* the controller starts, or restart the controller after) so every SealedSecret in your repo decrypts on the new cluster. No file, no capstone secrets — you'd be re-sealing from memory.

Walk the disaster table once out loud:

| Scenario | Outcome |
|---|---|
| cluster lost, key backup exists | restore key Secret → controller loads it → all SealedSecrets unseal. Full recovery. |
| cluster lost, no backup | ciphertext in git is garbage; re-seal everything from original plaintexts |
| key backup *leaks* | attacker decrypts your whole repo offline — rotate every value AND the key |

## Verify ✅

- [ ] `argocd app get sealed-secrets` → Synced/Healthy, deployed by root (not by your kubectl)
- [ ] `kubectl get sealedsecret podlab-secrets -n argocd-podlab` exists, and `kubectl get secret podlab-secrets -n argocd-podlab -o jsonpath='{.metadata.ownerReferences[0].kind}'` → `SealedSecret`
- [ ] `curl -s http://podlab-helm.localhost:8080/config | grep r0tated` → shows the rotated api-key value
- [ ] `git grep -I sk-live $(git rev-list --all)` in k8s-gitops → no matches in any revision
- [ ] `git check-ignore ~/Code/k8s-gitops/podlab-secrets-unsealed.yaml` (recreate an empty file to test) → ignored
- [ ] `ls -la ~/sealed-secrets-key-backup.yaml` exists, contains `tls.key`, and lives **outside** every git repo: `git -C ~ rev-parse --git-dir 2>&1` → "not a git repository"
- [ ] You can answer cold: what happens to the SealedSecrets in git if the cluster and the key backup are both lost?

## Interview corner 💬

**"How do you keep secrets in a GitOps repo — even a public one?"** Encrypt-in-git or reference-external-store. For encrypt-in-git: sealed-secrets runs a controller holding a private key in-cluster; `kubeseal` encrypts with the public cert, so the repo only ever holds ciphertext bound (strict scope) to a specific name+namespace — replay into another namespace fails by construction. The controller unseals into ordinary Secrets, so apps need zero changes, and rotation is a reviewable commit. The caveat I'd volunteer unprompted: the private key is now your single point of failure, which leads to…

**"What's your sealed-secrets DR story?"** The sealing key exists only in-cluster, so cluster loss without a key backup means every SealedSecret in git is permanently dead. So: export the key Secrets (they're labeled `sealedsecrets.bitnami.com/sealed-secrets-key`) on a schedule, store them in a KMS/vault — never in the repo, since the key decrypts the whole repo. Restore-before-apps ordering matters in a rebuild: key first, controller, then the app sync. And because the controller renews keys every 30 days, the backup is recurring, not one-time, and `kubeseal --re-encrypt` keeps old ciphertext on current keys.

## Stretch goals

- Seal a per-env secret for `podlab-prod` (strict scope forces a fresh seal for ns `podlab-prod`) and add it to the prod overlay — confirm dev/stage *can't* use that ciphertext by applying it to `podlab-dev` and reading the controller's error events.
- Try `--scope namespace-wide`, rename the unsealed-to-be secret, confirm it still decrypts; reason about what you gave up.
- Force a key renewal (`kubectl delete pod` won't do it — set `--key-renew-period=5m` via chart values, temporarily), watch a second labeled key Secret appear, then `kubeseal --re-encrypt` your file and diff the ciphertext.
- Inspect the controller's decryption in the audit trail: `kubectl get events -n argocd-podlab --field-selector involvedObject.kind=SealedSecret`.

## Cleanup

Nothing to delete. **Sealed-secrets stays for the rest of the course** — it's the course's primary secret mechanism, Day 48 depends on it, and `~/sealed-secrets-key-backup.yaml` is now officially course-critical state: check it exists one more time before you close the terminal.
