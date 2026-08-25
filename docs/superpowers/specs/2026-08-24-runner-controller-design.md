# oease runner controller — design

Status: approved 2026-08-24 (persistent runners by default, password-protected UI).

## Goal

Replace KMS + compose scaling with **one container** that owns the whole runner
fleet on a Docker host: `GITHUB_PAT` + a few envs in, N registered runners out,
runtime control (count, labels, image, destroy/recreate) through a small web UI.

## Components

```
controller/            Go, stdlib only
  main.go              env config, wiring, HTTP server, signals
  docker.go            Docker Engine API client over the unix socket
  github.go            GitHub REST client: registration tokens, list/delete runners
  settings.go          /data/settings.json (env seeds it, UI owns it afterwards)
  reconciler.go        desired-state loop, drains, rolling replacement, GC, event log
  web.go + templates/  dashboard, JSON API, basic auth
runner/                unchanged image; entrypoint takes RUNNER_TOKEN from env
```

`kms/`, compose scaling and most of `.env.example` are removed.

## Configuration

Env (bootstrap + secrets, never written to disk): `GITHUB_PAT` (required),
`GITHUB_OWNER` or `RUNNER_REGISTER_TO` (`org` | `owner/repo`), `RUNNER_COUNT`,
`RUNNER_LABELS`, `RUNNER_GROUP`, `RUNNER_IMAGE`, `RUNNER_NAME_PREFIX` (oease),
`ADMIN_PASSWORD`, `PORT` (8080), `DATA_DIR` (/data), `DOCKER_HOST`
(unix:///var/run/docker.sock), `REGISTRY_USERNAME`/`REGISTRY_PASSWORD` (pulls from
a private GHCR), `GITHUB_URL`, `GITHUB_API_URL`, `RECONCILE_INTERVAL` (30s).

`settings.json`: count, labels, group, image, ephemeral, docker_socket (mount the
host socket into runners, default true), work_base (host path; when set each
runner gets `<work_base>/<name>` bound at the identical path so `docker run -v`
inside jobs works), extra_env, graceful_stop_seconds. Created from env on first
start; afterwards the file/UI wins. Invalid file → refuse to start.

## Runner containers

Name `<prefix>-<8 hex>`, hostname = name, labels `dev.oease.gha.managed=true`,
`dev.oease.gha.name`, `dev.oease.gha.generation` (hash of the settings that shape a
runner), env `RUNNER_TOKEN` (fresh registration token), `RUNNER_REGISTER_TO`,
`RUNNER_NAME`, `RUNNER_LABELS`, `RUNNER_GROUP`, `RUNNER_EPHEMERAL`,
`RUNNER_DISABLE_UPDATE` (= ephemeral), `RUNNER_GRACEFUL_STOP_TIMEOUT`, extra env.
Restart policy `unless-stopped` (persistent) / `no` (ephemeral). StopTimeout =
graceful + 60. The entrypoint unsets `RUNNER_TOKEN` before starting the listener and
skips `config.sh` when `.runner` already exists (Docker restarts need no new token).

## Reconcile loop (every RECONCILE_INTERVAL, and after every UI action)

1. Observe: managed containers (Docker), runners (GitHub, best effort).
2. Exited/dead containers: remove + delete their GitHub registration; ephemeral ones
   are simply replaced, persistent ones are recreated (Docker restarts crashes on its
   own; a container we see exited was stopped or gave up).
3. Unhealthy persistent containers (healthcheck = listener process gone) → recreate.
4. members < count → create with fresh tokens; members > count → drain extras,
   idle (GitHub `busy=false`) and oldest first.
5. Stale containers (generation label differs, or container image id ≠ current local
   image id after a pull) → rolling replacement, one at a time, idle first.
6. GitHub GC: offline runners with our prefix and no container → delete.

Drains run in goroutines: stop (graceful timeout; the entrypoint waits for the job),
remove container, delete GitHub registration, then trigger a reconcile. The loop
never blocks on a drain and never crashes on one bad runner; failures go to the
event log (ring buffer of 200) and the UI.

## Web UI / API

`GET /` dashboard: settings form, scale ±, runners table (container state, GitHub
online/offline/busy, age, actions Recreate/Destroy), Pull & roll, events.
JSON: `GET /api/state`, `PUT /api/settings`, `POST /api/scale {delta}`,
`POST /api/runners/{name}/destroy` (also count−1), `POST /api/runners/{name}/recreate`,
`POST /api/pull`. `GET /health` open; everything else behind HTTP basic auth
(username `admin`, `ADMIN_PASSWORD`) when the password is set.

## Testing

Unit tests with a fake Docker Engine API and fake GitHub API: reconcile creates N
with the right spec, scale-down prefers idle, ephemeral replacement, destroy/recreate,
rolling replacement on generation change, GC, settings persistence, auth.
Entrypoint bash tests for the token/skip/graceful-stop paths. Local smoke against the
real Docker daemon with a fake GitHub API.

## Deploy

```yaml
services:
    gha-controller:
        image: ghcr.io/oeasenet/gha-docker-runner/controller:latest
        restart: unless-stopped
        environment: [GITHUB_OWNER=oeasenet, GITHUB_PAT=…, RUNNER_COUNT=3, ADMIN_PASSWORD=…]
        volumes: [/var/run/docker.sock:/var/run/docker.sock, gha-data:/data]
        ports: ["127.0.0.1:8080:8080"]
```

Trade-offs: the controller (and runners with the socket) are root-equivalent on the
host; the controller is a single coordinator — if it is down, runners keep working,
scaling/replacement pauses.
