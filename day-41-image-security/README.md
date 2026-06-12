# Day 41 — Image Security: Trivy, SBOMs, Pod Security Standards

> **Time:** ~3 h · **Builds on:** Days 1 (image builds), 9 (namespaces/labels), 23+27 (GitOps repo, make lint)

## Objectives

- Scan images with Trivy and tune the noise/signal levers (severity, fixed-only, ignore files, exit codes) so a scan can gate CI without being ignored.
- Generate an SBOM and explain why you'd keep one even though you can re-scan the image.
- Enforce **Pod Security Standards** on a namespace, read a rejection error, and make podlab fully `restricted`-compliant.
- Push the hardened securityContext into the kustomize base so every environment gets it via GitOps.

## Concepts

### Your image is mostly other people's code

`podlab:v1` contains your ~200 lines of Go — plus a base image, a libc (or not), CA bundles, and whatever the build stages dragged in. A typical non-slim app image ships hundreds of OS packages you never chose, each a potential CVE. "Supply chain security" sounds abstract until you frame it as: *you ship dependencies you've never listed, from sources you've never audited, and you'll only find out which when one makes the news.* Two disciplines attack this:

1. **Know what's inside** — scanning + SBOMs (today).
2. **Make the runtime hostile to exploitation** — even a compromised container should be a bad place to be: no shell, no root, no writable filesystem, no capabilities (also today, via PSS).

### Trivy: one scanner, several targets

[Trivy](https://trivy.dev/) scans **images** (OS packages + language dependencies against CVE databases), **filesystems/repos**, **IaC configs** (your Kubernetes YAML — misconfigurations, not CVEs), and **SBOMs**. The levers that decide whether a scan is signal or wallpaper:

| Lever | What it does | Why it matters |
|---|---|---|
| `--severity HIGH,CRITICAL` | filter by severity | a 400-row report gets ignored; 6 rows get fixed |
| `--ignore-unfixed` | hide CVEs with no released fix | you can't patch what upstream hasn't patched — track separately, don't fail builds on it |
| `.trivyignore` | per-CVE allowlist | every entry needs a **justification comment** — it's a risk-acceptance record, not a mute button |
| `--exit-code 1` | non-zero exit when findings remain | turns the scan into a CI gate |

### SBOM: the ingredients list

A **Software Bill of Materials** (CycloneDX or SPDX format) is a machine-readable inventory of every component in the image. "Why keep one if I can re-scan the image?" Three reasons:

- **New CVEs against old artifacts.** When the next Log4Shell drops, you ask "which of my 200 deployed images contain version X?" Answering by re-pulling and re-scanning every image is slow and may be impossible (registry pruned, image gone). Grepping stored SBOMs takes seconds.
- **License compliance** — the SBOM lists licenses; legal cares even when security doesn't.
- **Customers and regulators increasingly demand them** (US executive order 14028 made SBOMs procurement table stakes).

The pattern: generate the SBOM once at build time, store it next to the image, re-scan the *SBOM* forever after.

### Pod Security Standards: the runtime contract

PodSecurityPolicy (PSP) was Kubernetes' first attempt to restrict what pods may do — powerful, infamously confusing, removed in v1.25. Its replacement is two pieces: **Pod Security Standards** (three named profiles — a policy *vocabulary*) and the built-in **Pod Security Admission** controller that enforces them per namespace via labels:

| Profile | Allows | Use for |
|---|---|---|
| `privileged` | everything | system namespaces (CNI, kube-system) |
| `baseline` | no privileged containers, no hostPath/hostNetwork, no added caps beyond a safe set | legacy apps in transition |
| `restricted` | baseline **plus**: must run non-root, must drop ALL capabilities, must set `seccompProfile`, no privilege escalation | everything you write from scratch |

Three *modes*, settable independently — this is the adoption path that makes PSS humane:

```
pod-security.kubernetes.io/enforce: restricted   # reject violating pods
pod-security.kubernetes.io/warn:    restricted   # let them in, warn the client
pod-security.kubernetes.io/audit:   restricted   # let them in, mark the audit log
```

Roll out `warn`+`audit` first, fix what screams, then flip `enforce`. Note what PSS is *not*: it only checks pod `securityContext`-class fields. It can't require labels, ban `:latest`, or enforce registries — that's tomorrow's Kyverno.

### Why distroless wins today

On Day 1 you built podlab `FROM gcr.io/distroless/static` with `USER nonroot`. Today is the payoff lap: near-zero CVEs (there are no OS packages to be vulnerable), and `restricted` compliance with almost no work (already non-root, no shell to escalate with, writes nothing so the root filesystem can be read-only). Security you got by *choosing less stuff* — the cheapest kind.

## Lab

### 1. Scan podlab, then scan something old

```sh
brew install trivy
trivy image podlab:v1
```

Read the summary: the OS line says something like `(distroless)` with **0 or near-0 vulnerabilities** — there's no package manager, no shell, no OpenSSL, nothing to CVE. Any findings will be in the Go binary's modules (`gobinary`). Now the contrast:

```sh
trivy image nginx:1.14        # 2018-era image; first run downloads it + the CVE DB — be patient
```

A wall of hundreds of CVEs across libc, openssl, pcre… This is what "we never rebuilt the base image" looks like. Get it down to something actionable:

```sh
trivy image --severity HIGH,CRITICAL nginx:1.14
trivy image --severity HIGH,CRITICAL --ignore-unfixed nginx:1.14
```

Compare counts at each step and notice the judgment call `--ignore-unfixed` encodes: a CVE with no available fix can't gate a build (you'd be blocked forever), but it should still be *visible* somewhere — dashboards, not gates.

