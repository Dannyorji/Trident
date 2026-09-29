import { describe, expect, it } from "vitest";
import {
  computeSignature,
  DEFAULT_TOLERANCE_SECONDS,
  verifySignature,
  WebhookVerificationError,
} from "../src/webhook.js";

const BODY = `{"id":"evt1","event":{"contractId":"CABC"}}`;
const SECRET = "whsec_testsecret";

async function freshSig(
  body: string = BODY,
  secret: string = SECRET,
): Promise<{ body: string; sig: string; ts: string }> {
  const ts = Math.floor(Date.now() / 1000);
  const sig = await computeSignature(ts, body, secret);
  return { body, sig, ts: String(ts) };
}

describe("computeSignature", () => {
  it("is deterministic", async () => {
    const a = await computeSignature(1_700_000_000, "body", "secret");
    const b = await computeSignature(1_700_000_000, "body", "secret");
    expect(a).toBe(b);
  });

  it("starts with sha256=", async () => {
    const sig = await computeSignature(1_700_000_000, "body", "secret");
    expect(sig).toMatch(/^sha256=[0-9a-f]{64}$/);
  });

  it("differs on body", async () => {
    const a = await computeSignature(1_700_000_000, "aaa", "secret");
    const b = await computeSignature(1_700_000_000, "bbb", "secret");
    expect(a).not.toBe(b);
  });

  it("differs on secret", async () => {
    const a = await computeSignature(1_700_000_000, "body", "secret-a");
    const b = await computeSignature(1_700_000_000, "body", "secret-b");
    expect(a).not.toBe(b);
  });

  it("differs on timestamp", async () => {
    const a = await computeSignature(1_700_000_000, "body", "secret");
    const b = await computeSignature(1_700_000_001, "body", "secret");
    expect(a).not.toBe(b);
  });
});

describe("verifySignature", () => {
  it("accepts a valid signature", async () => {
    const { body, sig, ts } = await freshSig();
    await expect(verifySignature(body, sig, ts, SECRET)).resolves.toBeUndefined();
  });

  it("rejects the wrong secret", async () => {
    const { body, sig, ts } = await freshSig();
    await expect(verifySignature(body, sig, ts, "whsec_wrong")).rejects.toBeInstanceOf(
      WebhookVerificationError,
    );
  });

  it("rejects an altered body", async () => {
    const ts = Math.floor(Date.now() / 1000);
    const sig = await computeSignature(ts, "original", SECRET);
    await expect(
      verifySignature("altered", sig, String(ts), SECRET),
    ).rejects.toBeInstanceOf(WebhookVerificationError);
  });

  it("rejects a stale timestamp (replay protection)", async () => {
    const staleTs = Math.floor(Date.now() / 1000) - 600; // 10 min ago
    const sig = await computeSignature(staleTs, BODY, SECRET);
    await expect(
      verifySignature(BODY, sig, String(staleTs), SECRET, DEFAULT_TOLERANCE_SECONDS),
    ).rejects.toThrow(/replay/);
  });

  it("accepts a stale timestamp when tolerance is disabled (0)", async () => {
    const staleTs = Math.floor(Date.now() / 1000) - 9999;
    const sig = await computeSignature(staleTs, BODY, SECRET);
    await expect(verifySignature(BODY, sig, String(staleTs), SECRET, 0)).resolves.toBeUndefined();
  });

  it("rejects a non-numeric timestamp", async () => {
    await expect(
      verifySignature(BODY, "sha256=abc", "not-a-number", SECRET),
    ).rejects.toBeInstanceOf(WebhookVerificationError);
  });

  it("accepts the old secret during a rotation overlap", async () => {
    const old = "whsec_old";
    const newS = "whsec_new";
    const ts = Math.floor(Date.now() / 1000);
    const body = `{"id":"evt1"}`;
    const sigNew = await computeSignature(ts, body, newS);
    const sigOld = await computeSignature(ts, body, old);
    const combined = `${sigNew} ${sigOld}`;
    await expect(verifySignature(body, combined, String(ts), old)).resolves.toBeUndefined();
  });

  it("accepts the new secret during a rotation overlap", async () => {
    const old = "whsec_old";
    const newS = "whsec_new";
    const ts = Math.floor(Date.now() / 1000);
    const body = `{"id":"evt1"}`;
    const sigNew = await computeSignature(ts, body, newS);
    const sigOld = await computeSignature(ts, body, old);
    const combined = `${sigNew} ${sigOld}`;
    await expect(verifySignature(body, combined, String(ts), newS)).resolves.toBeUndefined();
  });
});

describe("test vectors", () => {
  // These vectors match docs/webhook-signature-test-vectors.md.
  const vectors = [
    {
      name: "vector-1-minimal",
      secret: "whsec_0000000000000000000000000000000000000000000000000000000000000001",
      timestamp: 1_700_000_000,
      body: `{"id":"evt_test_001"}`,
      want: "sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a",
    },
    {
      name: "vector-2-empty-body",
      secret: "whsec_0000000000000000000000000000000000000000000000000000000000000001",
      timestamp: 1_700_000_000,
      body: "",
      want: "sha256=e7f81d9f74fad194220a4403cd3d75a2ee450ee8422648b4a0230a4ce77c2f5d",
    },
  ];

  for (const v of vectors) {
    it(v.name, async () => {
      const got = await computeSignature(v.timestamp, v.body, v.secret);
      expect(got).toBe(v.want);
    });
  }
});
