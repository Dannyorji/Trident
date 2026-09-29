package trident

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// sign is the reference implementation: HMAC-SHA256 over "<ts>.<body>".
func sign(ts int64, body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", ts, body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature_ValidSignature(t *testing.T) {
	body := []byte(`{"id":"evt1","event":{"contractId":"CABC"}}`)
	secret := "whsec_testsecret"
	ts := time.Now().Unix()
	sig := sign(ts, string(body), secret)

	if err := VerifyWebhookSignature(body, sig, fmt.Sprint(ts), secret, DefaultWebhookToleranceSeconds); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

func TestVerifyWebhookSignature_WrongSecret(t *testing.T) {
	body := []byte(`{"id":"evt1"}`)
	ts := time.Now().Unix()
	sig := sign(ts, string(body), "whsec_correct")

	err := VerifyWebhookSignature(body, sig, fmt.Sprint(ts), "whsec_wrong", DefaultWebhookToleranceSeconds)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

func TestVerifyWebhookSignature_AlteredBody(t *testing.T) {
	secret := "whsec_testsecret"
	ts := time.Now().Unix()
	sig := sign(ts, `{"id":"evt1"}`, secret)

	// Verify against a different body — must fail.
	err := VerifyWebhookSignature([]byte(`{"id":"evt2"}`), sig, fmt.Sprint(ts), secret, DefaultWebhookToleranceSeconds)
	if err == nil {
		t.Fatal("expected error for altered body, got nil")
	}
}

func TestVerifyWebhookSignature_ReplayRejected(t *testing.T) {
	body := []byte(`{"id":"evt1"}`)
	secret := "whsec_testsecret"
	// Timestamp 10 minutes in the past.
	staleTS := time.Now().Unix() - 600
	sig := sign(staleTS, string(body), secret)

	err := VerifyWebhookSignature(body, sig, fmt.Sprint(staleTS), secret, DefaultWebhookToleranceSeconds)
	if err == nil {
		t.Fatal("expected replay rejection, got nil")
	}
}

func TestVerifyWebhookSignature_ToleranceDisabled(t *testing.T) {
	body := []byte(`{"id":"evt1"}`)
	secret := "whsec_testsecret"
	staleTS := time.Now().Unix() - 9999
	sig := sign(staleTS, string(body), secret)

	// toleranceSecs=0 disables the timestamp check.
	if err := VerifyWebhookSignature(body, sig, fmt.Sprint(staleTS), secret, 0); err != nil {
		t.Fatalf("expected nil with tolerance disabled, got: %v", err)
	}
}

func TestVerifyWebhookSignature_RotationOverlap(t *testing.T) {
	body := []byte(`{"id":"evt1"}`)
	oldSecret := "whsec_old"
	newSecret := "whsec_new"
	ts := time.Now().Unix()

	// During rotation the header contains both signatures space-separated.
	sigNew := sign(ts, string(body), newSecret)
	sigOld := sign(ts, string(body), oldSecret)
	combined := sigNew + " " + sigOld

	// Receiver still verifying with old secret must accept the combined header.
	if err := VerifyWebhookSignature(body, combined, fmt.Sprint(ts), oldSecret, DefaultWebhookToleranceSeconds); err != nil {
		t.Fatalf("old secret should verify against combined header: %v", err)
	}
	// Receiver already on new secret must also accept.
	if err := VerifyWebhookSignature(body, combined, fmt.Sprint(ts), newSecret, DefaultWebhookToleranceSeconds); err != nil {
		t.Fatalf("new secret should verify against combined header: %v", err)
	}
}

func TestVerifyWebhookSignature_InvalidTimestamp(t *testing.T) {
	body := []byte(`{"id":"evt1"}`)
	err := VerifyWebhookSignature(body, "sha256=abc", "not-a-number", "whsec_s", DefaultWebhookToleranceSeconds)
	if err == nil {
		t.Fatal("expected error for non-numeric timestamp")
	}
}

// TestVerifyWebhookSignature_TestVectors validates the published test vectors
// from docs/webhook-signature-test-vectors.md so the SDK stays in sync with
// the server-side implementation.
func TestVerifyWebhookSignature_TestVectors(t *testing.T) {
	vectors := []struct {
		name      string
		secret    string
		timestamp int64
		body      string
		wantSig   string
	}{
		{
			name:      "vector-1-minimal",
			secret:    "whsec_0000000000000000000000000000000000000000000000000000000000000001",
			timestamp: 1700000000,
			body:      `{"id":"evt_test_001"}`,
			wantSig:   "sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a",
		},
		{
			name:      "vector-2-empty-body",
			secret:    "whsec_0000000000000000000000000000000000000000000000000000000000000001",
			timestamp: 1700000000,
			body:      "",
			wantSig:   "sha256=e7f81d9f74fad194220a4403cd3d75a2ee450ee8422648b4a0230a4ce77c2f5d",
		},
		{
			name:      "vector-3-realistic-payload",
			secret:    "whsec_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			timestamp: 1700000000,
			body:      `{"id":"wh_1700000000000000000","webhook_id":"sub-abc123","event":{"id":"evt-xyz","contractId":"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM","ledgerSequence":55001,"topic0":"transfer","data":{"amount":"100"},"txHash":"abc123def456","network":"testnet"},"timestamp":1700000000,"delivered_at":"2023-11-14T22:13:20Z"}`,
			wantSig:   "sha256=db74732f4339f7362266716ac2613c575d4a6640e710a1505f8afa3d3db78276",
		},
	}

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			got := sign(v.timestamp, v.body, v.secret)
			if got != v.wantSig {
				t.Errorf("signature mismatch\n  got:  %s\n  want: %s", got, v.wantSig)
			}
		})
	}
}
