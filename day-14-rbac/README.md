# Day 14 — RBAC: Who May Do What

> **Time:** ~3 h · **Builds on:** Days 7, 9

## Objectives

- Trace a request through authentication and authorization in the API server
- Choose correctly among Role/ClusterRole × RoleBinding/ClusterRoleBinding
- Build a least-privilege ServiceAccount with its own kubeconfig and *prove* its limits
- Audit permissions fast with `kubectl auth can-i` and `--as` impersonation

## Concepts

### The API server's gauntlet

Every request — yours, a controller's, a pod's — hits the same pipeline:

```
request ──▶ AuthN ──▶ AuthZ ──▶ Admission ──▶ etcd
            "who are     "may they      "is the object
             you?"        do this?"      acceptable?" (Day 38)
            401 if not    403 if not
```

**Authentication** establishes identity. Two kinds matter:

- **Users** are *not API objects*. There is no `kind: User`. A user is whatever AuthN says it is — for kubeadm clusters that usually means client certificates: the CN of your cert is your username, the O fields are your groups. Look at your own identity: your kind kubeconfig has a `client-certificate-data` whose CN is `kubernetes-admin` in group `system:masters` (a group with a cluster-admin binding — which is why you can do anything).
- **ServiceAccounts** *are* API objects, namespaced, intended for programs (pods, CI systems). They authenticate with signed JWTs the cluster mints. Their AuthZ name is `system:serviceaccount:<namespace>:<name>`.

**Authorization** in practically every cluster means **RBAC**: rules grant verbs on resources; no rule means no (deny by default; there are no deny rules — you can only fail to allow).

### The four objects, two questions

| | Rules live in a namespace | Rules are cluster-wide |
|---|---|---|
| **Grant within one namespace** | Role + RoleBinding | ClusterRole + RoleBinding |
| **Grant across the cluster** | — (impossible) | ClusterRole + ClusterRoleBinding |

- **Role** = a named list of permissions, valid in one namespace. **ClusterRole** = same, but can also cover cluster-scoped resources (nodes, PVs, namespaces themselves) and non-resource URLs.
- **RoleBinding** = "these subjects get that role, *in this namespace*". **ClusterRoleBinding** = "...everywhere".
- The interesting combination is **ClusterRole + RoleBinding**: define the permission set once ("can read pods"), bind it per-namespace to different teams. This is exactly how the built-ins are meant to be used.

A rule has four dials:

```yaml
rules:
- apiGroups: ["apps"]        # "" = core group (pods, services, secrets...)
  resources: ["deployments"]
  verbs: ["get", "list", "update", "patch"]
  resourceNames: ["web"]     # optional: only THIS object (no wildcard; list is bypassable — caveat below)
```

Verbs you'll grant constantly: `get`, `list`, `watch` (reading — k9s needs all three), `create`, `update`, `patch`, `delete`. Two subtleties trip everyone:

- **Subresources** are separate permissions, written `resource/subresource`. Reading pods does **not** let you read logs — that's `pods/log` (verb `get`). Likewise `pods/exec` (`create`), `deployments/scale` (`update`/`patch`). Forgetting `pods/log` is the #1 "my CI bot can't see logs" bug.
- **`resourceNames`** restricts to named objects but doesn't apply to `list`/`watch` (those can't know names in advance) and doesn't survive `create`.

### The built-ins and aggregation

`kubectl get clusterroles | head -40`: among the `system:` plumbing live three you should reach for before writing your own: **view** (read everything non-secret in a namespace), **edit** (view + modify most things, *not* RBAC itself), **admin** (edit + manage Roles/RoleBindings in the namespace). They're built by **aggregation**: each carries an `aggregationRule` with label selectors, and a controller unions every ClusterRole matching those labels into them. That's how CRD authors plug in: ship a ClusterRole labeled `rbac.authorization.k8s.io/aggregate-to-view: "true"` and the stock `view` role learns to read your CRD. Inspect it: `kubectl get clusterrole view -o yaml | head -20`.

### Tokens, and the audit tool