### 2. CI ergonomics: exit codes and the ignore file

```sh
trivy image --severity CRITICAL --ignore-unfixed --exit-code 1 podlab:v1 ; echo "exit: $?"
trivy image --severity CRITICAL --ignore-unfixed --exit-code 1 nginx:1.14 ; echo "exit: $?"
```

`0` vs `1` — that's a CI gate in one flag. For the legitimate exception, create a `.trivyignore` in the scanned repo/dir:

```
# CVE-2023-XXXXX — vulnerable code path is in the TLS server which we do not enable.
# Risk accepted by <you>, review 2026-09-01.
CVE-2023-XXXXX
```

The comment discipline is the point: an ignore file without justifications is just a list of lies you'll believe later.

### 3. SBOM: generate, then scan the document instead of the image

```sh
trivy image --format cyclonedx --output podlab.sbom.json podlab:v1
python3 -m json.tool podlab.sbom.json | head -40
python3 -c "import json;d=json.load(open('podlab.sbom.json'));print(len(d.get('components',[])),'components')"
```

Skim the components: the Go stdlib, your module deps, the distroless base layers — the full ingredients list. Now the trick that matters at 3am during the next big CVE:

```sh
trivy sbom podlab.sbom.json
```

Same vulnerability report, **no image needed** — this works even after the image is pruned from every registry. Imagine `find sboms/ | xargs grep -l '"name": "log4j-core"'` across your whole fleet.

### 4. Scan your own YAML

CVEs aren't the only supply-chain risk — so are the manifests you wrote at 6pm on a Friday:

```sh
cd ~/Code/k8s-gitops
trivy config .
```

Triage the findings against what you know: some are real (missing `securityContext` fields — today fixes those), some don't apply locally (no resource limits on a lab chart). The skill is the *triage*, not a zero count.

### 5. Pod Security Standards: get rejected, then earn your way in

Create an enforced namespace and try a naive pod:

```sh
kubectl create namespace hardened
kubectl label namespace hardened pod-security.kubernetes.io/enforce=restricted
kubectl run naive --image=podlab:v1 -n hardened
```

**Rejected.** Read the error completely — it's one of the better error messages in Kubernetes and it lists every violation: `allowPrivilegeEscalation != false`, `unrestricted capabilities`, `runAsNonRoot != true`, `seccompProfile`. The image *is* non-root, but PSS judges the **manifest**, not the image — it can't know what's inside.

Now the day's core artifact: a fully `restricted`-compliant podlab. Requirements — pod named `podlab-hardened` (or a 1-replica Deployment) in `hardened`, image `podlab:v1`, port 8080, and a securityContext satisfying every clause: `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`, `seccompProfile.type: RuntimeDefault`, plus `readOnlyRootFilesystem: true` (not required by `restricted`, but podlab writes nothing — take the free hardening).

<details><summary>Solution</summary>

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: podlab-hardened
  namespace: hardened
  labels:
    app: podlab
spec:
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: podlab
      image: podlab:v1
      ports:
        - containerPort: 8080
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
```

</details>

```sh
kubectl apply -f hardened-pod.yaml
kubectl get pod -n hardened                          # Running — admitted AND working
kubectl exec -n hardened podlab-hardened -- sh       # fails: no shell. Distroless, remember.
kubectl port-forward -n hardened pod/podlab-hardened 8081:8080 &
curl -s localhost:8081/healthz && kill %1
```

It runs clean because of Day 1's choices: distroless + `USER nonroot` + writes-nothing meant the manifest just had to *declare* what the image already was.

### 6. Warn + audit on a live namespace — the safe adoption path

Never flip `enforce` on a running namespace blind. Dry-run it on `podlab-prod`:

```sh
kubectl label namespace podlab-prod \
  pod-security.kubernetes.io/warn=restricted \
  pod-security.kubernetes.io/audit=restricted
