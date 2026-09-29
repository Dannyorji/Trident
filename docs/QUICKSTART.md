# ⚡ Trident 10-Minute Developer Quickstart

Get from **zero** to your **first live Soroban indexed event query on Stellar Testnet in under 10 minutes**.

---

## ⏱️ The 10-Minute Timeline

```
[0:00] Clone & Setup  ──► [2:00] Configure .env  ──► [4:00] Start Stack  ──► [6:00] Health Check  ──► [8:00] Query Events!
```

---

## 📋 Prerequisites

You only need **Docker** with Compose v2 installed. No Rust, Go, or compiler toolchain is required to run the indexer stack.

| Requirement | Minimum Version | Installation Check |
|---|---|---|
| **Docker** | 24.0+ | `docker --version` |
| **Docker Compose** | v2.20+ (Compose Plugin) | `docker compose version` |
| **curl & jq** | Standard CLI | `curl --version && jq --version` |

> 💡 **Using Windows or macOS?** Docker Desktop provides both Docker and Docker Compose v2 out of the box.

---

## 🚀 Step 1: Clone the Repository (1 Minute)

Clone the Trident repository and navigate to the project root:

```bash
git clone https://github.com/Telocel-Labs/Trident.git
cd Trident
```

---

## ⚙️ Step 2: Configure Environment (2 Minutes)

Copy the environment template to create your `.env` file:

```bash
cp .env.example .env
```

For a local Testnet developer setup, the default values work immediately out of the box:

```ini
# Core Storage & Ingestion
DATABASE_URL=postgresql://trident:password@postgres:5432/trident
REDIS_URL=redis://redis:6379

# Stellar Soroban Testnet RPC
STELLAR_RPC_URL=https://soroban-testnet.stellar.org
NETWORK=testnet
NETWORK_PASSPHRASE="Test SDF Network ; September 2015"

# Security & Ports
POSTGRES_USER=trident
POSTGRES_PASSWORD=password
POSTGRES_DB=trident
API_KEY_SALT=trident-dev-salt-12345
```

---

## 🐳 Step 3: Start the Full Stack (2 Minutes)

Launch all four components (**PostgreSQL**, **Redis**, **Rust Indexer Core**, and **Go API**) in the background:

```bash
docker compose -f docker/docker-compose.yml up -d
```

### What is running?
1. 🐘 **PostgreSQL** (`port 5432`): Persistent storage for all historical contract events, topics, and metrics.
2. ⚡ **Redis Streams** (`port 6379`): Real-time pub/sub bus for sub-second event streaming.
3. 🦀 **Trident Indexer** (Rust Core): Polls Stellar Testnet RPC, decodes XDR events natively, and persists ledger batches.
4. 🐹 **Trident API** (Go REST/WebSocket, `port 3000`): Serves high-speed filtered queries and WebSocket subscriptions.

Check container status:
```bash
docker compose -f docker/docker-compose.yml ps
```

---

## 🩺 Step 4: Verify System Health (2 Minutes)

Verify that the Go API is online and the Rust Indexer is streaming Stellar Testnet ledgers:

```bash
curl -s http://localhost:3000/v1/health | jq
```

### Expected Response:
```json
{
  "status": "ok",
  "indexer": {
    "status": "healthy",
    "network": "testnet",
    "latestLedger": 1458290,
    "lagLedgers": 0
  },
  "database": "connected",
  "redis": "connected"
}
```

View live ingestion logs:
```bash
docker compose -f docker/docker-compose.yml logs -f indexer
```
*You will see: `[INFO] RPC page received, latest_ledger=..., committing events`.*

---

## 🔍 Step 5: Query Your First Events! (3 Minutes)

Now that Trident is actively indexing Stellar Testnet, query events using REST or WebSocket.

### 5.1 Query Latest Contract Events

Retrieve the latest 5 contract events across the entire testnet:

```bash
curl -s "http://localhost:3000/v1/events?network=testnet&limit=5" | jq
```

**Response:**
```json
{
  "events": [
    {
      "id": "00001458-0000-0000-0000-000000000001",
      "contractId": "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
      "ledgerSequence": 1458290,
      "ledgerTimestamp": "2026-08-29T03:30:15Z",
      "transactionHash": "4a7b1c...",
      "eventIndex": 0,
      "eventType": "contract",
      "topics": ["transfer", "GA2C..."],
      "value": { "amount": "10000000" }
    }
  ],
  "nextCursor": "eyJsZWRnZXIiOjE0NTgyOTAsImlkIjoxfQ==",
  "hasMore": true
}
```

### 5.2 Filter Events by Contract Address

Filter events emitted by a specific Soroban token or contract:

```bash
curl -s "http://localhost:3000/v1/events?network=testnet&contract_id=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC&limit=10" | jq
```

### 5.3 Filter by Event Topic & Ledger Range

```bash
curl -s "http://localhost:3000/v1/events?network=testnet&topic=transfer&start_ledger=1450000&end_ledger=1458290" | jq
```

### 5.4 Live WebSocket Subscription

Stream newly landed events in real time:

```bash
# Using wscat (npm install -g wscat) or any WebSocket client:
wscat -c "ws://localhost:3000/v1/events/stream?network=testnet"
```

---

## 🛠️ Inline Troubleshooting & Common Stalls

If you run into an issue during setup, check these common scenarios:

### 1. Port Conflict (`Bind for 0.0.0.0:5432 failed: port is already allocated`)
* **Cause**: A local PostgreSQL or Redis server is already running on your host machine.
* **Fix**: Either stop your local postgres/redis service:
  ```bash
  sudo systemctl stop postgresql redis
  ```
  Or change the external port mapping in `docker/docker-compose.yml` (e.g. `"5433:5432"`).

### 2. `docker compose` command not found
* **Cause**: Older Docker installations used `docker-compose` (with a hyphen).
* **Fix**: Update Docker to include the Compose V2 plugin, or run:
  ```bash
  docker-compose -f docker/docker-compose.yml up -d
  ```

### 3. Public RPC Rate Limiting (`HTTP 429 Too Many Requests`)
* **Cause**: Public Stellar testnet RPC endpoint is under high global load.
* **Fix**: Add backup RPC endpoints in `.env`:
  ```ini
  STELLAR_RPC_URLS="https://soroban-testnet.stellar.org,https://testnet.sorobanrpc.com"
  ```

### 4. Database Connection Refused
* **Cause**: The indexer container booted before PostgreSQL finished initializing database schemas.
* **Fix**: Docker Compose healthchecks will automatically restart the indexer until PostgreSQL is healthy. You can manually restart with:
  ```bash
  docker compose -f docker/docker-compose.yml restart indexer api
  ```

---

## 🧹 Stopping & Teardown

To stop the services:
```bash
docker compose -f docker/docker-compose.yml down
```

To stop and completely delete all stored indexer state (clean slate):
```bash
docker compose -f docker/docker-compose.yml down -v
```

---

## 📚 Next Steps

- 📖 [REST API Reference](docs/site/api-reference/events-list.mdx) — Detailed query parameters and filters.
- 🔌 [TypeScript / React SDK](docs/site/sdk/client.mdx) — Integrate Trident into your Web3 frontend.
- 🛡️ [API Key Lifecycle & Auth](docs/runbooks/api-key-lifecycle.md) — Production authentication and rate limiting.
- 📊 [Metrics & Monitoring](docs/metrics-catalog.md) — Prometheus and Grafana dashboards.
