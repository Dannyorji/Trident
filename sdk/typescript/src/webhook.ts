/**
 * Webhook signature verification helpers for Trident (issue #452).
 *
 * Receivers call {@link verifySignature} on every incoming webhook request to
 * confirm the delivery is authentic and within the replay-protection window.
 *
 * Signing scheme (for offline validation):
 * ```
 * const message  = `${timestamp}.${rawBody}`;
 * const mac      = createHmac("sha256", secret).update(message).digest("hex");
 * const expected = `sha256=${mac}`;
 * ```
 *
 * The `X-Trident-Signature` header may contain two space-separated signatures
 * during a secret rotation overlap window.  Pass your current active secret —
 * the function checks all tokens and succeeds if any one matches.
 */

/** Recommended replay-protection window in seconds (5 minutes). */
export const DEFAULT_TOLERANCE_SECONDS = 300;

/** Thrown by {@link verifySignature} when verification fails. */
export class WebhookVerificationError extends Error {
  readonly reason: string;

  constructor(reason: string) {
    super(`webhook signature verification failed: ${reason}`);
    this.name = "WebhookVerificationError";
    this.reason = reason;
  }
}

/**
 * Compute the `sha256=<hex>` signature for the given body and timestamp.
 *
 * This is the reference implementation — use it to produce test vectors or to
 * sign outgoing requests in your own test server.
 *
 * Works in both Node.js (via `node:crypto`) and browsers / edge runtimes (via
 * the Web Crypto API).
 */
export async function computeSignature(
  timestamp: number,
  body: string | Uint8Array,
  secret: string,
): Promise<string> {
  const enc = new TextEncoder();
  const bodyBytes = typeof body === "string" ? enc.encode(body) : body;
  const message = enc.encode(`${timestamp}.`);
  const combined = new Uint8Array(message.length + bodyBytes.length);
  combined.set(message, 0);
  combined.set(bodyBytes, message.length);

  const key = await crypto.subtle.importKey(
    "raw",
    enc.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, combined);
  const hex = Array.from(new Uint8Array(sig))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
  return `sha256=${hex}`;
}

/** Constant-time string comparison using XOR reduction. */
function safeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

/**
 * Verify an incoming Trident webhook delivery.
 *
 * @param body            Raw request body — read it **before** parsing JSON.
 * @param signature       Value of the `X-Trident-Signature` header.
 * @param timestamp       Value of the `X-Trident-Timestamp` header (Unix seconds, string).
 * @param secret          Your webhook subscription secret (starts with `whsec_`).
 * @param toleranceSecs   Maximum delivery age in seconds.  Pass `0` to disable
 *                        the timestamp check (not recommended in production).
 *                        Defaults to {@link DEFAULT_TOLERANCE_SECONDS} (300 s).
 *
 * @throws {@link WebhookVerificationError} on any failure.
 */
export async function verifySignature(
  body: string | Uint8Array,
  signature: string,
  timestamp: string,
  secret: string,
  toleranceSecs: number = DEFAULT_TOLERANCE_SECONDS,
): Promise<void> {
  // 1. Parse and range-check the timestamp.
  const ts = parseInt(timestamp, 10);
  if (!Number.isFinite(ts) || ts <= 0) {
    throw new WebhookVerificationError(
      "X-Trident-Timestamp is missing or not a valid Unix second",
    );
  }
  if (toleranceSecs > 0) {
    const age = Math.abs(Math.floor(Date.now() / 1000) - ts);
    if (age > toleranceSecs) {
      throw new WebhookVerificationError(
        `timestamp is ${age}s old (tolerance ${toleranceSecs}s) — possible replay`,
      );
    }
  }

  // 2. Compute the expected signature.
  const expected = await computeSignature(ts, body, secret);

  // 3. Accept any token in a space-separated signature header.
  const tokens = signature.split(" ").filter(Boolean);
  if (tokens.some((token) => safeEqual(token, expected))) {
    return;
  }
  throw new WebhookVerificationError("signature does not match");
}
