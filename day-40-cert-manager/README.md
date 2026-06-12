# Day 40 — cert-manager: TLS as a Controller's Job

> **Time:** ~3 h · **Builds on:** Days 5 (ingress), 7 (secrets), 26–27 (ArgoCD apps), 38–39 (rollouts-lab)

## Objectives

- Explain the certificate lifecycle problem (issue → distribute → renew → rotate) and how cert-manager turns it into declarative resources.
- Build a local PKI the way corporations do: self-signed bootstrap → your own CA → a `ClusterIssuer` that signs everything.
- Put real, verifiable TLS on an Ingress with one annotation, and prove the chain with `curl --cacert` and `openssl s_client`.
- Force a renewal and watch the certificate rotate underneath a live endpoint.

## Concepts

### The problem isn't getting a cert. It's the other 364 days.

Anyone can run `openssl req` and produce a certificate. The operational problem is everything after: certificates expire (typically 90 days for Let's Encrypt; the industry keeps shortening lifetimes precisely so that *manual renewal is impossible to sustain*), they must land in the right Secret in the right namespace, every consumer must pick up the renewed cert without downtime, and when a CA is compromised you need to re-issue *everything*. Expired-cert outages have taken down giants — it's consistently one of the dumbest, most preventable classes of outage. The fix is the standard Kubernetes move: stop doing it by hand, declare the desired state, let a controller reconcile.

### cert-manager's resource model

[cert-manager](https://cert-manager.io/) splits "how to get certificates" from "which certificate I want":

| Resource | Question it answers | Scope |
|---|---|---|
| `Issuer` / `ClusterIssuer` | *How* do we sign? (ACME, a CA keypair, Vault, self-signed…) | namespace / cluster |
| `Certificate` | *What* do I want? (dnsNames, duration, which issuer, target Secret name) | namespace |
| `CertificateRequest` | One signing transaction (created by the controller; you mostly read these when debugging) | namespace |
| the **Secret** (`kubernetes.io/tls`) | The output: `tls.crt`, `tls.key` (+ `ca.crt`) — what pods and ingress actually consume | namespace |

The chain when you create a Certificate:

```
Certificate ──controller──► CertificateRequest ──issuer──► signed cert
     │                                                        │
     └────────────── Secret (tls.crt / tls.key) ◄─────────────┘
                        ▲ consumed by Ingress / pods
```

Renewal is the same loop on a timer: at `renewBefore` (default: 1/3 of lifetime before expiry) the controller re-runs the request and **updates the Secret in place**. ingress-nginx watches Secrets, so the new cert is served within seconds — no restarts, no humans.

### Why Let's Encrypt can't work here, and what mirrors reality instead

In production-on-the-internet, you'd use an **ACME** issuer (Let's Encrypt). ACME requires you to *prove control* of the domain before the CA signs:

- **HTTP-01**: the CA fetches `http://yourdomain/.well-known/acme-challenge/<token>` — cert-manager spins up a solver pod and routes the path to it. Requires the CA to reach your cluster on port 80. Per-hostname, easy, but no wildcards.
- **DNS-01**: cert-manager creates a TXT record via your DNS provider's API; the CA checks DNS. No inbound reachability needed; supports wildcards; requires DNS API credentials.

Your cluster lives on `*.localhost` behind a kind port-mapping. Let's Encrypt can neither resolve nor reach it — HTTP-01 and DNS-01 are both physically impossible here. The local pattern is the one **real corporate PKI uses for internal services anyway**:

1. A `selfsigned` ClusterIssuer — pure bootstrap, can sign anything, trusted by no one.
2. Use it once, to issue a **CA certificate** (`isCA: true`) — your root: "Course CA".
3. A `ClusterIssuer` of type `ca` backed by that CA's Secret — now anything in the cluster can request certs signed by Course CA.
4. Distribute Course CA's public cert to clients that should trust it (`curl --cacert`, OS trust stores; in-cluster at scale, that's [trust-manager](https://cert-manager.io/docs/trust/trust-manager/)'s job).

Same shape as a bank's internal PKI, minus the HSM. Everything you do today transfers; only the issuer type changes in the real world (`ca` → `acme` or `vault`).

### Two ways to wire TLS into an Ingress

1. **Explicit**: you create a `Certificate` resource, it produces a Secret, your Ingress references it under `spec.tls[].secretName`.
2. **Annotation (ingress-shim)**: you add `cert-manager.io/cluster-issuer: <name>` to the Ingress; cert-manager *generates the Certificate for you* from the Ingress's `tls` block (hosts → dnsNames, secretName → target). Less YAML, one fewer thing to drift. This is the primary route today; you'll do one explicit Certificate too (the CA itself) so you've seen both.

## Lab

### 1. Install cert-manager as an ArgoCD app

Create `~/Code/k8s-gitops/argocd/apps/cert-manager.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: cert-manager
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://charts.jetstack.io
    chart: cert-manager
    targetRevision: "*"
    helm:
      valuesObject:
        installCRDs: true
  destination:
    server: https://kubernetes.default.svc
    namespace: cert-manager
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

```sh
cd ~/Code/k8s-gitops
git add argocd/apps/cert-manager.yaml && git commit -m "platform: cert-manager" && git push
kubectl get pods -n cert-manager -w     # controller, webhook, cainjector → Running
kubectl get crd | grep cert-manager     # certificates, issuers, clusterissuers, ...
```

Also grab the CLI — you'll want it for renewal and debugging:

```sh
brew install cmctl
```

### 2. Build the PKI: bootstrap → CA → CA issuer

The day's core artifact is a three-resource chain. Requirements:

- `ClusterIssuer` **`selfsigned-bootstrap`** — type `selfSigned`, no config.
- `Certificate` **`course-ca`** in namespace `cert-manager` (CA material belongs with the PKI controller): `isCA: true`, `commonName: course-ca`, `secretName: course-ca-secret`, ECDSA-256 private key, `duration: 8760h` (1 year), issued by `selfsigned-bootstrap` (kind `ClusterIssuer`!).
- `ClusterIssuer` **`course-ca`** — type `ca`, `secretName: course-ca-secret`.

Write it, then compare:

<details><summary>Solution</summary>

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-bootstrap
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: course-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: course-ca
  secretName: course-ca-secret
  duration: 8760h          # 1 year
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: selfsigned-bootstrap
    kind: ClusterIssuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: course-ca
spec:
  ca:
    secretName: course-ca-secret
```

</details>

```sh
kubectl apply -f pki.yaml
kubectl get clusterissuer                              # both READY True
kubectl get certificate -n cert-manager course-ca      # READY True
kubectl get secret -n cert-manager course-ca-secret    # type kubernetes.io/tls
```

Trace what happened: `kubectl get certificaterequest -n cert-manager` — the intermediate object the controller created and the bootstrap issuer signed. When certificates *don't* go Ready, this chain (`Certificate` → `describe` → `CertificateRequest` → events) is your debugging path; `cmctl status certificate course-ca -n cert-manager` walks it for you.

### 3. TLS on the canary ingress — the annotation route

One annotation plus a `tls` block on Day 39's stable Ingress. Edit `day-39-rollouts-canary/rollout-canary.yaml`'s Ingress (or patch live):

```yaml
metadata:
  name: podlab-stable
  namespace: rollouts-lab
  annotations:
    cert-manager.io/cluster-issuer: course-ca     # ingress-shim trigger
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - canary.localhost
      secretName: podlab-tls                       # cert-manager will create this
  rules:
    ...unchanged...
```

```sh
kubectl apply -f rollout-canary.yaml   # or kubectl edit ingress podlab-stable -n rollouts-lab
kubectl get certificate -n rollouts-lab            # podlab-tls appeared — you never wrote it
kubectl get secret podlab-tls -n rollouts-lab      # kubernetes.io/tls, 3 keys
```

That auto-created `Certificate` is ingress-shim at work: annotation route = cert-manager writes the Certificate object *for* you. (If you ever need SANs or durations the Ingress can't express, fall back to writing the Certificate explicitly, like you did for the CA.)

### 4. Prove the TLS, properly

First the lazy way, then the right way:

```sh
curl -sk https://canary.localhost:8443/ | python3 -m json.tool | head -5     # -k = "ignore trust" — works, proves nothing
```

Now extract your CA's public cert and verify like a real client:

```sh
kubectl get secret -n cert-manager course-ca-secret -o jsonpath='{.data.ca\.crt}' | base64 -d > course-ca.crt
curl --cacert course-ca.crt https://canary.localhost:8443/ | python3 -m json.tool | head -5
```

No `-k`. curl validated: hostname matches a dnsName, signature chains to course-ca, dates valid. Inspect the chain yourself:

```sh
openssl s_client -connect localhost:8443 -servername canary.localhost -showcerts </dev/null 2>/dev/null | head -25
openssl s_client -connect localhost:8443 -servername canary.localhost </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
```

Read the output: `subject` is the leaf for `canary.localhost`, `issuer` is `CN=course-ca`, and the SAN extension carries the dnsName (modern clients ignore CN — SANs are what's verified). Note `-servername`: that's SNI, how one ingress IP serves many certs.

### 5. Renewal drill — rotation under live traffic

Renewal must be a non-event. Prove it. Terminal 1, a slow loop that fails loudly if TLS ever breaks:

```sh
while true; do curl --cacert course-ca.crt -s -o /dev/null -w '%{http_code} ' https://canary.localhost:8443/ || echo TLS-BROKE; sleep 1; done
```

Terminal 2 — record the serial, force a renewal, watch it change:

```sh
openssl s_client -connect localhost:8443 -servername canary.localhost </dev/null 2>/dev/null | openssl x509 -noout -serial
cmctl renew podlab-tls -n rollouts-lab
kubectl get certificaterequest -n rollouts-lab     # a new request, then Approved/Ready
sleep 10
openssl s_client -connect localhost:8443 -servername canary.localhost </dev/null 2>/dev/null | openssl x509 -noout -serial
```

New serial, and terminal 1 never printed anything but `200` — the Secret was updated in place and nginx hot-reloaded it. In production nobody runs `cmctl renew`; the controller does this automatically at `renewBefore`. The drill exists so you've *seen* the moving parts before you trust them.

### 6. Discussion: when the CA itself must rotate

Think through (no commands): your `course-ca` expires in a year, or worse, its key leaks. Every leaf cert chains to it. Rotation plan: issue a new CA, distribute *both* CA certs to clients (a CA **bundle** — this is exactly what trust-manager automates: a `Bundle` resource synced into every namespace), re-issue leaves against the new CA (with cert-manager: point the ClusterIssuer at the new Secret; leaves re-issue on renewal or `cmctl renew`), then retire the old CA from bundles. The lesson: *leaf* rotation is free with cert-manager; *CA* rotation is a distribution problem — which is why real CAs are long-lived, kept offline/in HSMs, and sign through intermediates.

## Verify ✅

- [ ] `kubectl get application cert-manager -n argocd` → `Synced/Healthy`; `kubectl get pods -n cert-manager` → 3 pods Running
- [ ] `kubectl get clusterissuer` → `selfsigned-bootstrap` and `course-ca`, both `READY True`
- [ ] `kubectl get certificate -A` → `course-ca` (cert-manager ns) and `podlab-tls` (rollouts-lab) both `READY True` — and you only wrote one of them
- [ ] `curl --cacert course-ca.crt https://canary.localhost:8443/` → JSON, **no `-k` flag**
- [ ] `openssl s_client ... | openssl x509 -noout -issuer` → `CN = course-ca`
- [ ] After `cmctl renew`: the serial differs, and the curl loop showed unbroken `200`s

## Interview corner 💬

**"How does certificate renewal work in your cluster?"**
cert-manager owns the lifecycle. Each cert is a `Certificate` resource declaring dnsNames, duration, and an issuer; the controller issues it into a TLS Secret and re-issues automatically at `renewBefore` — by default a third of the lifetime before expiry. Consumers don't participate: ingress controllers watch the Secret and hot-reload, so renewal is a non-event. We monitor it anyway — cert-manager exports `certmanager_certificate_expiration_timestamp_seconds`, and an alert on "expires in under N days" catches the case where renewal is silently failing, which is the real risk.

**"HTTP-01 vs DNS-01 — when do you use which?"**
Both prove domain control to an ACME CA. HTTP-01: the CA fetches a token over port 80 at the domain — simple, no DNS credentials, but the cluster must be publicly reachable, and no wildcards. DNS-01: cert-manager writes a TXT record through the DNS provider's API — works for private/internal clusters since nothing inbound is needed, and it's the only way to get wildcards; the cost is DNS API credentials living in the cluster, which are themselves a sensitive secret. Rule of thumb: public single hostnames → HTTP-01; wildcards or non-reachable clusters → DNS-01; internal-only services → skip ACME entirely and run a private CA issuer, like the one I built locally.

**"Why is everyone shortening cert lifetimes? Isn't that more risk of expiry outages?"**
It's the opposite bet: short lifetimes *force* automation, and automated renewal is far more reliable than humans with calendar reminders. Short-lived certs also shrink the blast radius of a stolen key and reduce dependence on revocation (CRL/OCSP), which in practice barely works. The expiry-outage risk only exists if renewal is manual — which is exactly the practice short lifetimes are designed to kill.

## Stretch goals

- Write the **explicit** Certificate variant for a second host (`active.localhost`, Day 38's ingress): a `Certificate` with two dnsNames, then reference its secret from the Ingress `tls` block — compare the YAML cost vs the annotation.
- Trust the CA at the OS level: `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain course-ca.crt`, then open https://canary.localhost:8443 in a browser — padlock, no warning. (Remember to remove it after the course: Keychain Access → System → course-ca.)
- Install trust-manager next to cert-manager and create a `Bundle` that distributes `course-ca.crt` into every namespace as a ConfigMap — the in-cluster half of trust distribution.
- Move the PKI YAML (`pki.yaml`) into `~/Code/k8s-gitops` as an ArgoCD app — issuers are platform config and belong in git.
- Set `duration: 1h` + `renewBefore: 55m` on a test Certificate and watch automatic renewal happen for real within the hour.

## Cleanup

**cert-manager STAYS** — the capstone uses it, and Day 44 breaks (then fixes) the TLS you built today. Keep the ClusterIssuers, the CA secret, `podlab-tls`, and `course-ca.crt` on disk.

Nothing else to delete — today added almost no resource weight (3 small controllers). If you did the OS-trust stretch goal, that's the one thing to remember to undo at course end.
