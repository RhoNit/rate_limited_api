# Rate-Limited API (Go + Echo)

A production-considerate HTTP service that accepts requests from identified
users and enforces a **per-user sliding-window rate limit**, backed by
**MySQL** for distributed state and **RabbitMQ** for retry queueing of
rate-limited requests.

## API Contract


| Method | Path       | Purpose                                                       |
| ------ | ---------- | ------------------------------------------------------------- |
| `POST` | `/request` | Submit a request. Counts against the caller's quota.          |
| `GET`  | `/stats`   | Per-user counters + queue stats. Optional `?user_id=` filter. |


---

### `POST /request`

**Request**

```
POST /request
Content-Type: application/json
```

```json
{
  "user_id": "Ranit",
  "payload": { "key": "value" }
}
```


| Field     | Type           | Required | Notes                          |
| --------- | -------------- | -------- | ------------------------------ |
| `user_id` | string         | yes      | Non-blank identifier           |
| `payload` | any JSON value | yes      | Forwarded as-is; not validated |


---

#### Case 1 — Allowed → `200 OK`

```json
{
  "status": "accepted",
  "user_id": "Ranit",
  "remaining": 4,
  "limit": 5,
  "reset_at_epoch": 1776752062.381
}
```


| Field            | Type   | Notes                                       |
| ---------------- | ------ | ------------------------------------------- |
| `status`         | string | Always `"accepted"`                         |
| `user_id`        | string |                                             |
| `remaining`      | int    | Requests left in the current window         |
| `limit`          | int    | Configured max requests per window          |
| `reset_at_epoch` | float  | Unix epoch (seconds) when the window resets |


Response headers set on every `200`:

```
X-RateLimit-Limit:     5
X-RateLimit-Remaining: 4
X-RateLimit-Reset:     1776752062.381
```

---

#### Case 2 — Rate-limited, queue **disabled** → `429 Too Many Requests`

```json
{
  "error": "rate_limit_exceeded",
  "message": "limit is 5 per 60s",
  "retry_after_seconds": 42.8,
  "reset_at_epoch": 1776752062.381
}
```


| Field                 | Type   | Notes                                       |
| --------------------- | ------ | ------------------------------------------- |
| `error`               | string | Always `"rate_limit_exceeded"`              |
| `message`             | string | Human-readable description                  |
| `retry_after_seconds` | float  | Seconds until the window reopens            |
| `reset_at_epoch`      | float  | Unix epoch (seconds) when the window resets |


Additional headers on `429`:

```
X-RateLimit-Limit:     5
X-RateLimit-Remaining: 0
X-RateLimit-Reset:     1776752062.381
Retry-After:           43
```

---

#### Case 3 — Rate-limited, queue **enabled** → `202 Accepted`

The request is enqueued in RabbitMQ and will be retried once the rate-limit window reopens.

```json
{
  "status": "queued",
  "job_id": "a3f1b2c4d5e6f7a8",
  "user_id": "Ranit",
  "retry_after_seconds": 42.8
}
```


| Field                 | Type   | Notes                                       |
| --------------------- | ------ | ------------------------------------------- |
| `status`              | string | Always `"queued"`                           |
| `job_id`              | string | Random hex ID; can be used to track the job |
| `user_id`             | string |                                             |
| `retry_after_seconds` | float  | Expected delay before the consumer retries  |


> The consumer will call `CheckAndRecord` again after the TTL fires. If the window is still full (burst / clock skew) it re-queues with exponential backoff, up to `MaxAttempts = 3`. After that the job is dropped and `dropped` counter increments.

---

#### Case 4 — Validation error → `400 Bad Request`

Triggered when `user_id` is missing/blank, `payload` is missing, or the body is not valid JSON.

```json
{
  "error": "validation_error",
  "message": "user_id is required"
}
```


| `error` value      | Cause                  |
| ------------------ | ---------------------- |
| `invalid_json`     | Body is not valid JSON |
| `validation_error` | Missing / blank field  |


---

### `GET /stats`

**Request**

```
GET /stats
GET /stats?user_id=Ranit     ← filter to a single user
```

**Response `200 OK`**

```json
{
  "backend": "mysql",
  "limit": 5,
  "window_seconds": 60,
  "users": [
    {
      "user_id": "Ranit",
      "allowed_requests": 5,
      "rejected_requests": 2,
      "current_window_count": 3
    }
  ],
  "queue": {
    "queued": 2,
    "processed": 1,
    "dropped": 0
  }
}
```


| Field            | Type          | Notes                                 |
| ---------------- | ------------- | ------------------------------------- |
| `backend`        | string        | `"memory"` or `"mysql"`               |
| `limit`          | int           | Configured max requests per window    |
| `window_seconds` | int           | Window duration in seconds            |
| `users`          | array         | One entry per user seen since startup |
| `queue`          | object | null | `null` when `QUEUE_ENABLED=false`     |


