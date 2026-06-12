# Day 17 — Troubleshooting Gauntlet I

> **Time:** ~2.5 h · **Builds on:** Days 8, 10, 13, 16

## Objectives

- Internalize a fixed triage loop instead of guessing: get → describe → logs → exec
- Map each pod status (ImagePullBackOff, CrashLoopBackOff, Pending, CreateContainerConfigError, Running-but-broken) to its diagnostic move
- Fix six deliberately broken workloads in place, time-boxed, without peeking
- Calibrate yourself against a CKA-style scoring rubric

## Concepts

### Debugging is a loop, not an inspiration

Under pressure, people flail: re-apply YAML, delete pods, restart "to see if it helps". The professionals you've watched in k9s aren't smarter — they run a fixed loop and let the system confess. The loop:

```
1. kubectl get          What is the STATUS? (the status names the problem class)
        │
2. kubectl describe     What does KUBERNETES say about it? (Events = the system's
        │               own diagnosis: scheduler verdicts, probe failures, pull errors)
3. kubectl logs         What does the APP say? (-p/--previous for the crashed
        │               container — the current one may not have failed yet)
4. kubectl exec/debug   What is TRUE inside? (env, ports, files — verify, don't assume)
```

Each level answers a different question, and you only descend when the level above is clean. Most problems never get past level 2 — **the describe Events section solves more incidents than any other command in Kubernetes.** A useful supplement when events have aged out of `describe`: `kubectl get events -n NS --sort-by=.lastTimestamp | tail`.

### The status decision table

`kubectl get pods` triages for you, if you can read the column:

| STATUS / symptom | It means | First move |
|---|---|---|
| `ImagePullBackOff` / `ErrImagePull` | node can't get the image: typo, missing tag, auth, or never loaded into kind | `describe` → read the exact image string in the event |
| `CrashLoopBackOff` | container starts and dies, kubelet backing off | `logs --previous` (why did the *last* one die?), then `describe` (did a **liveness probe** kill it? exit code?) |
| `Pending` | scheduler can't place it | `describe` → the event lists every node and why it was rejected (resources / taints / affinity — Day 13) |
| `CreateContainerConfigError` | kubelet can't even build the container: missing ConfigMap/Secret reference | `describe` → event names the missing object |
| `ContainerCreating` (stuck) | volumes/CNI/sandbox problems | `describe` → mount or network plugin errors |
| `Running` but 0/1 READY | readiness probe failing — app up but not accepting | `describe` (probe error text) + **app logs** |
| `Running`, READY, but unreachable | the workload is fine; the *wiring* isn't | `kubectl get endpoints` — empty endpoints = selector/port mismatch |
| Climbing RESTARTS, status `Running` | something keeps killing a working app | `describe` events: liveness kills vs OOMKilled (`Last State: ... Reason`) |

Two distinctions worth tattooing somewhere:

- **CrashLoop vs ConfigError**: CrashLoopBackOff means the container *ran and exited* — there are logs to read. CreateContainerConfigError means it *never existed* — there are no logs; only events.
- **Broken pod vs broken route**: if pods are green and users see errors, stop staring at pods. `get endpoints` is the single highest-value command for "it works in the pod but not through the Service" — empty endpoints means selector typo, no ready pods, or wrong port. You will use it today.

### Exit codes and Last State

When a container died, `kubectl describe pod` carries a forensic record under the container's `Last State`:

```
Last State:  Terminated
  Reason:    Error          ← or OOMKilled — read this first
  Exit Code: 1
```

The exit code is the process's dying word: `0` = clean exit (a Job finishing, or a container whose command simply ended — wrong `command:` for a server!), `1`/`2` = app error (go read logs), `137` = SIGKILL (128+9: OOMKilled, or a liveness kill after the grace period), `143` = SIGTERM (128+15: polite shutdown — something *asked* it to stop), `127` = command not found, `126` = not executable. `137` + `Reason: OOMKilled` vs `137` + a liveness-failure event are different incidents with identical exit codes — the Events disambiguate.

### When there's no shell to exec into

