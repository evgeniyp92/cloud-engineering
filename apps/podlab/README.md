# podlab — the course's workhorse demo app

A single small Go HTTP service whose only job is to make Kubernetes behavior
**observable**. Every endpoint exists to prove a specific concept from a lesson.

## Build & load into kind

```sh
docker build -t podlab:v1 apps/podlab
kind load docker-image podlab:v1 --name course
```

Build different "releases" for rollout lessons by changing the tag and env:

```sh
docker build -t podlab:v2 apps/podlab   # same binary; VERSION/COLOR come from env at runtime
```

## Endpoints

| Endpoint | Method | What it proves |
|---|---|---|
| `/` | GET | Identity banner: hostname, pod IP, node, namespace, `VERSION`, `COLOR`. Use it to see load-balancing across pods and which "release" served you. |
| `/config` | GET | Dumps all env vars and the contents of every file under `CONFIG_DIR` (default `/etc/podlab`). **This is how you verify a ConfigMap/Secret mount or override actually landed.** |
| `/healthz` | GET | 200 when healthy, 503 when toggled off. Wire this to liveness/readiness probes. |
| `/healthz/toggle` | POST | Flips health on/off so you can watch probes evict/restart the pod. |
| `/load?seconds=n` | GET | Burns CPU for `n` seconds (default 10, max 120). Drives HPA and limit/QoS demos. |
| `/error?rate=0.5` | GET | Returns 500s at the given probability. Makes a "bad release" fail canary analysis. |
| `/metrics` | GET | Prometheus metrics: `podlab_http_requests_total`, `podlab_http_request_duration_seconds`, `podlab_build_info`. |

## Configuration (all via env)

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `VERSION` | `dev` | Shown on `/` and in `podlab_build_info` — set per release |
| `COLOR` | `none` | Shown on `/` — handy for blue/green and canary demos |
| `CONFIG_DIR` | `/etc/podlab` | Directory dumped by `/config` — mount ConfigMaps/Secrets here |
| `POD_IP`, `NODE_NAME`, `POD_NAMESPACE` | — | Inject via the Downward API (Day 2) |

## Behavior worth knowing

- Logs are **structured JSON on stdout** — exactly what Loki/LogQL lessons query.
- On SIGTERM it logs `"signal received, draining connections"`, drains for up to
  15 s, then logs `"shutdown complete"` — watch this during the graceful-shutdown
  lesson (Day 10) and during rolling updates.
- The image is distroless and runs as `nonroot`: no shell inside. Debugging it
  requires ephemeral containers (`kubectl debug`) — that's intentional (Day 2, Day 41).