`**users[*]` fields**


| Field                  | Type   | Notes                                       |
| ---------------------- | ------ | ------------------------------------------- |
| `user_id`              | string |                                             |
| `allowed_requests`     | int64  | Lifetime allowed count                      |
| `rejected_requests`    | int64  | Lifetime rejected count                     |
| `current_window_count` | int    | Active timestamps inside the current window |


`**queue` fields** (present only when `QUEUE_ENABLED=true`)


| Field       | Type  | Notes                                      |
| ----------- | ----- | ------------------------------------------ |
| `queued`    | int64 | Total jobs published since startup         |
| `processed` | int64 | Jobs successfully retried by the consumer  |
| `dropped`   | int64 | Jobs discarded after `MaxAttempts` retries |


## Configuration (environment variables)


| Variable                    | Default                                     | Notes                                              |
| --------------------------- | ------------------------------------------- | -------------------------------------------------- |
| `HTTP_ADDR`                 | `:8000`                                     |                                                    |
| `RATE_LIMIT_BACKEND`        | `memory`                                    | `memory` or `mysql`                                |
| `RATE_LIMIT_MAX_REQUESTS`   | `5`                                         | Requests allowed per window                        |
| `RATE_LIMIT_WINDOW_SECONDS` | `60`                                        | Window length in seconds                           |
| `MYSQL_DSN`                 | `root:secret@tcp(localhost:3306)/ratelimit` | `parseTime=true` appended automatically if missing |
| `QUEUE_ENABLED`             | `false`                                     | Set `true` to enable RabbitMQ retry                |
| `AMQP_URL`                  | `amqp://guest:guest@localhost:5672/`        | Used when `QUEUE_ENABLED=true`                     |


## How to run

There are two fully supported paths. **Both start from the same
`config.yml`** — Docker Compose just overrides the service hostnames via
environment variables so the API finds MySQL and RabbitMQ inside the
Docker network instead of on `localhost`.

---

### Path A — Local (no Docker for the API)

Configuration is read from `**config.yml**`. Edit that file or set env
vars to switch modes. The binary itself never needs Docker.

#### 1. In-memory — zero external dependencies

The default `config.yml` already has `backend: memory` and
`queue.enabled: false`. Just:

```bash
go run ./cmd/server
# or
make run
```

#### 2. MySQL + RabbitMQ — start dependencies in Docker, run API on the host (DEV mode)

```bash
make dev-up        # starts MySQL (:3306) and RabbitMQ (:5672) in Docker
```

Edit `config.yml` (or export env vars):

```yaml
rate_limit:
  backend: "mysql"      # was "memory"

queue:
  enabled: true         # was false
```

Then run the API:

```bash
make run-full
# equivalent to: RATE_LIMIT_BACKEND=mysql QUEUE_ENABLED=true go run ./cmd/server
```

To stop the dependencies:

```bash
make dev-down
```

> **Tip:** You can also mix and match — `make run-mysql` uses MySQL without
> the queue, keeping `queue.enabled: false` in `config.yml`.

---

### Path B — Full Docker stack

Everything (API + MySQL + RabbitMQ) runs in Docker. `docker-compose.yml`
overrides the MySQL DSN and RabbitMQ URL to use Docker service hostnames
(`mysql`, `rabbitmq`) instead of `localhost`.

```bash
make compose-up
# API:            http://localhost:8000
# RabbitMQ UI:    http://localhost:15672  (guest / guest)
```

To stop and remove all containers and volumes:

```bash
make compose-down
```

You can override limit or window without touching any file:

```bash
RATE_LIMIT_MAX_REQUESTS=10 RATE_LIMIT_WINDOW_SECONDS=30 docker compose up --build
```

---

### Smoke test (works for both paths)

```bash
# Hit the limit
for i in $(seq 1 6); do
  curl -s -w " HTTP %{http_code}\n" -XPOST localhost:8000/request \
    -H 'content-type: application/json' \
    -d '{"user_id":"Ranit","payload":{"i":'$i'}}'
done

# View stats
curl -s localhost:8000/stats | python3 -m json.tool
```

Expected: first 5 requests return `200`, the 6th returns `429` (or `202`
when queue is enabled).

---

### Tests

```bash
make test        # unit + handler tests
make test-race   # same, with the race detector (recommended)
make cover       # HTML coverage report → coverage.html
```

## Design decisions

### Algorithm: sliding-window log

Fixed-window counters allow up to `2 × limit` requests at the window
boundary. The sliding-window log evicts timestamps older than the window
before counting, so the cap is honoured at **every instant in time**. Memory
per user is naturally bounded at `limit` timestamps.

### MySQL: per-user lock row for atomic check-and-record

A naïve `SELECT COUNT(*) … INSERT` in separate statements is not atomic:
two concurrent goroutines can both read `count = 4` (under a 5-request cap)
and both insert, yielding `count = 6`.

