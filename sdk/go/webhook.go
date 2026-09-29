package trident

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultWebhookToleranceSeconds is the recommended replay-protection window.
// Reject any webhook whose X-Trident-Timestamp differs from the current wall
// clock by more than this value. Five minutes matches Stripe's convention and
// is wide enough to absorb normal network and clock-skew jitter while keeping
// the replay window short.
const DefaultWebhookToleranceSeconds int64 = 300

// WebhookVerificationError is returned by VerifyWebhookSignature when
// verification fails. The Reason field carries a human-readable explanation
// suitable for logging (never send it to the end user).
type WebhookVerificationError struct {
	Reason string
}

func (e *WebhookVerificationError) Error() string {
	return "webhook signature verification failed: " + e.Reason
}

// VerifyWebhookSignature verifies that an incoming webhook delivery is
// authentic and within the replay-protection window.
//
// Parameters:
//   - body:      the raw request body bytes (do not parse before verifying)
//   - signature: the value of the X-Trident-Signature header
//   - timestamp: the value of the X-Trident-Timestamp header (Unix seconds, as a string)
//   - secret:    your webhook subscription secret (starts with "whsec_")
//   - toleranceSecs: maximum age of the delivery in seconds; pass
//     DefaultWebhookToleranceSeconds (300) for the recommended window, or 0
//     to skip the timestamp check (not recommended in production)
//
// Returns nil on success, *WebhookVerificationError on any failure.
//
// Signing scheme (for offline validation):
//
//	message  = fmt.Sprintf("%d.%s", timestampUnix, rawBody)
//	mac      = HMAC-SHA256(key=secret, message=message)
//	expected = "sha256=" + hex.EncodeToString(mac)
//
// The X-Trident-Signature header may contain two space-separated signatures
// during a secret rotation overlap window; verification passes if either one
// matches. Always verify against your current active secret — the server sends
// both during rotation so you have time to swap your key.
func VerifyWebhookSignature(body []byte, signature, timestamp, secret string, toleranceSecs int64) error {
	// 1. Parse and range-check the timestamp.
	// strconv.ParseInt, not fmt.Sscanf: Sscanf stops at the first non-digit and
	// reports success, so "1700000000junk" would parse as 1700000000. The other
	// SDKs reject trailing garbage, and this one must agree with them.
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return &WebhookVerificationError{Reason: "X-Trident-Timestamp is missing or not a valid Unix second"}
	}
	if toleranceSecs > 0 {
		age := time.Now().Unix() - ts
		if age < 0 {
			age = -age // tolerate small clock skew in both directions
		}
		if age > toleranceSecs {
			return &WebhookVerificationError{
				Reason: fmt.Sprintf("timestamp is %d seconds old (tolerance %d s) — possible replay", age, toleranceSecs),
			}
		}
	}

	// 2. Compute the expected signature.
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", ts, body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// 3. Accept any token in a space-separated signature header.
	for _, token := range strings.Fields(signature) {
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			return nil
		}
	}
	return &WebhookVerificationError{Reason: "signature does not match"}
}
