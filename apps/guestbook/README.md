# guestbook — the course's stateful demo app

A minimal API + Postgres pair. It exists because some lessons need **state**:

- **Day 11 (StatefulSets & storage)** — Postgres runs as a StatefulSet with a PVC.
- **Day 15 (NetworkPolicies)** — api → db traffic is something real to allow/deny.
- **Day 43 (Velero)** — back up the namespace, destroy it, restore it, and check
  whether your entries survived.

## Build & load into kind

```sh
docker build -t guestbook:v1 apps/guestbook
kind load docker-image guestbook:v1 --name course
```

Postgres itself uses the public `postgres:16` image — you deploy it yourself in
the Day 11 lab (that's the lesson).

## Endpoints

| Endpoint | Method | Notes |
|---|---|---|
| `/entries` | GET | Last 50 entries, newest first |
| `/entries` | POST | Body: `{"message": "hello"}` → 201 with the created row |
| `/healthz` | GET | Liveness: always 200 while the process runs |
| `/readyz` | GET | Readiness: 503 until the database is reachable — the classic "alive ≠ ready" demo |

## Configuration

| Var | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | `postgres://guestbook:guestbook@guestbook-db:5432/guestbook?sslmode=disable` | Postgres DSN — in lessons you assemble this from a Secret |
| `PORT` | `8080` | Listen port |

It creates its own `entries` table on startup (retrying until the DB is up), so
there is no migration step to manage.
