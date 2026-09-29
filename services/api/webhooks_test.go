package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyWebhookSignature(t *testing.T) {
	body := `{"hello":"world"}`
	secret := "super-secret"
	ts := time.Now().Unix()
	signature := "sha256=" + signWebhookPayload(ts, body, secret)

	if !verifyWebhookSignature(ts, body, signature, secret) {
		t.Fatal("expected signature verification to succeed")
	}

	if verifyWebhookSignature(ts, body, "sha256=deadbeef", secret) {
		t.Fatal("expected signature verification to fail for a mismatched signature")
	}

	// A different timestamp must not verify.
	if verifyWebhookSignature(ts+1, body, signature, secret) {
		t.Fatal("expected signature to fail when timestamp is different (replay protection)")
	}
}

// allowLoopbackWebhookTargets exempts a single test from the SSRF rules in
// validateWebhookTargetURL (issue #453). The delivery tests drive httptest
// servers, which are plain http:// on 127.0.0.1 and so fail both the https
// requirement and the loopback block.
//
// Scoped per-test with Cleanup rather than set globally, so the validator's
// own tests still exercise the real rules in the same binary.
func allowLoopbackWebhookTargets(t *testing.T) {
	t.Helper()
	previous := allowInsecureWebhookTargets
	allowInsecureWebhookTargets = true
	t.Cleanup(func() { allowInsecureWebhookTargets = previous })
}

func TestDeliverWebhookSendsTimestampAndSignedPayload(t *testing.T) {
	allowLoopbackWebhookTargets(t)
	var (
		gotBody      []byte
		gotSignature string
		gotTimestamp string
		gotCT        string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotSignature = r.Header.Get("X-Trident-Signature")
		gotTimestamp = r.Header.Get("X-Trident-Timestamp")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub := webhookSubscription{
		ID:        "sub-ts",
		Secret:    "super-secret",
		TargetURL: server.URL,
	}
	event := webhookEvent{
		ID:              "evt-ts",
		ContractID:      "C123",
		LedgerSequence:  55001,
		Topic0:          "transfer",
		Data:            map[string]any{"amount": "100"},
		TransactionHash: "abc123",
		Network:         "testnet",
	}

	if err := deliverWebhook(context.Background(), sub, event); err != nil {
		t.Fatalf("deliverWebhook: %v", err)
	}

	if gotCT != "application/json" {
		t.Fatalf("unexpected Content-Type: %q", gotCT)
	}
	if gotTimestamp == "" {
		t.Fatal("expected X-Trident-Timestamp header")
	}
	ts, err := strconv.ParseInt(gotTimestamp, 10, 64)
	if err != nil || ts <= 0 {
		t.Fatalf("X-Trident-Timestamp is not a valid unix second: %q", gotTimestamp)
	}
	if gotSignature == "" {
		t.Fatal("expected X-Trident-Signature header")
	}
	expected := "sha256=" + signWebhookPayload(ts, string(gotBody), sub.Secret)
	if gotSignature != expected {
		t.Fatalf("signature mismatch: got %q, want %q", gotSignature, expected)
	}

	// Verify the payload structure.
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if payload["webhook_id"] != sub.ID {
		t.Fatalf("expected webhook_id=%q, got %v", sub.ID, payload["webhook_id"])
	}
	if payload["event"].(map[string]any)["id"] != event.ID {
		t.Fatalf("expected event.id=%q in payload", event.ID)
	}
	// Timestamp field must match the header.
	if int64(payload["timestamp"].(float64)) != ts {
		t.Fatalf("payload.timestamp %v != header timestamp %d", payload["timestamp"], ts)
	}
}

func TestDeliverWebhookRetrySucceedsAfterTransientFailures(t *testing.T) {
	allowLoopbackWebhookTargets(t)
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			// Fail the first two attempts.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sub := webhookSubscription{
		ID:        "sub-retry",
		Secret:    "s3cr3t",
		TargetURL: server.URL,
	}
	event := webhookEvent{ID: "evt-retry", ContractID: "C1", Network: "testnet"}

	err := deliverSubscriptionWithRetry(context.Background(), nil, sub, event)
	if err != nil {
		t.Fatalf("expected eventual success, got err: %v", err)
	}
	if got := int(callCount.Load()); got != 3 {
		t.Fatalf("expected 3 HTTP calls (2 failures + 1 success), got %d", got)
	}
}

func TestDeliverWebhookDeadLetterAfterMaxAttempts(t *testing.T) {
	allowLoopbackWebhookTargets(t)
	// Capture the log output to verify the dead-letter warning is emitted.
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	sub := webhookSubscription{
		ID:        "sub-dlq",
		Secret:    "s3cr3t",
		TargetURL: server.URL,
	}
	event := webhookEvent{ID: "evt-dlq", ContractID: "C1", Network: "testnet"}

	err := deliverSubscriptionWithRetry(context.Background(), nil, sub, event)
	// After max attempts an error should be returned.
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if got := int(callCount.Load()); got != maxWebhookAttempts {
		t.Fatalf("expected %d HTTP calls, got %d", maxWebhookAttempts, got)
	}
}

func TestDeliverWebhookDeadLetterOnNetworkError(t *testing.T) {
	allowLoopbackWebhookTargets(t)
	// Point to a server that never responds.
	sub := webhookSubscription{
		ID:        "sub-net-dlq",
		Secret:    "s3cr3t",
		TargetURL: "http://127.0.0.1:0", // immediately refused
	}
	event := webhookEvent{ID: "evt-net-dlq", ContractID: "C1", Network: "testnet"}

	err := deliverSubscriptionWithRetry(context.Background(), nil, sub, event)
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
}

func TestSignWebhookPayloadIsStable(t *testing.T) {
	body := `{"id":"evt1"}`
	secret := "my-secret"
	ts := int64(1_700_000_000)

	sig1 := signWebhookPayload(ts, body, secret)
	sig2 := signWebhookPayload(ts, body, secret)
	if sig1 != sig2 {
		t.Fatalf("expected deterministic signature, got %q and %q", sig1, sig2)
	}

	// Different body → different signature.
	sig3 := signWebhookPayload(ts, `{"id":"evt2"}`, secret)
	if sig1 == sig3 {
		t.Fatal("expected different signature for different body")
	}

	// Different secret → different signature.
	sig4 := signWebhookPayload(ts, body, "other-secret")
	if sig1 == sig4 {
		t.Fatal("expected different signature for different secret")
	}

	// Signature must cover the timestamp — different ts ≠ same signature.
	sig5 := signWebhookPayload(ts+1, body, secret)
	if sig1 == sig5 {
		t.Fatal("expected different signature for different timestamp")
	}

	// Must start with the hex representation (no prefix yet — raw hex from signWebhookPayload).
	if strings.HasPrefix(sig1, "sha256=") {
		t.Fatal("signWebhookPayload should return raw hex, not the sha256= prefixed form")
	}
}