Every pod gets its namespace's `default` ServiceAccount token auto-mounted at `/var/run/secrets/kubernetes.io/serviceaccount/` unless you say otherwise. A compromised container can use it against the API. The default SA has ~no permissions, but defense in depth says: workloads that don't talk to the API should set `automountServiceAccountToken: false`. For humans and CI you mint short-lived tokens on demand: `kubectl create token <sa> --duration=2h` (a JWT, no Secret object involved — the modern way; long-lived SA token Secrets are legacy).

Your fastest RBAC tool is impersonation:

```sh
kubectl auth can-i delete pods                                  # me?
kubectl auth can-i list secrets --as=jane                       # a user?
kubectl auth can-i get pods/log -n ci --as=system:serviceaccount:ci:ci-bot
kubectl auth can-i --list -n ci --as=system:serviceaccount:ci:ci-bot   # dump everything
```

Test grants *before* shipping credentials. Today's lab does both: build a deployer bot, then prove — with real Forbidden errors — what it can't do.

## Lab

### 1. Namespace, ServiceAccount, and the core Role

```sh
kubectl create namespace ci
kubectl create serviceaccount ci-bot -n ci
```

Now the Role — the day's core object. The bot is a *deployer*: it watches pods, reads their logs, and updates deployments (e.g. `kubectl set image`). Write `ci-bot-role.yaml` yourself. Requirements:

- name `ci-deployer`, namespace `ci`
- get/list/watch on `pods` (core group)
- get on `pods/log`
- get/list/watch + update/patch on `deployments` (group `apps`) — kubectl needs to *read* a deployment before patching it
- nothing else. No secrets, no create, no delete.

<details><summary>Solution</summary>

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ci-deployer
  namespace: ci
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "watch", "update", "patch"]
```

</details>

Bind it (boilerplate — or imperatively, see CKA corner):

```sh
kubectl apply -f ci-bot-role.yaml
kubectl create rolebinding ci-deployer \
  --role=ci-deployer \
  --serviceaccount=ci:ci-bot \
  -n ci
```

Audit before you build anything else:

```sh
kubectl auth can-i list pods -n ci --as=system:serviceaccount:ci:ci-bot          # yes
kubectl auth can-i get pods/log -n ci --as=system:serviceaccount:ci:ci-bot       # yes
kubectl auth can-i patch deployments.apps -n ci --as=system:serviceaccount:ci:ci-bot  # yes
kubectl auth can-i list secrets -n ci --as=system:serviceaccount:ci:ci-bot       # no
kubectl auth can-i delete pods -n ci --as=system:serviceaccount:ci:ci-bot        # no
kubectl auth can-i list pods -n guestbook --as=system:serviceaccount:ci:ci-bot   # no
kubectl auth can-i --list -n ci --as=system:serviceaccount:ci:ci-bot             # the full picture
```

### 2. Mint a token, assemble a kubeconfig by hand

This is the part that demystifies kubeconfigs forever. A kubeconfig is just three things: *where* (server URL + CA to trust), *who* (credentials), and a context tying them together.

```sh
TOKEN=$(kubectl create token ci-bot -n ci --duration=2h)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
echo "$SERVER"   # https://127.0.0.1:<port> — kind publishes the apiserver on localhost
```

Write `ci-bot.kubeconfig` (substitute the three values; `envsubst` or paste by hand):

```sh
cat > ci-bot.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters:
- name: course
  cluster:
    server: ${SERVER}
    certificate-authority-data: ${CA}
users:
- name: ci-bot
  user:
    token: ${TOKEN}
contexts:
- name: ci-bot@course
  context:
    cluster: course
    user: ci-bot
    namespace: ci
current-context: ci-bot@course
EOF
```

Decode the JWT out of curiosity — subject, audience, expiry are all visible:

```sh
echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | python3 -m json.tool
```

### 3. Prove the boundaries (Forbidden errors are the success criteria)

Give the bot something to manage, then become the bot:

```sh
kubectl create deployment web --image=podlab:v1 -n ci