podlab is distroless: no shell, no `kubectl exec` (you knew this from Day 2). Level 4 of the loop still works, two ways: run a **separate curl pod** in the namespace and test over the network (today's approach — it also tests the *route*, which exec never does), or attach an **ephemeral debug container** sharing the pod's namespaces:

```sh
kubectl debug -it POD --image=busybox --target=podlab -- sh
# wget -qO- localhost:8080/   ← the app's port, from inside the pod's netns
```

`--target` puts you in the container's process namespace (`ps` shows the app). Nothing about the pod spec changes; the debug container vanishes with the pod.

### k9s as the fast path

Everything above has a k9s gesture: pod list shows STATUS/RESTARTS/READY live; `d` = describe, `l` = logs (then `p` for previous), `e` = edit, `:events` for the event stream. During the gauntlet, running k9s in one pane and applying fixes from another is the realistic workflow — the loop is the same, the keystrokes are just shorter.

### Rules of engagement

The `broken/` folder in this directory contains six manifests. Each deploys "fine" — `kubectl apply` succeeds, YAML is syntactically perfect — and each is broken in a different way. For each one:

- **Time-box: 15 minutes.** At 7–8 min stuck, take the one-line nudge in `HINTS.md`. Only after solving (or 15 min) read `SOLUTIONS.md`.
- **Fix in place** with `kubectl edit` / `kubectl set image` / `kubectl patch` / `kubectl create <the missing thing>` — like production, where "delete everything and re-apply" isn't on the menu. (Editing the file and re-applying is acceptable; deleting the deployment is not.)
- **Done** = the workload healthy *and* doing its job (for the Service scenarios: an actual successful curl).

## Lab

### 0. Warm-up: run the loop on something healthy (5 min)

Calibrate on the guestbook stack so you know what "clean" looks like:

```sh
kubectl get pods -n guestbook                                  # statuses, READY, RESTARTS
kubectl describe pod guestbook-db-0 -n guestbook | tail -8     # healthy Events (or none — events expire after ~1h)
kubectl logs -n guestbook deploy/guestbook --tail=3            # the app's voice
kubectl get endpoints -n guestbook                             # both Services backed
```

Four commands, ~30 seconds, full system picture. That's the cadence to keep under pressure.

### 1. Arm the gauntlet

```sh
kubectl create namespace gauntlet
kubectl apply -f broken/
kubectl get pods -n gauntlet
```

Give it ~60 seconds to settle, then look at the board. You should see a zoo: BackOffs, Pending, ConfigError, deceptively green pods. Resist diagnosing yet — work them **one at a time**, in order, clock running.

A curl pod for testing Services from inside (leave it running all day):

```sh
kubectl run curl -n gauntlet --image=curlimages/curl --restart=Never -- sleep 7200
```

### 2. Scenario 01 — `web-pull` (⏱ 15 min)

Two pods, neither running. Work the loop. Done when: `kubectl get deploy web-pull -n gauntlet` shows `2/2`.

### 3. Scenario 02 — `web-restarts` (⏱ 15 min)

RESTARTS climbing forever. The app is innocent — prove it, then find the guilty party. Done when: pod stays `1/1` for 2+ minutes with restarts no longer increasing.

### 4. Scenario 03 — `web-pending` (⏱ 15 min)

Nothing crashes; nothing starts either. Done when: pod `Running` and `1/1`.

### 5. Scenario 04 — `web-noroute` (⏱ 15 min)

Pods green across the board, but:

```sh
kubectl exec -n gauntlet curl -- curl -sS --max-time 5 http://web-noroute.gauntlet.svc/
# hangs, then: connection timed out
```

Find out why customers get nothing from a perfectly healthy app. Done when that curl returns podlab's JSON.

### 6. Scenario 05 — `web-config` (⏱ 15 min)

A status you may not have met before. The fix does not involve editing the Deployment (though that's also a legal route). Done when: pod `1/1`.

### 7. Scenario 06 — `web-notready` (⏱ 15 min)

The subtle one. Running, never Ready, zero restarts, and describe's probe error looks identical to scenario 02's. The difference is one level deeper in the loop. Done when:

```sh
kubectl exec -n gauntlet curl -- curl -sS http://web-notready.gauntlet.svc/
```

returns JSON.

### 8. Final inspection

```sh
kubectl get all -n gauntlet
```

Everything Running/Ready/`x/x`, no BackOffs, both Services with endpoints:

```sh
kubectl get endpoints -n gauntlet
```

### 9. Debrief (10 min — don't skip)

For each scenario, write three lines in a scratch file: *symptom → revealing command → root cause*. Then compare against the meta-lesson table at the bottom of `SOLUTIONS.md`. The scenarios where your revealing command differed from the canonical one are your gaps — those are the two to re-run cold next week. This habit (a written one-page debrief per incident) is also exactly what separates seniors from firefighters on real teams.

## Verify ✅

- [ ] `kubectl get deploy -n gauntlet` → all six deployments `READY` equal to desired (`2/2`, `1/1`, `1/1`, `2/2`, `1/1`, `1/1`)
- [ ] `kubectl get pods -n gauntlet` → zero pods in any BackOff/Error/Pending state; restart counters stable for 5 minutes
- [ ] `kubectl get endpoints web-noroute web-notready -n gauntlet` → both show pod IPs, neither `<none>`
- [ ] `kubectl exec -n gauntlet curl -- curl -sS http://web-noroute.gauntlet.svc/` → podlab JSON
- [ ] `kubectl exec -n gauntlet curl -- curl -sS http://web-notready.gauntlet.svc/` → podlab JSON
- [ ] `kubectl get events -n gauntlet --sort-by=.lastTimestamp | tail -15` → no fresh warnings (old ones from the broken phase are fine)

## CKA corner 🎓

Today *was* CKA practice — troubleshooting is the heaviest-weighted exam domain (~30%), and these six are its bread-and-butter question shapes. Exam translation: you're given a namespace and a complaint ("the application is not reachable"), and the points are for the *fix being in effect*, not for elegance. `kubectl edit` is your friend; nobody grades your YAML style.

**Scoring rubric** — grade yourself per scenario:

| Points | Criteria |
|---|---|
| 4 | Fixed within 15 min, no hint |
| 3 | Fixed within 15 min, with hint |
| 2 | Fixed after time-box or after reading the solution's *revealing command* (not the fix) |
| 1 | Needed the full solution |
| +1 bonus | You ran the revealing command *first* (your loop found the signal immediately) |

| Total (of 24+) | Calibration |
|---|---|
| 20+ | Exam-ready on this domain; Day 28's gauntlet will need to try harder |
| 14–19 | Solid; redo your weakest two scenarios from scratch tomorrow |
| < 14 | No problem — re-run the whole gauntlet in 3 days (`kubectl delete ns gauntlet`, re-apply); repetition is the entire trick |

**Drill 1 — speed round (5 min).** Without applying anything, write down the *first command* you'd run for each complaint: (a) "deploy says 0/3 ready", (b) "service worked yesterday, times out today", (c) "pod restarts every ~30 seconds", (d) "pod stuck Pending for an hour".

<details><summary>Solution</summary>

(a) `kubectl get pods -n NS` — which state are the three in? Everything follows from STATUS.
(b) `kubectl get endpoints SVC -n NS` — empty endpoints is the 80% case; then check readiness.
(c) `kubectl describe pod P -n NS` — Events tell you liveness-kill vs OOMKilled vs crash; then `logs --previous`.
(d) `kubectl describe pod P -n NS` — read the scheduler event; it enumerates every node's rejection reason.
</details>

**Drill 2 — exit-code flashcards (3 min).** A container shows `Last State: Terminated`. Diagnose from the code alone: (a) `Exit Code: 137, Reason: OOMKilled`, (b) `Exit Code: 1`, (c) `Exit Code: 0` on a Deployment pod that keeps "crash"-looping, (d) `Exit Code: 127`.

<details><summary>Solution</summary>

(a) memory limit hit — raise the limit or fix the leak (Day 8's lab, in the wild).
(b) the app errored on its own — `kubectl logs --previous` has the stack trace.
(c) the process exited *successfully* — the container's command isn't a long-running server (typo'd `command:`, or a one-shot script in a Deployment that should be a Job).
(d) command not found — wrong `command:`/entrypoint path for this image.
</details>

## Stretch goals

- Re-break scenario 06 the *other* way: set probe/targetPort to 9090 but remove the env — diagnose from the opposite direction.
- Design a seventh breakage for a friend (or future you): something this README's decision table does *not* directly cover. Candidates: a NetworkPolicy in the namespace blocking the curl pod (Day 15 knowledge), an `imagePullPolicy: Always` on a kind-loaded image, a Secret mounted with a wrong key name.
- Run the entire gauntlet again using **only k9s** — no raw kubectl. Time yourself; compare.

## Cleanup

```sh
kubectl delete namespace gauntlet
```

Everything today was disposable. Do **not** delete the `broken/` directory — Day 28's gauntlet assumes you can re-run this one for practice, and re-running it cold in a week is genuinely worth an hour. The guestbook namespace stays, as always.
