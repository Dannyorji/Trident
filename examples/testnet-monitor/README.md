# 🔱 Trident Testnet Event Monitor Example

This is a working, production-grade example application that connects to **Trident Indexer** on **Stellar Testnet** using the official `@trident/sdk` TypeScript client.

---

## Features Demonstrated

1. **Client Setup**: Authenticating and configuring the typed TypeScript client for Stellar Testnet.
2. **Historical Queries**: Querying indexed events by contract ID with limit and keyset pagination.
3. **Live Streaming**: Real-time event subscription via WebSocket (`client.subscribe`).

---

## Prerequisites

- **Node.js** (v20 LTS or later)
- **npm** (v9 or later)

---

## Quick Start

### 1. Install Dependencies

```bash
npm install
```

### 2. Configure Environment

Create a `.env` file (optional; sensible defaults point to public testnet):

```ini
TRIDENT_API_URL=https://api.testnet.trident.telocel.com
TRIDENT_API_KEY=your-api-key
CONTRACT_ID=CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC
```

### 3. Run the Monitor

```bash
npm start
```

### 4. CI Dry-Run Validation

```bash
npm test
```
