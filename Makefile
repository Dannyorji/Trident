.PHONY: all dev stop db migrate indexer grpc-api go-api sdk-build test lint lint-openapi help

# Load environment variables from .env if it exists
ifneq (,$(wildcard .env))
    include .env
    export
endif

# Default target
all: dev

help: ## Show this help message
	@echo ""
	@echo "Trident — Soroban Event Indexer for Stellar"
	@echo ""
	@echo "Usage: make <target>"
	@echo ""
	@echo "Targets:"
	@echo "  dev          Start the full development stack (DB + migrate + all services)"
	@echo "  stop         Stop all Docker containers"
	@echo "  db           Start only Postgres and Redis via Docker Compose"
	@echo "  migrate      Apply database migrations (requires sqlx-cli or psql)"
	@echo "  indexer      Run the Rust indexer with dev env vars"
	@echo "  grpc-api     Run the Rust gRPC API with dev env vars"
	@echo "  go-api       Run the Go REST API"
	@echo "  sdk-build    Build the TypeScript and React SDKs"
	@echo "  test         Run all unit tests (integration tests require TEST_DATABASE_URL)"
	@echo "  lint         Run all linters (cargo fmt, clippy, go vet, tsc)"
	@echo "  lint-openapi Run Spectral linter on OpenAPI spec"
	@echo "  deploy       Deploy all services to Fly.io (requires flyctl)"
	@echo "  help         Show this help message"
	@echo ""

dev: db migrate
	@echo "Starting indexer, grpc-api, and go-api..."
	@trap 'kill 0' INT TERM EXIT; \
	cargo run --bin trident-indexer 2>&1 | sed -e 's/^/[indexer] /' & \
	cargo run --bin trident-api 2>&1 | sed -e 's/^/[grpc-api] /' & \
	cd services/api && go run main.go 2>&1 | sed -e 's/^/[go-api] /' & \
	wait

stop:
	docker compose -f docker/docker-compose.dev.yml down

db:
	docker compose -f docker/docker-compose.dev.yml up -d
	@echo "Waiting for PostgreSQL to be healthy..."
	@until docker exec $$(docker compose -f docker/docker-compose.dev.yml ps -q postgres) pg_isready -U trident -d trident >/dev/null 2>&1; do \
		sleep 1; \
	done
	@echo "PostgreSQL is healthy!"

migrate:
	@echo "Applying database migrations..."
	@if command -v sqlx >/dev/null 2>&1; then \
		sqlx db create --database-url "$(DATABASE_URL)" || true; \
		sqlx migrate run --database-url "$(DATABASE_URL)" --source database/migrations; \
	else \
		echo "sqlx-cli not found, attempting raw psql migrations..."; \
		psql "$(DATABASE_URL)" -f database/schema.sql; \
		for f in database/migrations/*.sql; do \
			echo "Applying $$f..."; \
			psql "$(DATABASE_URL)" -f "$$f"; \
		done; \
	fi

indexer:
	cargo run --bin trident-indexer

grpc-api:
	cargo run --bin trident-api

go-api:
	cd services/api && go run main.go

sdk-build:
	cd sdk/typescript && npm install && npm run build
	cd sdk/react && npm install && npm run build

test:
	cargo test --all
	cd services/api && go test ./...
	cd sdk/go && go test ./...
	cd sdk/python && pip install -e ".[dev]" && pytest -q
	cd sdk/typescript && npm install && npm run build && npm run test
	cd sdk/react && npm install && npm run test

deploy: ## Deploy all services to Fly.io in dependency order (requires flyctl)
	@echo "Deploying gRPC API..."
	fly deploy -c fly/grpc-api.toml --remote-only
	@echo "Deploying Indexer..."
	fly deploy -c fly/indexer.toml --remote-only
	@echo "Deploying Go REST API..."
	fly deploy -c fly/api.toml --remote-only
	@echo "All services deployed."

lint:
	cargo fmt --all -- --check
	cargo clippy --all-targets --all-features -- -D warnings
	cd services/api && go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd services/api && golangci-lint run; \
	fi
	cd sdk/typescript && npm install && npm run lint
	cd sdk/react && npm install && npm run lint

lint-openapi: ## Lint the OpenAPI specification
	@if command -v spectral >/dev/null 2>&1; then \
		spectral lint api/openapi.yaml --ruleset spectral:oas; \
	else \
		echo "spectral CLI not found. Install with: npm install -g @stoplight/spectral-cli"; \
		exit 1; \
	fi

# Coverage (issue #325). Mirrors the `coverage` job in .github/workflows/ci.yml
# so a local run and CI report the same numbers from the same tools.
#
# cargo-llvm-cov, not tarpaulin: llvm-cov uses the compiler's own
# instrumentation, so it reports the regions rustc actually sees.

coverage: coverage-rust coverage-go coverage-sdk ## Collect coverage for Rust, Go, and the SDKs

coverage-rust: ## Rust workspace coverage (HTML + lcov in target/llvm-cov)
	@command -v cargo-llvm-cov >/dev/null 2>&1 || { \
		echo "cargo-llvm-cov not found. Install with: cargo install cargo-llvm-cov"; \
		exit 1; \
	}
	cargo llvm-cov --workspace --html --lcov --output-path lcov.info -- --test-threads=1
	@echo "HTML report: target/llvm-cov/html/index.html"

coverage-go: ## Go API coverage (HTML in services/api/coverage.html)
	cd services/api && go test ./... -coverprofile=coverage.out
	cd services/api && go tool cover -html=coverage.out -o coverage.html
	cd services/api && go tool cover -func=coverage.out | tail -1

coverage-sdk: ## TypeScript and Python SDK coverage
	cd sdk/typescript && npm run test -- --coverage || \
		echo "note: install @vitest/coverage-v8 for TypeScript coverage"
	cd sdk/python && pytest -q --cov --cov-report=term --cov-fail-under=0

# Enforce the floors CI enforces. Runs the measurement itself rather than
# depending on a profile a previous target may or may not have produced —
# the previous version read services/api/coverage.out without generating it,
# so a clean tree failed on a missing file rather than on coverage.
#
# Floors are set from measured baselines, not aspiration; see the `coverage`
# job in .github/workflows/ci.yml for the rationale and current numbers.
coverage-check: ## Enforce coverage floors on MVP-critical packages
	@set -e; \
	cd services/api; \
	echo "Checking Go critical-package coverage floors..."; \
	fail=0; \
	for spec in "handlers:43" "middleware:66" "cursor:90" "validation:95"; do \
		pkg=$${spec%%:*}; floor=$${spec##*:}; \
		actual=$$(go test ./$$pkg -coverprofile=cov-$$pkg.out 2>/dev/null \
			| grep -oE 'coverage: [0-9.]+%' | grep -oE '[0-9.]+' || echo 0); \
		if awk -v a="$$actual" -v f="$$floor" 'BEGIN { exit !(a + 0 < f + 0) }'; then \
			echo "  FAIL $$pkg: $$actual% is below the $$floor% floor"; fail=1; \
		else \
			echo "  ok   $$pkg: $$actual% (floor $$floor%)"; \
		fi; \
	done; \
	exit $$fail
