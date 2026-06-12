# Hints — one nudge per scenario

Open this only after 7–8 minutes of honest effort on a scenario.

**01 — web-pull**
The pod status names the problem class outright. `kubectl describe` the pod and read the *exact* image string in the Events — then compare it letter-by-letter with the images that actually exist on the nodes (you loaded them on Day 15; `docker exec course-worker crictl images | grep podlab`).

**02 — web-restarts**
RESTARTS is climbing, but the app logs look perfectly happy (look at them with `--previous`!). So who is killing it? `kubectl describe pod` → Events. Note *which port* the kubelet is probing, and how forgiving the probe is configured to be.

**03 — web-pending**
A Pending pod always explains itself in its Events. Read the scheduler's message, then ask: how much memory does a kind worker actually have? (`kubectl describe node course-worker | grep -A6 Allocatable`)

**04 — web-noroute**
The pods are Running AND Ready, so stop looking at pods. Follow the traffic path instead: Service → Endpoints → pods. `kubectl get endpoints web-noroute -n gauntlet` — what backs the Service? Compare the Service's selector with the pods' labels, character by character.

**05 — web-config**
The pod never even starts a container — that narrows it enormously. `describe` the pod: the Event names a missing object. Where would the container have gotten its environment from?

**06 — web-notready**
Running, 0/1 Ready, zero restarts, readiness probe failing with "connection refused". The probe config looks right… so is anything actually *listening* on that port? The app announces its configuration in its very first log line. `kubectl logs` and read it.
