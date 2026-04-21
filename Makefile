# ── Rate-Limited API ────────────────────────────────────────────────────────
#
# Local (no Docker):
#   make run           in-memory backend, no external deps
#   make dev-up        start MySQL + RabbitMQ in Docker (deps only)
#   make run-mysql     MySQL backend  (requires: make dev-up)
#   make run-full      MySQL + RabbitMQ + retry queue  (requires: make dev-up)
#   make dev-down      stop and remove dev containers
#
# Full Docker stack:
#   make compose-up    build image + start API + MySQL + RabbitMQ
#   make compose-down  stop and remove all containers + volumes
#
# Other:
#   make test          run all tests
#   make test-race     run all tests with race detector
#   make cover         generate HTML coverage report
#   make build         compile a static binary to ./bin/server
#   make lint          run go vet
#   make tidy          tidy go.mod / go.sum
# ────────────────────────────────────────────────────────────────────────────

.PHONY: run run-mysql run-full \
        dev-up dev-down \
        compose-up compose-down \
        test test-race cover build lint tidy docker-build

# ── Local run targets ───────────────────────────────────────────────────────

# Reads config.yml as-is (backend=memory, queue disabled). Zero deps.
run:
	go run ./cmd/server

# MySQL backend. Reads all other values from config.yml.
# Requires: make dev-up  (or a locally running MySQL on :3306)
run-mysql:
	RATE_LIMIT_BACKEND=mysql go run ./cmd/server

# MySQL backend + RabbitMQ retry queue.
# Requires: make dev-up  (or locally running MySQL on :3306 and RabbitMQ on :5672)
run-full:
	RATE_LIMIT_BACKEND=mysql QUEUE_ENABLED=true go run ./cmd/server

# ── Dev dependency management ────────────────────────────────────────────────

# Start MySQL and RabbitMQ in Docker (API runs on the host via go run).
dev-up:
	docker compose -f docker-compose.dev.yml up -d
	@echo ""
	@echo "  MySQL     → localhost:3306  (user: app / secret)"
	@echo "  RabbitMQ  → localhost:5672  (amqp)"
	@echo "  RabbitMQ UI → http://localhost:15672  (guest/guest)"
	@echo ""
	@echo "  Now run: make run-mysql   or   make run-full"

dev-down:
	docker compose -f docker-compose.dev.yml down -v

# ── Full Docker stack ────────────────────────────────────────────────────────

# Build the Docker image and start API + MySQL + RabbitMQ.
compose-up:
	docker compose up --build

compose-down:
	docker compose down -v

docker-build:
	docker build -t rate-limited-api:latest .

# ── Development ──────────────────────────────────────────────────────────────

tidy:
	go mod tidy

lint:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage report: coverage.html"

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -o bin/server ./cmd/server
