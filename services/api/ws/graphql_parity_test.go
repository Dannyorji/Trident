package ws

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// End-to-end transport tests for GraphQL parity (issue #223), driving the
// real serveGQL loop over a net.Pipe rather than calling resolvers directly.
// These are what prove the query path, the subscription path, auth and rate
// limiting actually compose on one connection.

// gqlConnectAndAck runs connection_init and consumes the ack.
func gqlConnectAndAck(t *testing.T, conn net.Conn, r interface {
	Read([]byte) (int, error)
}, key string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":"` + key + `"}`),
	}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Query over the live transport
// ---------------------------------------------------------------------------

// TestGQLTransport_QueryReturnsNextThenComplete is the query half of parity:
// a query resolves once and the operation completes, rather than streaming.
func TestGQLTransport_QueryReturnsNextThenComplete(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{page: EventsPage{
		Events:  []map[string]any{{"id": "e1", "contract_id": "C1"}},
		HasMore: false,
	}}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:    authFromValidator(func(string) bool { return true }),
		Backend: backend,
	})

	gqlConnectAndAck(t, conn, r, "key")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if ack := readGQLFrame(t, r); ack.Type != "connection_ack" {
		t.Fatalf("expected connection_ack, got %q", ack.Type)
	}

	p, _ := json.Marshal(map[string]any{
		"query": `query { events(contractId: "CABC") { events { id } hasMore nextCursor } }`,
	})
	if err := writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "q1", Payload: p}); err != nil {
		t.Fatalf("write query: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	next := readGQLFrame(t, r)
	if next.Type != "next" || next.ID != "q1" {
		t.Fatalf("expected next for q1, got type=%q id=%q", next.Type, next.ID)
	}

	var body struct {
		Data struct {
			Events struct {
				Events     []map[string]any `json:"events"`
				HasMore    bool             `json:"hasMore"`
				NextCursor *string          `json:"nextCursor"`
			} `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(next.Payload, &body); err != nil {
		t.Fatalf("decode next payload: %v (%s)", err, next.Payload)
	}
	if len(body.Data.Events.Events) != 1 || body.Data.Events.Events[0]["id"] != "e1" {
		t.Errorf("payload = %s", next.Payload)
	}
	// nextCursor must be present-and-null on the last page, matching the REST
	// envelope (issue #575) so one auto-pager rule works on both transports.
	if body.Data.Events.NextCursor != nil {
		t.Errorf("nextCursor = %v, want null on the last page", *body.Data.Events.NextCursor)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	done := readGQLFrame(t, r)
	if done.Type != "complete" || done.ID != "q1" {
		t.Fatalf("expected complete for q1, got type=%q id=%q", done.Type, done.ID)
	}

	// A query must not register with the hub — only subscriptions do.
	hub.mu.RLock()
	n := len(hub.clients)
	hub.mu.RUnlock()
	if n != 0 {
		t.Errorf("a query registered %d hub subscribers, want 0", n)
	}
}

// TestGQLTransport_QueryAndSubscriptionOnOneConnection is the parity claim in
// full: the same socket serves a one-shot query and a live subscription,
// multiplexed by operation id.
func TestGQLTransport_QueryAndSubscriptionOnOneConnection(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{page: EventsPage{Events: []map[string]any{{"id": "from-query"}}}}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:    authFromValidator(func(string) bool { return true }),
		Backend: backend,
	})

	gqlConnectAndAck(t, conn, r, "key")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readGQLFrame(t, r) // ack

	// 1. A query resolves and completes.
	qp, _ := json.Marshal(map[string]any{"query": `query { events { events { id } } }`})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "q1", Payload: qp})
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if m := readGQLFrame(t, r); m.Type != "next" || m.ID != "q1" {
		t.Fatalf("query: got type=%q id=%q", m.Type, m.ID)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if m := readGQLFrame(t, r); m.Type != "complete" || m.ID != "q1" {
		t.Fatalf("query complete: got type=%q id=%q", m.Type, m.ID)
	}

	// 2. A subscription on the same connection streams live events.
	sp, _ := json.Marshal(map[string]any{
		"query": `subscription { contractEvents(contractId: "CLIVE") { id } }`,
	})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "s1", Payload: sp})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	hub.Broadcast("CLIVE", []byte(`{"id":"live-1","contract_id":"CLIVE","topics":[]}`))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "next" || msg.ID != "s1" {
		t.Fatalf("subscription delivery: got type=%q id=%q", msg.Type, msg.ID)
	}
	if !containsJSON(msg.Payload, "live-1") {
		t.Errorf("subscription payload = %s", msg.Payload)
	}
}

func containsJSON(raw json.RawMessage, want string) bool {
	return json.Valid(raw) && strings.Contains(string(raw), want)
}

// ---------------------------------------------------------------------------
// Auth is required for queries, not only subscriptions
// ---------------------------------------------------------------------------

// TestGQLTransport_QueryRequiresAuth closes the obvious hole: the query
// transport must not be reachable without completing connection_init.
func TestGQLTransport_QueryRequiresAuth(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:    authFromValidator(func(k string) bool { return k == "good" }),
		Backend: backend,
	})

	// Send a query with no connection_init at all.
	p, _ := json.Marshal(map[string]any{"query": `query { events { events { id } } }`})
	if err := writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "q1", Payload: p}); err != nil {
		t.Fatalf("write query: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "connection_error" {
		t.Fatalf("expected connection_error for an unauthenticated query, got %q", msg.Type)
	}
	if backend.calls != 0 {
		t.Errorf("backend was called %d times for an unauthenticated query, want 0", backend.calls)
	}
}

// TestGQLTransport_BadKeyRejected covers the DB-backed rejection path.
func TestGQLTransport_BadKeyRejected(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:    authFromValidator(func(k string) bool { return k == "good" }),
		Backend: backend,
	})

	gqlConnectAndAck(t, conn, r, "revoked")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if msg := readGQLFrame(t, r); msg.Type != "connection_error" {
		t.Fatalf("expected connection_error for an unknown key, got %q", msg.Type)
	}
	if backend.calls != 0 {
		t.Errorf("backend was called for a rejected connection")
	}
}

// TestGQLTransport_NetworkFromKeyScopesQuery proves the connection's network
// — not anything the client sends — is what reaches the backend.
func TestGQLTransport_NetworkFromKeyScopesQuery(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth: func(_ context.Context, _ string) (string, string, bool) {
			return "key-1", "mainnet", true
		},
		Backend: backend,
	})

	gqlConnectAndAck(t, conn, r, "key")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readGQLFrame(t, r) // ack

	// The client asks for testnet; the key says mainnet. The key wins.
	p, _ := json.Marshal(map[string]any{
		"query":     `query { events { events { id } } }`,
		"variables": map[string]any{"network": "testnet"},
	})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "q1", Payload: p})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readGQLFrame(t, r) // next

	if backend.gotQuery.Network != "mainnet" {
		t.Fatalf("network = %q, want mainnet — the key's network must scope the read",
			backend.gotQuery.Network)
	}
}

// ---------------------------------------------------------------------------
// Rate limiting applies to GraphQL operations
// ---------------------------------------------------------------------------

// TestGQLTransport_RateLimitRejectsOperation is the acceptance criterion
// "auth + rate limiting enforced on GraphQL/WS". Before #223 the HTTP tiered
// limiter never saw these operations: it keys off the X-API-Key header, which
// a WebSocket client does not send.
func TestGQLTransport_RateLimitRejectsOperation(t *testing.T) {
	hub := NewHub()
	backend := &fakeBackend{}
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:        authFromValidator(func(string) bool { return true }),
		RateLimiter: func(context.Context, string) (bool, error) { return false, nil },
		Backend:     backend,
	})

	gqlConnectAndAck(t, conn, r, "key")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readGQLFrame(t, r) // ack

	p, _ := json.Marshal(map[string]any{"query": `query { events { events { id } } }`})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "q1", Payload: p})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "error" || msg.ID != "q1" {
		t.Fatalf("expected a rate-limit error for q1, got type=%q id=%q", msg.Type, msg.ID)
	}

	var errs []struct {
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(msg.Payload, &errs); err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if len(errs) != 1 || errs[0].Extensions.Code != "RATE_LIMITED" {
		t.Fatalf("payload = %s, want a RATE_LIMITED code", msg.Payload)
	}
	if backend.calls != 0 {
		t.Errorf("backend was called %d times despite the rate limit, want 0", backend.calls)
	}
}

// A subscription is an operation too: the limiter must gate it as well, or a
// client could open unlimited streams while its queries are throttled.
func TestGQLTransport_RateLimitAppliesToSubscriptions(t *testing.T) {
	hub := NewHub()
	conn, r := testGQLPipeDeps(t, hub, GraphQLDeps{
		Auth:        authFromValidator(func(string) bool { return true }),
		RateLimiter: func(context.Context, string) (bool, error) { return false, nil },
	})

	gqlConnectAndAck(t, conn, r, "key")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	readGQLFrame(t, r) // ack

	p, _ := json.Marshal(map[string]any{
		"query": `subscription { contractEvents(contractId: "C1") { id } }`,
	})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "s1", Payload: p})

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if msg := readGQLFrame(t, r); msg.Type != "error" {
		t.Fatalf("expected a rate-limit error for the subscription, got %q", msg.Type)
	}

	hub.mu.RLock()
	n := len(hub.clients)
	hub.mu.RUnlock()
	if n != 0 {
		t.Errorf("a rate-limited subscription registered %d hub subscribers, want 0", n)
	}
}
