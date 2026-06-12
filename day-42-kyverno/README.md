# Day 42 — Kyverno: Policy as Code

> **Time:** ~3.5 h · **Builds on:** Days 9 (quotas), 15 (NetworkPolicies), 23 (make lint), 41 (PSS)

## Objectives

- Explain where admission control sits in the request path and why orgs need policy beyond Pod Security Standards.
- Install Kyverno via ArgoCD and write all four rule types: **validate**, **mutate**, **generate** (and know where verifyImages fits).
- Adopt a policy safely: Audit → read PolicyReports → Enforce.
- Shift policy left: run the same policies in CI with the Kyverno CLI before they ever reach the cluster.

## Concepts

### The last gate before etcd

Every `kubectl apply`, every controller action, every ArgoCD sync is an API request, and each one passes through the same pipeline: authentication → authorization (RBAC, Day 14) → **admission** → persistence in etcd. Admission is the last point where the cluster can say "no" or "yes, but let me fix that" — *before* the object exists. Two webhook flavors:

- **Validating** webhooks: inspect the object, allow or reject. (Pod Security Admission from yesterday is a built-in one.)
- **Mutating** webhooks: rewrite the object on the way in (defaults, sidecar injection, label stamping). These run *before* validation — so a mutation can make an object pass a check.

Why isn't PSS enough? It validates exactly one thing: pod security fields. Your org's actual rules look like: *every Deployment carries a `team` label; images come from our registry, never `:latest`; everything has resource requests; every namespace starts with a default-deny NetworkPolicy*. Those are policy, and hand-enforcing them in code review is how they get skipped. **Policy as code** puts them in git, enforced by a controller, with an audit trail.

### The engine landscape

| | Kyverno | OPA/Gatekeeper | ValidatingAdmissionPolicy |
|---|---|---|---|
| Policy language | YAML (+ JMESPath/CEL) | Rego | CEL, in-tree |
| Mutate / generate | ✅ / ✅ | mutation limited, no generate | ❌ validate only |
| Learning curve | low if you know K8s YAML | a real language to learn | low-ish, but verbose |
| Beyond Kubernetes | K8s-focused | Rego runs anywhere (CI, apps, Envoy) | K8s only |

