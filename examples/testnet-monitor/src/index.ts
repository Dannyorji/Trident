/**
 * Trident Testnet Event Monitor Example Application
 *
 * Demonstrates:
 * 1. Initializing the official Trident TypeScript SDK for Stellar Testnet
 * 2. Querying historical paginated contract events via REST
 * 3. Subscribing to live real-time contract events via WebSocket
 */

import { TridentClient, iterEvents, SorobanEvent } from "@trident/sdk";
import WebSocket from "ws";
import * as dotenv from "dotenv";

dotenv.config();

// Configuration
const TRIDENT_API_URL = process.env.TRIDENT_API_URL || "https://api.testnet.trident.telocel.com";
const TRIDENT_API_KEY = process.env.TRIDENT_API_KEY || "trident_demo_key";
const CONTRACT_ID =
  process.env.CONTRACT_ID || "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC";

const isDryRun = process.argv.includes("--dry-run");

async function main() {
  console.log("=================================================");
  console.log("🔱 Trident Testnet Event Monitor");
  console.log("=================================================");
  console.log(`🌐 Network:     Testnet`);
  console.log(`🔗 Endpoint:    ${TRIDENT_API_URL}`);
  console.log(`📜 Contract:    ${CONTRACT_ID}`);
  console.log("-------------------------------------------------");

  // 1. Initialize Trident Client
  const client = new TridentClient({
    apiUrl: TRIDENT_API_URL,
    apiKey: TRIDENT_API_KEY,
    network: "testnet",
    webSocketImpl: WebSocket,
  });

  if (isDryRun) {
    console.log("✅ Dry-run validation mode: SDK initialized and configured successfully.");
    process.exit(0);
  }

  try {
    // 2. Query Recent Historical Events
    console.log("\n📦 Fetching latest indexed events...");
    const result = await client.queryEvents({
      contractId: CONTRACT_ID,
      limit: 5,
    });

    console.log(`Found ${result.events.length} recent events.\n`);
    for (const ev of result.events) {
      displayEvent(ev);
    }

    // 3. Live WebSocket Subscription
    console.log("\n⚡ Subscribing to real-time events over WebSocket...");
    const sub = client.subscribeToContract({
      contractId: CONTRACT_ID,
      onEvent: (event: SorobanEvent) => {
        console.log("\n🔔 [LIVE EVENT RECEIVED]");
        displayEvent(event);
      },
      onError: (err: Error) => {
        console.error("❌ Subscription error:", err.message);
      },
    });

    // Keep process alive for streaming
    process.on("SIGINT", () => {
      console.log("\nShutting down monitor...");
      sub.unsubscribe();
      process.exit(0);
    });
  } catch (err: any) {
    console.error("⚠️ Error querying Trident API:", err.message);
    // Don't crash dry-run or mock environments
    if (!isDryRun) {
      process.exit(1);
    }
  }
}

function displayEvent(ev: SorobanEvent) {
  console.log(`  [Ledger ${ev.ledgerSequence}] ${ev.eventType.toUpperCase()} | Tx: ${ev.transactionHash.slice(0, 10)}...`);
  console.log(`  Topics: [${ev.topics.join(", ")}]`);
  console.log(`  Data:   ${JSON.stringify(ev.data)}`);
  console.log("  ---------------------------------------------");
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