kubectl label namespace podlab-prod pod-security.kubernetes.io/enforce=restricted --dry-run=server -o yaml | head -5
kubectl rollout restart deployment -n podlab-prod    # recreate pods → warnings surface
```

Watch for `Warning: would violate PodSecurity "restricted"` lines naming exact fields. Nothing breaks — pods still run — but now you have the gap list.

### 7. Fix it once, fix it everywhere: harden the kustomize base

The violations you just saw get fixed in **git**, in the base, so dev/stage/prod all inherit it. In `~/Code/k8s-gitops/kustomize/podlab/base/`, add the securityContext blocks from step 5 to the podlab Deployment's pod template. Then:

```sh
cd ~/Code/k8s-gitops
kustomize build kustomize/podlab/overlays/prod | grep -A8 securityContext   # sanity-check the render
make lint                                                                   # Day 23 muscle memory
git add -A && git commit -m "harden podlab: restricted-compliant securityContext in base" && git push
```

Watch ArgoCD sync all three podlab apps, then re-run the restart from step 6 — the warnings are gone. One commit hardened three environments: GitOps amplifies fixes exactly like it amplifies mistakes.

### 8. The checklist

Commit this to memory — it's an interview answer and a code-review rubric:

- [ ] Minimal base: distroless/static or scratch; no shell or package manager in prod images
- [ ] `USER nonroot` baked into the image **and** `runAsNonRoot` declared in the manifest
- [ ] Pin by digest (`image@sha256:...`) for prod, never `:latest` (a tag is a mutable pointer — tomorrow Kyverno enforces this)
- [ ] Scan in CI with `--exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed`; SBOM generated and stored at build time
- [ ] `readOnlyRootFilesystem`, drop ALL caps, no privilege escalation, RuntimeDefault seccomp
- [ ] Namespaces labeled with PSS: `restricted` enforced for app namespaces, exceptions documented

## Verify ✅

- [ ] `trivy image --severity HIGH,CRITICAL podlab:v1` → 0 (or near-0) findings; same flags on `nginx:1.14` → dozens
- [ ] `trivy image --severity CRITICAL --ignore-unfixed --exit-code 1 podlab:v1; echo $?` → `0`
- [ ] `trivy sbom podlab.sbom.json` runs and reports against the file, image not touched
- [ ] `kubectl run naive --image=podlab:v1 -n hardened` → error mentioning `violates PodSecurity "restricted:latest"`
- [ ] `kubectl get pod podlab-hardened -n hardened` → `Running`, and `kubectl get ns hardened -o jsonpath='{.metadata.labels}'` shows the enforce label
- [ ] After the base hardening commit: `kubectl get deploy -n podlab-prod -o yaml | grep -c runAsNonRoot` ≥ 1, and a rollout restart produces no PodSecurity warnings

## Interview corner 💬

**"What's in your image pipeline before something reaches prod?"**
Build from a pinned minimal base — distroless for compiled languages — as non-root. In CI: Trivy scan gating on HIGH/CRITICAL with `--ignore-unfixed`, an audited `.trivyignore` for accepted risks, and a CycloneDX SBOM generated and stored with the artifact so we can answer "who ships libX?" without re-pulling images. Push by digest, deploy via GitOps. At admission, the cluster enforces the `restricted` Pod Security Standard plus policy rules (registries, no `:latest`). And scanning isn't only at build time — images age, so stored SBOMs get re-scanned on a schedule against fresh CVE data.

**"PSS vs PSP — what happened there?"**
PodSecurityPolicy tried to do per-pod security enforcement with its own RBAC-bound policy objects; it was powerful but the binding model was so confusing that many clusters ran it misconfigured or not at all. It was deprecated in 1.21 and removed in 1.25. The replacement deliberately does less: Pod Security *Standards* define just three profiles, and the built-in admission controller applies them per namespace via labels, with enforce/warn/audit modes for gradual adoption. Everything PSP did beyond that — fine-grained or org-specific rules — moved to external policy engines like Kyverno or Gatekeeper, which are better at it anyway.

**"Why distroless? Isn't Alpine small enough?"**
Alpine is small but it's still a distro: a shell, a package manager, musl, busybox — attack surface and CVE surface. Distroless is the runtime dependencies and nothing else; for a static Go binary that's essentially CA certs and tzdata. Consequences: vulnerability scans come back near-empty, an attacker who achieves code execution has no shell or curl to live off, and image pulls are faster. The tradeoff is debuggability — no shell for `kubectl exec` — which Kubernetes solved with ephemeral debug containers (`kubectl debug --target`), so the tradeoff is mostly gone.

## Stretch goals

- `kubectl debug -n hardened podlab-hardened --image=busybox --target=podlab -it` — debug the shell-less pod with an ephemeral container; check PSS's view of it.
- Re-scan by digest: `docker inspect podlab:v1 --format '{{.Id}}'`, then discuss why prod manifests should pin `image@sha256:...` (tags move; digests don't).
- `trivy image --format table --list-all-pkgs podlab:v1` — the full package inventory without the SBOM formality.
- Add a `make scan` target to `~/Code/k8s-gitops` running `trivy config --exit-code 1 --severity HIGH,CRITICAL .` next to Day 23's `make lint` — tomorrow you'll add `make policy` beside it.
- Label `hardened` with `enforce-version=v1.31` (pin the PSS version) and read up on why unpinned `latest` PSS can break you on cluster upgrades.

## Cleanup

```sh
kubectl delete namespace hardened
rm -f podlab.sbom.json nginx-scan.txt 2>/dev/null
docker rmi nginx:1.14        # free ~100MB on your Mac; the cluster never ran it
```

**Keep:** the PSS `warn`/`audit` labels on `podlab-prod` (they cost nothing and feed tomorrow's policy discussion), the hardened kustomize base in git (permanent improvement), and Trivy itself. The hardening commit is now part of your platform — don't revert it.