The fix uses an `InnoDB` row-level lock:

1. `INSERT IGNORE INTO rate_limit_user_lock (user_id)` — creates the lock row
  once, auto-committed, idempotent.
2. `BEGIN`
3. `SELECT user_id FROM rate_limit_user_lock WHERE user_id = ? FOR UPDATE` —
  acquires an exclusive row lock. Every other transaction for the **same**
   user blocks here; different users never contend.
4. `DELETE` expired timestamps, `SELECT COUNT(*)`, insert if under cap.
5. `COMMIT` — releases the lock.

This gives the same atomicity guarantee as the in-memory `sync.Mutex` per
user, but across multiple API pods.

### RabbitMQ retry queue — TTL + Dead Letter Exchange

When `QUEUE_ENABLED=true` and a request is rate-limited the handler returns
**202 Accepted** and publishes the job to `ratelimit.waiting` with a
per-message TTL equal to `retry_after_ms`. RabbitMQ holds the message until
the TTL fires, then dead-letters it to `ratelimit.retry`. The consumer calls
`CheckAndRecord` again — the window should have opened by then. If not
(clock skew / burst), the message is re-queued with exponential backoff up to
`MaxAttempts = 3`.

```
POST /request (rate-limited)
   │
   └─► ratelimit.waiting  (TTL = retry_after_ms)
              │ expires
              ▼
        ratelimit.retry  ◄── consumer goroutine
              │
              └─► CheckAndRecord → allowed → ack
                               → still limited → re-queue (×MaxAttempts)
```

Reconnect: the consumer monitors the AMQP connection via `NotifyClose` and
re-establishes with exponential backoff (1 s → 2 s → … → 30 s).

### Pluggable backend

`limiter.Limiter` is a small interface. `limiter.Build` is a factory keyed on
`RATE_LIMIT_BACKEND`. Handlers and the queue consumer depend only on the
interface, so swapping backends (or adding PostgreSQL, DynamoDB, etc.) is
mechanical.

### Other production-considerate touches

- Graceful shutdown on `SIGINT`/`SIGTERM` with a 10 s drain.
- HTTP read/write timeouts (slow-loris protection).
- Body-size limit middleware + per-payload check (defence in depth).
- Echo `Recover` middleware converts panics to 500.
- Structured access logs with per-request `X-Request-Id`.
- MySQL auto-migration on startup (idempotent `CREATE TABLE IF NOT EXISTS`).
- Connection pool tuning (`MaxOpenConns`, `MaxIdleConns`, `ConnMaxLifetime`).
- Distroless, non-root Docker image.

## Limitations and what I'd improve with more time

1. **Idle-user GC in the MySQL backend.** `rate_limit_log` accumulates rows
  indefinitely; a periodic cleanup job should drop rows for users inactive
   for `N × window`. A MySQL Event or an application-side cron would work.
2. **Job result delivery.** Queued jobs are fire-and-forget — the original
  HTTP caller gets a `job_id` in the 202 but has no way to poll the result.
   A `GET /jobs/{job_id}` endpoint backed by a jobs table would close this
   gap; alternatively use a webhook/callback URL in the request body.
3. **Per-user / per-route limits.** Currently one global limit applies to
  every user. Hierarchical config (global default + per-user overrides from a
   config service) would suit a real product.
4. **Authentication.** `user_id` is taken from the request body; any caller
  can impersonate another. In production the limit key should be an
   authenticated identity (JWT `sub`, API-key hash, etc.).
5. **Observability.** Prometheus counters (`rl_allowed_total`,
  `rl_rejected_total`, `rl_queued_total`, `rl_check_latency_seconds`) would
   make capacity planning and alerting trivial.
6. **Queue consumer idempotency.** If the consumer crashes after calling
  `CheckAndRecord` but before acking, the message is redelivered and the
   request is double-counted. Adding a `processed_jobs` table with an
   `INSERT IGNORE` on `job_id` before every `CheckAndRecord` call would
   prevent duplicates.
7. **Cloud deployment.** The service is designed for stateless horizontal
  scaling (MySQL backend + RabbitMQ). An Azure Container Apps or AWS ECS
   Fargate deployment manifest would be a natural next step.

## Project layout

```
.
├── cmd/server/              # main() — wires config, limiter, queue, Echo
├── internal/
│   ├── handler/             # Echo handlers (+ tests)
│   ├── config/              # Viper-based config loader (config.yml + env vars)
│   ├── limiter/             # Limiter interface, memory + MySQL backends (+ tests)
│   ├── model/               # Request / response types
│   └── queue/               # RabbitMQ manager (publisher + consumer)
├── Dockerfile               # multi-stage, distroless, non-root
├── docker-compose.yml       # API + MySQL + RabbitMQ
├── Makefile
├── go.mod / go.sum
└── README.md
```

