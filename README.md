<div align="center">

# 🔱 Trident

**Soroban Event Indexer for Stellar**

[![Status: Pre-Alpha](https://img.shields.io/badge/Status-Pre--Alpha-orange?style=flat-square)]()
[![License: MIT](https://img.shields.io/badge/License-MIT-blue?style=flat-square)](./LICENSE)
[![Built with Rust](https://img.shields.io/badge/Built%20with-Rust-orange?style=flat-square&logo=rust)](https://www.rust-lang.org/)
[![Network: Stellar](https://img.shields.io/badge/Network-Stellar%20%2F%20Soroban-black?style=flat-square)](https://stellar.org)

*The indexing layer Stellar's developer ecosystem needs.*

> ⚠️ Trident is in active pre-development. The codebase is not yet public. Watch this repo for updates.

</div>

---

## Quick Start

> ⚡ **Evaluating Trident?** Walk through the **[10-Minute Testnet Developer Quickstart](docs/QUICKSTART.md)** to get from zero to your first indexed event query with no prior setup.

### Prerequisites
Before running Trident locally, make sure you have the following installed:
- **Docker** with Compose v2
- **Rust** (via [rustup](https://rustup.rs))
- **Go** (1.21+)
- **Node.js** (20 LTS+)

### Setup & Run
Get the entire development stack running in seconds:
```bash
cp .env.example .env
make dev
```

This command will:
1. Start Postgres and Redis via Docker Compose.
2. Wait for Postgres to be healthy.
3. Apply all database migrations automatically.
4. Compile and start the Rust indexer, the Rust gRPC API, and the Go REST API.

Use `Ctrl+C` or `make stop` to cleanly shut down all services. For the full Docker-only flow, see [QUICKSTART.md](docs/QUICKSTART.md).

---

## The Problem


Soroban's RPC node is intentionally thin — no long-term event storage, no historical queries, no filtering. That's a reasonable protocol decision, but it forces every developer building on Stellar to solve the same infrastructure problem before they can build their actual product. Every serious team ends up writing their own event streaming pipeline, their own database schema, their own parser — in isolation, with no shared guarantees, and no easy recovery when something breaks.

Every mature smart contract ecosystem has solved this exactly once:

| Ecosystem | Solution |
|-----------|----------|
| Ethereum  | The Graph |
| Solana    | Helius / Triton |
| Cosmos    | SubQuery |
| **Stellar** | **Trident** |

---

## What Trident Does

Trident is a dedicated indexing layer that streams every Soroban contract event off the network, stores it persistently, and exposes it through a clean API. A developer using Trident can query every event a contract has ever emitted — filtered by topic, paginated, in real time or historically — without writing a single line of indexing infrastructure themselves.

---

## Architecture

The system is split into two layers with a hard boundary between them. Everything from ingestion to storage is handled by a **Rust core** — chosen because it decodes XDR natively through the same libraries the Stellar protocol uses, has no garbage collector to introduce latency spikes, and gives the kind of predictable performance a 24/7 indexer demands. The **Go front office** sits in front of that, serving the REST, GraphQL, and WebSocket interfaces that developers actually interact with.

<img width="1400" height="1000" alt="trident-architecture" src="https://github.com/user-attachments/assets/91b8fddf-4837-406e-a76d-d44d0b963005" />


The Rust gRPC server polls Soroban RPC on a short interval, decodes every event from XDR into a normalised record, and writes it to PostgreSQL. It also publishes each event into Redis Streams — a persistent, ordered log that the Go layer consumes to power real-time WebSocket subscriptions. This separation is intentional: historical queries read from PostgreSQL, real-time delivery reads from Redis, and the two paths never interfere with each other.

---

## Planned Features

Trident is being built to cover the full range of what developers need from an indexer — not just the easy parts.

Full historical event storage with no enforced retention limit, so a query against a contract's entire history works on day one and on day one thousand. Filtering by contract address, event topic, ledger range, and timestamp, with cursor-based pagination for large result sets. A REST API for straightforward queries and a GraphQL interface for composable ones. Real-time WebSocket subscriptions so a frontend can react to new contract events as they land on-chain. A TypeScript SDK that wraps all of this into a typed client developers can drop into an existing project in minutes. Self-hosted deployment via a single Docker Compose command, and a free hosted tier so teams that don't want to run infrastructure don't have to.

---

## Roadmap

| Phase | Focus | Status |
|-------|-------|--------|
| **1 — MVP** | Rust indexer, PostgreSQL, REST API, Testnet | 🔄 In progress |
| **2 — Developer Ready** | GraphQL, TypeScript SDK, Mainnet, hosted free tier | 📋 Planned |
| **3 — Scale** | WebSocket subscriptions, analytics, developer dashboard | 📋 Planned |
| **4 — Ecosystem** | Public event explorer, Rust SDK, multi-RPC redundancy | 📋 Planned |

---

## Project Status

- [x] Architecture defined
- [x] Full specification written — [`docs/SPECIFICATION.md`](./docs/SPECIFICATION.md)
- [x] Soroban event model documented — [`docs/soroban-event-model.md`](./docs/soroban-event-model.md)
- [x] Repository scaffolded
- [x] CI pipeline active
- [ ] Phase 1 development begins

---

## Reference Contracts

`contracts/` holds a minimal SEP-41 token contract plus build/deploy/invoke
scripts (local quickstart and testnet) used for local demos and E2E testing.
See [`contracts/README.md`](./contracts/README.md).

---

## Production Deployment

Trident ships a docker-compose overlay for production that terminates TLS at nginx and hides the API port from the host.

### Prerequisites

TLS certificates must be placed in the `nginx_certs` Docker volume before starting the stack. The nginx service expects:

- `/etc/nginx/certs/fullchain.pem`
- `/etc/nginx/certs/privkey.pem`

Populate the volume once (e.g. via Certbot or your own certificate pipeline) before the first `up`.

### Start the stack

```bash
docker compose -f docker/docker-compose.yml -f docker/docker-compose.prod.yml up -d
```

This command merges the base compose (postgres, redis, indexer, api) with the production overlay (nginx, port hiding). The API is reachable only through nginx — direct access on port 3000 is disabled in production.

### What the overlay changes

| Service | Base | Prod overlay |
|---------|------|--------------|
| `nginx` | — | Added: ports 80 + 443, TLS termination |
| `api` | `${PORT}:${PORT}` exposed | `ports: []` — no direct host access |

nginx proxies all traffic to `api:3000` on the internal Docker network, upgrades WebSocket connections at `/ws`, and disables response buffering for the SSE stream at `/v1/events/stream`.

---

## Contributing

All branches come off `dev`. Before opening a pull request, make sure all three CI checks pass locally — the pipeline will block merge if any of them fail.

**Rust**
```bash
cargo fmt --all             # format — CI runs --check, so this must be clean
cargo clippy --all-targets --all-features -- -D warnings
cargo test --all
```

**Go** (`services/api`)
```bash
go vet ./...
golangci-lint run           # install: https://golangci-lint.run/usage/install/
```

**TypeScript** (`sdk/typescript`)
```bash
npm ci
npm run lint                # runs tsc --noEmit in strict mode
```

Running these before pushing means CI passes on the first try. See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for branching conventions, commit message format, and code standards.

---

<div align="center">

*Build on Stellar. Query everything.*

🔱

[Discussions](https://github.com/trident-build/trident/discussions) · [Specification](./docs/SPECIFICATION.md) · [API Stability Policy](./docs/API_STABILITY.md) · [SDK Versioning Policy](./docs/SDK_VERSIONING_POLICY.md) · [10-Min Quickstart](./docs/QUICKSTART.md) · [Cutover Runbook](./docs/runbooks/testnet-cutover.md) · [Example App](./examples/testnet-monitor)

</div>