[Kyverno](https://kyverno.io/) wins on Kubernetes-native ergonomics: policies are CRDs that *look like* the resources they govern, and matching/mutating uses patterns you already know from kustomize-style overlays. Be fair to **Gatekeeper**: Rego is a genuine policy *language* — reusable logic, unit-testable, and the same policies can run in CI pipelines, microservices, and Envoy filters; if your org standardizes policy across more than Kubernetes, that generality wins. And know **ValidatingAdmissionPolicy**: CEL-based validation built into the API server itself (stable since v1.30) — no webhook, no controller, no availability tradeoff; it will eat the simple-validation end of this space, but it neither mutates nor generates.

### Kyverno's model

- **`ClusterPolicy`** (cluster-wide) / **`Policy`** (one namespace) contain **rules**. Each rule = `match` (kinds, names, namespaces, label selectors, optional `exclude`) + one verb:
  - `validate` — pattern or CEL check; `failureAction: Audit` (admit, report) or `Enforce` (reject).
  - `mutate` — strategic-merge patch or JSON patch; anchors like `+(key)` mean "add only if absent".
  - `generate` — create *other* resources when something happens (classically: on new Namespace).
  - `verifyImages` — verify cosign/notary signatures before admitting (mention-level today).
- **Autogen**: match `Pod` and Kyverno automatically derives rules for Deployments, StatefulSets, DaemonSets, Jobs — you write the rule once at the pod level.
- **Background scans**: validation isn't only at admission; Kyverno periodically re-checks *existing* resources and writes **`PolicyReport`** objects per namespace. New policy, instant inventory of current violations — this is what makes safe adoption possible.

### The adoption path (the part that gets you promoted)

Day-one `Enforce` on a live cluster breaks deploys and makes the platform team the enemy. The grown-up sequence:

```
1. failureAction: Audit  →  2. read PolicyReports for a few days
→  3. fix or grant exceptions  →  4. flip to Enforce
```

Exceptions are first-class: a **`PolicyException`** resource names the policy, the rule, and exactly which resource is exempt — reviewable in git, instead of a permanent "disable the webhook for team X" hack.

## Lab

### 1. Install Kyverno as an ArgoCD app

`~/Code/k8s-gitops/argocd/apps/kyverno.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kyverno
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://kyverno.github.io/kyverno
    chart: kyverno
    targetRevision: "*"
    helm:
      valuesObject:
        features:
          policyExceptions:
            enabled: true
            namespace: kyverno     # PolicyExceptions only honored from here
  destination:
    server: https://kubernetes.default.svc
    namespace: kyverno
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      - ServerSideApply=true       # kyverno's CRDs are huge; SSA avoids annotation-size errors
```

```sh
cd ~/Code/k8s-gitops && git add argocd/apps/kyverno.yaml && git commit -m "platform: kyverno" && git push
kubectl get pods -n kyverno        # admission-, background-, cleanup-, reports-controller
kubectl get validatingwebhookconfigurations | grep kyverno
```

That webhook registration is the moment Kyverno entered the API request path.

The four policies below are the day's core artifact. For each: requirements in the step, full reference in [`policies/`](policies/). Write your own first.

### 2. Policy 1 — VALIDATE (Enforce): require a team label

Requirements: a `ClusterPolicy` named `require-team-label`; match `Deployment`; reject (Enforce) any Deployment without a non-empty `metadata.labels.team`; rejection message that tells the user exactly what to add; `background: true`. (Hint: pattern value `"?*"` = any non-empty string. Note: since Kyverno 1.13 the action field lives at `rules[].validate.failureAction`; the old top-level `validationFailureAction` is deprecated.)

Reference: [`policies/require-team-label.yaml`](policies/require-team-label.yaml)

```sh
kubectl apply -f policies/require-team-label.yaml
kubectl create deployment unlabeled --image=podlab:v1 -n default
```

Rejected — and look *whose words* are in the error: yours. `admission webhook "validate.kyverno.svc-fail" denied the request: ... Every Deployment needs a 'team' label so we know who to page.` Error messages that teach are the difference between guardrails and gatekeeping. Now satisfy it:

```sh
kubectl create deployment labeled --image=podlab:v1 --dry-run=client -o yaml -n default \
  | kubectl label --local -f - team=platform --dry-run=client -o yaml | kubectl apply -f -
kubectl delete deployment labeled -n default
```

### 3. Policy 2 — VALIDATE (Audit → Enforce): ban `:latest`

Requirements: `ClusterPolicy` `disallow-latest-tag`, match `Pod` (autogen covers the workload kinds), **two rules**: images must have an explicit tag (`*:*` — untagged means latest), and the tag must not be `latest` (`!*:latest`). Start with `failureAction: Audit`.

Reference: [`policies/disallow-latest-tag.yaml`](policies/disallow-latest-tag.yaml)

```sh
kubectl apply -f policies/disallow-latest-tag.yaml
kubectl run sloppy --image=nginx:latest -n default        # ADMITTED — Audit doesn't block
sleep 30   # give the reports controller a moment
kubectl get policyreport -n default
kubectl get policyreport -n default -o yaml | grep -B2 -A6 'disallow-latest'
```

The pod runs, but the violation is on record — `result: fail` naming pod, policy, rule. This is the adoption path from Concepts in miniature: in real life you'd watch reports for days; here, survey the whole cluster's compliance right now:

```sh
kubectl get policyreport -A
```

(Background scans also grade everything that existed *before* the policy.) Now flip both rules to `failureAction: Enforce` in the file, re-apply, and:

```sh
kubectl delete pod sloppy -n default
kubectl run sloppy --image=nginx:latest -n default        # now REJECTED
```

### 4. Policy 3 — MUTATE: default resource requests

Requirements: `ClusterPolicy` `add-default-resources`, match `Pod`, strategic-merge mutate that adds `requests: {cpu: 50m, memory: 64Mi}` to every container **only when requests are absent** — pods that declare their own must pass through untouched. (Hints: `(name): "*"` targets all containers; the `+(key)` anchor means add-if-absent.)

Reference: [`policies/add-default-resources.yaml`](policies/add-default-resources.yaml)

```sh
kubectl apply -f policies/add-default-resources.yaml
kubectl run bare --image=podlab:v1 -n default
kubectl get pod bare -n default -o jsonpath='{.spec.containers[0].resources}{"\n"}'
```

`{"requests":{"cpu":"50m","memory":"64Mi"}}` — you never wrote that; the webhook rewrote your pod on the way in (the pod also carries a `policies.kyverno.io/last-applied-patches` annotation as a receipt). Counter-test that explicit values survive:

```sh
kubectl run explicit --image=podlab:v1 -n default --overrides='{"spec":{"containers":[{"name":"explicit","image":"podlab:v1","resources":{"requests":{"cpu":"200m"}}}]}}'
kubectl get pod explicit -n default -o jsonpath='{.spec.containers[0].resources}{"\n"}'   # cpu stays 200m; memory added
kubectl delete pod bare explicit -n default
```

That's Day 8's QoS lesson turned into infrastructure: BestEffort pods can no longer happen by accident.

### 5. Policy 4 — GENERATE: namespaces born with guardrails

Requirements: `ClusterPolicy` `ns-defaults`, match `Namespace` (exclude `kube-*` and `kyverno`), two generate rules with `synchronize: true`: a `NetworkPolicy` `default-deny-all` (empty podSelector, both policyTypes — Day 15) and a `ResourceQuota` `default-quota` (e.g. `requests.cpu: 2`, `requests.memory: 2Gi`, `pods: 20` — Day 9). **Plus** the RBAC Kyverno's background controller needs to create those kinds: a ClusterRole labeled `rbac.kyverno.io/aggregate-to-background-controller: "true"` (Kyverno can't create what *it* isn't authorized to create).

Reference: [`policies/generate-ns-defaults.yaml`](policies/generate-ns-defaults.yaml)

```sh
kubectl apply -f policies/generate-ns-defaults.yaml
kubectl create namespace test-gen
kubectl get networkpolicy,resourcequota -n test-gen
```

Both objects exist and nobody applied them. Day 9 + Day 15, automated into the platform: every future namespace starts closed and bounded, and teams open up *deliberately*. Test `synchronize`:

```sh
kubectl delete networkpolicy default-deny-all -n test-gen
sleep 5; kubectl get networkpolicy -n test-gen     # it's back
```

### 6. Shift left: the Kyverno CLI in CI

Admission-time rejection is the backstop; the *developer experience* is catching it before push, exactly like Day 23's `make lint`:

```sh
brew install kyverno
cat > /tmp/bad-deploy.yaml <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: bad, namespace: default}
spec:
  selector: {matchLabels: {app: bad}}
  template:
    metadata: {labels: {app: bad}}
    spec: {containers: [{name: app, image: nginx:latest}]}
EOF
kyverno apply policies/ --resource /tmp/bad-deploy.yaml
```

Two policies fail, locally, in milliseconds — no cluster involved. Wire it into the GitOps repo's Makefile next to `lint`:

```make
policy:   ## evaluate rendered manifests against cluster policies
	kustomize build kustomize/podlab/overlays/prod | kyverno apply policies/ --resource - || exit 1
```

(Copy the `policies/` directory into `~/Code/k8s-gitops` and commit — the policies themselves belong in git, applied by ArgoCD like everything else. Same rule as ever: the cluster state you typed today should end the day as a git commit.)

### 7. The legit exception

`podlab-dev` wants `:latest` for fast iteration — a defensible dev-only exception. Don't weaken the policy; carve a hole with provenance (must live in the `kyverno` namespace, per the helm value from step 1):

```yaml
apiVersion: kyverno.io/v2
kind: PolicyException
metadata:
  name: dev-latest-ok
  namespace: kyverno
spec:
  exceptions:
    - policyName: disallow-latest-tag
      ruleNames: ["disallow-latest", "autogen-disallow-latest"]
  match:
    any:
      - resources:
          kinds: ["Pod", "Deployment"]
          namespaces: ["podlab-dev"]
```

```sh
kubectl apply -f exception.yaml
kubectl run dev-latest --image=nginx:latest -n podlab-dev     # admitted
kubectl run dev-latest --image=nginx:latest -n default        # still rejected
kubectl delete pod dev-latest -n podlab-dev
```

The exception is an auditable object: who is exempt, from which rule, reviewable in git — not a Slack promise.

## Verify ✅

- [ ] `kubectl get application kyverno -n argocd` → `Synced/Healthy`; `kubectl get clusterpolicy` → 4 policies, all `READY True`
- [ ] `kubectl create deployment unlabeled --image=podlab:v1 -n default` → rejected, error contains your team-label message
- [ ] With `disallow-latest-tag` in Enforce: `kubectl run sloppy --image=nginx:latest -n default` → rejected; same command in `podlab-dev` (with the exception) → admitted
- [ ] `kubectl run bare --image=podlab:v1 -n default` then `kubectl get pod bare -o jsonpath='{.spec.containers[0].resources.requests}'` → `{"cpu":"50m","memory":"64Mi"}`
- [ ] `kubectl create ns test-gen` → `kubectl get networkpolicy,resourcequota -n test-gen` shows `default-deny-all` and `default-quota`
- [ ] `kyverno apply policies/ --resource /tmp/bad-deploy.yaml` → reports both the tag rules failing, locally
- [ ] `kubectl get policyreport -A` → reports exist; you can name which namespace has the most `fail` results

## Interview corner 💬

**"How do you enforce org standards without becoming the bottleneck?"**
Move the enforcement from people to admission control, and move the feedback as early as possible. Standards live as Kyverno policies in git — reviewable, versioned. New policies always land in Audit mode first; background scans give us a PolicyReport inventory of existing violations, we fix or grant explicit PolicyExceptions, then flip to Enforce — so enforcement never breaks a running team by surprise. Developers get the same verdicts in CI via the Kyverno CLI against rendered manifests, so the cluster webhook is the backstop, not the place you find out. And rejection messages are written to teach: they say what to change, not just "denied".

**"Kyverno vs Gatekeeper?"**
Kyverno: policies are Kubernetes-style YAML, so anyone who writes manifests can write policy; it does validate, mutate, generate, and image verification — generate especially (default NetworkPolicies/quotas per namespace) has no real Gatekeeper equivalent. Gatekeeper: policies in Rego, a real policy language — unit-testable, composable, and portable beyond Kubernetes to CI, services, and API gateways; if the org standardizes on OPA everywhere, that consistency beats Kyverno's ergonomics. For a Kubernetes-centric platform team I default to Kyverno; and for simple validations I'd now also consider in-tree ValidatingAdmissionPolicy — CEL in the API server, no webhook on the critical path at all.

**"A mutating webhook rewrote something and broke prod. Thoughts?"**
That's the known cost of mutation: the applied object differs from git, so GitOps diffing gets noisy and surprises happen at admission, invisibly. Mitigations: keep mutations small and idempotent (defaults only, add-if-absent semantics), surface them (Kyverno annotates patched resources), make ArgoCD ignore known mutated fields, and prefer validate-with-good-error over mutate when the fix is trivial for the author. Also know your webhook's `failurePolicy`: `Fail` means policy outage blocks all deploys; `Ignore` means policy silently off — you must consciously choose per use case.

## Stretch goals

- Write policy 5 yourself: require `readinessProbe` on all containers in `podlab-prod` only (use a `Policy`, not `ClusterPolicy`). Audit first, check the report.
- Explore `verifyImages`: read the Kyverno + cosign docs and sketch the policy that would require signatures on `podlab:*` images (full signing lands in the capstone's CI day).
- Rewrite `require-team-label` as an in-tree `ValidatingAdmissionPolicy` (CEL: `object.metadata.?labels.team.orValue('') != ''`) and compare the YAML.
- Run `kubectl get clusterpolicyreport` / scan reports for the `monitoring` namespace — how compliant is the stack you installed in Phase 5?
- Look at the Kyverno policy library (https://kyverno.io/policies/) — find the three policies you'd adopt first at work.

## Cleanup

```sh
kubectl delete ns test-gen
kubectl delete deployment unlabeled -n default --ignore-not-found
rm -f /tmp/bad-deploy.yaml
```

**Kyverno and all four policies STAY** — the capstone deploys through them (and Day 44's chaos is more fun with guardrails on). RAM note: Kyverno's controllers idle around a few hundred MB total; if pressed, scale the optional ones to zero between days:

```sh
kubectl scale deploy kyverno-cleanup-controller kyverno-reports-controller -n kyverno --replicas=0
```

(Keep `admission` and `background` controllers running, or your policies stop working.)