export KUBECONFIG=$PWD/ci-bot.kubeconfig
kubectl get pods                          # ✓ works (note: defaults to ns ci from the context)
kubectl logs deploy/web                   # ✓ works — pods/log granted
kubectl set image deployment/web podlab=podlab:v1 --record=false   # ✓ patch deployments
kubectl get secrets                       # ✗ Forbidden
kubectl delete pod -l app=web             # ✗ Forbidden
kubectl get pods -n guestbook             # ✗ Forbidden (Role is ci-only)
kubectl get nodes                         # ✗ Forbidden (cluster-scoped, no ClusterRole)
kubectl auth can-i --list                 # see yourself as the API sees you
unset KUBECONFIG                          # back to admin — don't forget!
```

Read one Forbidden message closely:

> `Error from server (Forbidden): secrets is forbidden: User "system:serviceaccount:ci:ci-bot" cannot list resource "secrets" in API group "" in the namespace "ci"`

It names the subject, verb, resource, group, and namespace — everything you need to write the missing rule, *if* it should exist. This message format is half of RBAC debugging.

### 4. Harden the default ServiceAccount

Any pod in `ci` still auto-mounts the `default` SA token:

```sh
kubectl run probe -n ci --image=busybox --restart=Never -- sleep 3600
kubectl exec -n ci probe -- ls /var/run/secrets/kubernetes.io/serviceaccount/
# ca.crt  namespace  token  ← live credentials inside every container
```

Turn it off at the SA level so every pod using `default` in `ci` stops mounting:

```sh
kubectl patch serviceaccount default -n ci -p '{"automountServiceAccountToken": false}'
kubectl delete pod probe -n ci
kubectl run probe -n ci --image=busybox --restart=Never -- sleep 3600
kubectl exec -n ci probe -- ls /var/run/secrets/kubernetes.io/serviceaccount/ 2>&1
# ls: ... No such file or directory  ← nothing to steal
kubectl delete pod probe -n ci
```

(The same field exists on the pod spec for per-workload opt-out, and pods that *do* need the API should run a dedicated SA like `ci-bot`, never `default`.)

### 5. Look at aggregation

```sh
kubectl get clusterrole admin -o yaml | grep -B2 -A6 aggregationRule
kubectl get clusterroles -l rbac.authorization.k8s.io/aggregate-to-view=true
```

The `view`/`edit`/`admin` trio is assembled from labeled parts. When you install CRD-shipping software later in the course (ArgoCD, Prometheus operators), look for their `aggregate-to-*` ClusterRoles — it's how `kubectl get` on their custom resources "just works" for namespace viewers.

## Verify ✅

- [ ] `kubectl auth can-i --list -n ci --as=system:serviceaccount:ci:ci-bot` shows exactly: pods get/list/watch, pods/log get, deployments.apps get/list/watch/update/patch (plus the standard `selfsubjectreviews` boilerplate every authenticated subject has)
- [ ] `KUBECONFIG=$PWD/ci-bot.kubeconfig kubectl get pods` succeeds and lists the `web` pod
- [ ] `KUBECONFIG=$PWD/ci-bot.kubeconfig kubectl get secrets` → `Forbidden`, message naming `system:serviceaccount:ci:ci-bot`
- [ ] `KUBECONFIG=$PWD/ci-bot.kubeconfig kubectl get pods -n guestbook` → `Forbidden`
- [ ] `KUBECONFIG=$PWD/ci-bot.kubeconfig kubectl get nodes` → `Forbidden`
- [ ] After the patch, a fresh pod in `ci` has no `/var/run/secrets/kubernetes.io/serviceaccount/` directory
- [ ] `kubectl get clusterrole view -o jsonpath='{.aggregationRule}'` is non-empty

## CKA corner 🎓

RBAC is guaranteed exam material and 100% doable imperatively — never write Role YAML by hand under time pressure:

```sh
kubectl create role NAME --verb=get,list --resource=pods,pods/log -n NS
kubectl create clusterrole NAME --verb=get,list --resource=nodes
kubectl create rolebinding NAME --role=R --serviceaccount=NS:SA -n NS
kubectl create rolebinding NAME --clusterrole=view --user=jane -n NS
kubectl create clusterrolebinding NAME --clusterrole=CR --group=devs
```

Then verify with `auth can-i ... --as=...`. Remember: subjects can be `--user`, `--group`, or `--serviceaccount=ns:name`; binding a ClusterRole with a Role*Binding* scopes it to that namespace.

**Drill 1 (5 min).** Create SA `app-reader` in namespace `default`. Grant it read-only access (get/list/watch) to pods and services in `default` only, using imperative commands. Verify it cannot list pods in `kube-system`.

<details><summary>Solution</summary>

```sh
kubectl create sa app-reader
kubectl create role app-reader --verb=get,list,watch --resource=pods,services
kubectl create rolebinding app-reader --role=app-reader --serviceaccount=default:app-reader
kubectl auth can-i list pods --as=system:serviceaccount:default:app-reader            # yes
kubectl auth can-i list pods -n kube-system --as=system:serviceaccount:default:app-reader  # no
```
</details>

**Drill 2 (5 min).** User `jane` must be able to read (get/list/watch) nodes and PersistentVolumes — cluster-scoped resources. Create the minimal RBAC and verify.

<details><summary>Solution</summary>

```sh
kubectl create clusterrole node-pv-reader --verb=get,list,watch --resource=nodes,persistentvolumes
kubectl create clusterrolebinding jane-node-pv --clusterrole=node-pv-reader --user=jane
kubectl auth can-i list nodes --as=jane          # yes
kubectl auth can-i delete nodes --as=jane        # no
```
Cluster-scoped resources require ClusterRole + Cluster**Role**Binding — a RoleBinding can never grant nodes.
</details>

**Drill 3 (6 min).** A pod in namespace `apps` runs SA `deployer` and reports `Forbidden` when it tries `kubectl rollout restart deployment/api`. An existing Role `deploy-rw` in `apps` grants get/list/update on `deployments`. Find and fix the problem(s) — two are plausible; check both.

<details><summary>Solution</summary>

```sh
# 1. Is the role actually bound to the SA?
kubectl get rolebindings -n apps -o wide          # look for deploy-rw → apps:deployer
# 2. Does the role have the needed verb? rollout restart PATCHes the deployment:
kubectl auth can-i patch deployments.apps -n apps --as=system:serviceaccount:apps:deployer
```
Typical fixes: missing binding → `kubectl create rolebinding deploy-rw --role=deploy-rw --serviceaccount=apps:deployer -n apps`; missing verb → add `patch` to the role (`kubectl edit role deploy-rw -n apps`). The Forbidden message tells you exactly which verb/resource was denied — read it before touching anything.
</details>

## Stretch goals

- Run k9s with the bot's kubeconfig: `KUBECONFIG=$PWD/ci-bot.kubeconfig k9s -n ci`. Watch which views work and which show errors — a visceral demo of `list`+`watch` being what UIs actually need.
- Re-mint with `--duration=30s`, wait, and watch the API reject the expired JWT (`Unauthorized`, 401 — authentication failed, not 403). Now you've seen both failure classes and can tell them apart forever.
- Create a Role with `resourceNames: ["web"]` allowing `delete` on only that deployment; verify the bot can delete `web` but not other deployments.
- Read `kubectl get clusterrolebindings cluster-admin -o yaml` — the `system:masters` group escape hatch your admin cert uses. That group bypasses RBAC entirely at the binding level; this is why cert-based admin credentials are uncomfortable in production (no way to revoke short of rotating the CA).

## Cleanup

```sh
kubectl delete namespace ci          # takes SA, Role, RoleBinding, deployment with it
kubectl delete role app-reader -n default --ignore-not-found
kubectl delete rolebinding app-reader -n default --ignore-not-found
kubectl delete sa app-reader -n default --ignore-not-found
kubectl delete clusterrole node-pv-reader --ignore-not-found
kubectl delete clusterrolebinding jane-node-pv --ignore-not-found
rm -f ci-bot.kubeconfig              # it holds a live (if short-lived) credential
```

Keep `ci-bot-role.yaml` in this folder for reference — you'll write RBAC again for ArgoCD and the observability stack. The `guestbook` namespace stays.
