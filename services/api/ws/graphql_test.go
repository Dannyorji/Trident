package ws

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit tests — helper functions
// ---------------------------------------------------------------------------

func TestGQLParseSubscribe_Variables(t *testing.T) {
	payload := json.RawMessage(`{
		"query": "subscription($cid:String!){contractEvents(contractId:$cid){id}}",
		"variables": {"contractId": "CTEST", "topic0": "transfer"}
	}`)
	cid, topic0, err := gqlParseSubscribe(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "CTEST" {
		t.Errorf("contractId: got %q, want CTEST", cid)
	}
	if topic0 != "transfer" {
		t.Errorf("topic0: got %q, want transfer", topic0)
	}
}

func TestGQLParseSubscribe_QueryTooLong(t *testing.T) {
	huge := strings.Repeat("a", gqlMaxQueryLen+1)
	payload, _ := json.Marshal(map[string]interface{}{
		"query":     huge,
		"variables": map[string]string{"contractId": "CTEST"},
	})
	_, _, err := gqlParseSubscribe(payload)
	if err == nil {
		t.Fatal("expected an error for an oversized query string, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Fatalf("expected a max-length error, got: %v", err)
	}
}

func TestGQLParseSubscribe_QueryAtLimit_Allowed(t *testing.T) {
	// contractId comes from variables, so the query body itself can be
	// padding right up to (but not over) the limit.
	padding := strings.Repeat("a", gqlMaxQueryLen)
	payload, _ := json.Marshal(map[string]interface{}{
		"query":     padding,
		"variables": map[string]string{"contractId": "CTEST"},
	})
	cid, _, err := gqlParseSubscribe(payload)
	if err != nil {
		t.Fatalf("unexpected error at exactly the limit: %v", err)
	}
	if cid != "CTEST" {
		t.Fatalf("contractId: got %q, want CTEST", cid)
	}
}

func TestGQLParseSubscribe_InlineQuery(t *testing.T) {
	payload := json.RawMessage(`{
		"query": "subscription { contractEvents(contractId: \"CINLINE\", topic0: \"mint\") { id } }"
	}`)
	cid, topic0, err := gqlParseSubscribe(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "CINLINE" {
		t.Errorf("contractId: got %q, want CINLINE", cid)
	}
	if topic0 != "mint" {
		t.Errorf("topic0: got %q, want mint", topic0)
	}
}

func TestGQLParseSubscribe_VariablesTakePrecedence(t *testing.T) {
	payload := json.RawMessage(`{
		"query": "subscription { contractEvents(contractId: \"INLINE\") { id } }",
		"variables": {"contractId": "VAR"}
	}`)
	cid, _, err := gqlParseSubscribe(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cid != "VAR" {
		t.Errorf("variables should win: got %q, want VAR", cid)
	}
}

func TestGQLParseSubscribe_MissingContractID(t *testing.T) {
	payload := json.RawMessage(`{"query": "subscription { contractEvents { id } }"}`)
	_, _, err := gqlParseSubscribe(payload)
	if err == nil {
		t.Fatal("expected error for missing contractId")
	}
	if !strings.Contains(err.Error(), "contractId") {
		t.Errorf("error should mention contractId: %v", err)
	}
}

func TestGQLParseSubscribe_NilPayload(t *testing.T) {
	_, _, err := gqlParseSubscribe(nil)
	if err == nil {
		t.Fatal("expected error for nil payload")
	}
}

func TestGQLExtractKey_Direct(t *testing.T) {
	payload := json.RawMessage(`{"Authorization":"tk_direct"}`)
	if got := gqlExtractKey(payload); got != "tk_direct" {
		t.Errorf("got %q, want tk_direct", got)
	}
}

func TestGQLExtractKey_Bearer(t *testing.T) {
	payload := json.RawMessage(`{"Authorization":"Bearer tk_bearer"}`)
	if got := gqlExtractKey(payload); got != "tk_bearer" {
		t.Errorf("got %q, want tk_bearer", got)
	}
}

func TestGQLExtractKey_NilPayload(t *testing.T) {
	if got := gqlExtractKey(nil); got != "" {
		t.Errorf("nil payload: got %q, want empty", got)
	}
}

func TestGQLMatchesTopic0_ArrayTopics(t *testing.T) {
	raw := []byte(`{"id":"e1","topics":["transfer","token"]}`)
	if !gqlMatchesTopic0(raw, "transfer") {
		t.Error("expected match for first topic")
	}
	if gqlMatchesTopic0(raw, "token") {
		t.Error("second topic must not match topic0 filter")
	}
	if gqlMatchesTopic0(raw, "other") {
		t.Error("unrelated topic must not match")
	}
}

func TestGQLMatchesTopic0_StringEncodedTopics(t *testing.T) {
	// Some Redis consumers serialise topics as a JSON string of a JSON array.
	raw := []byte(`{"topics":"[\"mint\",\"addr\"]"}`)
	if !gqlMatchesTopic0(raw, "mint") {
		t.Error("expected match for string-encoded topics")
	}
	if gqlMatchesTopic0(raw, "addr") {
		t.Error("second topic must not match")
	}
}

func TestGQLMatchesTopic0_EmptyTopics(t *testing.T) {
	if gqlMatchesTopic0([]byte(`{"topics":[]}`), "transfer") {
		t.Error("empty topics must not match")
	}
}

func TestGQLSub_ImplementsSubscriber(t *testing.T) {
	var _ subscriber = (*gqlSub)(nil)
}

func TestGQLSub_ShutdownClosesChannel(t *testing.T) {
	s := &gqlSub{cid: "C1", send: make(chan []byte, 1)}
	s.shutdown()
	_, open := <-s.send
	if open {
		t.Error("shutdown must close send channel")
	}
}

func TestGQLNextMsg_FieldRemap(t *testing.T) {
	raw := []byte(`{
		"id": "evt1",
		"contract_id": "CABC",
		"transaction_hash": "txhash",
		"ledger_sequence": 42,
		"event_type": "contract",
		"topics": ["transfer"],
		"data": "{}"
	}`)
	msg := gqlNextMsg("sub1", raw)

	if msg.Type != "next" || msg.ID != "sub1" {
		t.Fatalf("type/id: got %q/%q", msg.Type, msg.ID)
	}

	var outer struct {
		Data struct {
			ContractEvents map[string]interface{} `json:"contractEvents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg.Payload, &outer); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	ev := outer.Data.ContractEvents
	if ev["contractId"] != "CABC" {
		t.Errorf("contractId remapping: got %v", ev["contractId"])
	}
	if ev["txHash"] != "txhash" {
		t.Errorf("txHash remapping: got %v", ev["txHash"])
	}
	if ev["id"] != "evt1" {
		t.Errorf("id: got %v", ev["id"])
	}
}

// ---------------------------------------------------------------------------
// Integration test — full serveGQL loop via net.Pipe
// ---------------------------------------------------------------------------

// testGQLPipe sets up a server-side serveGQL goroutine over a net.Pipe pair
// and returns the client-side connection and a buffered reader for it.
//
// It still takes a plain func(string) bool and adapts it to GraphQLDeps, so
// every subscription test written before the query transport landed (#223)
// exercises the same protocol loop unchanged. Tests that need a backend, a
// rate limiter, or a specific network use testGQLPipeDeps instead.
func testGQLPipe(t *testing.T, hub *Hub, validateKey func(string) bool) (net.Conn, *bufio.Reader) {
	t.Helper()
	return testGQLPipeDeps(t, hub, GraphQLDeps{Auth: authFromValidator(validateKey)})
}

// testGQLPipeDeps is testGQLPipe with the full dependency set.
func testGQLPipeDeps(t *testing.T, hub *Hub, deps GraphQLDeps) (net.Conn, *bufio.Reader) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	bufrw := bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn))
	go serveGQL(serverConn, bufrw, hub, deps)
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	return clientConn, bufio.NewReader(clientConn)
}

// authFromValidator adapts a boolean key check to a GraphQLAuthFunc, giving
// every accepted key a fixed identity on the default network.
func authFromValidator(validateKey func(string) bool) GraphQLAuthFunc {
	return func(_ context.Context, key string) (string, string, bool) {
		if !validateKey(key) {
			return "", "", false
		}
		return "test-key-id", "testnet", true
	}
}

// writeGQLFrame writes a masked text frame carrying a JSON-encoded gqlMessage.
func writeGQLFrame(w io.Writer, msg gqlMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return writeMaskedFrame(w, data)
}

// writeMaskedFrame writes an RFC 6455 masked text frame to w.
func writeMaskedFrame(w io.Writer, payload []byte) error {
	maskKey := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	var header []byte
	n := len(payload)
	switch {
	case n < 126:
		header = []byte{0x81, 0x80 | byte(n)}
	case n <= 0xFFFF:
		header = []byte{0x81, 0xFE, byte(n >> 8), byte(n)}
	default:
		header = []byte{0x81, 0xFF,
			0, 0, 0, 0,
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
		}
	}
	header = append(header, maskKey[:]...)
	_, err := w.Write(append(header, masked...))
	return err
}

// readGQLFrame reads one graphql-transport-ws message from a server connection.
// It skips non-text (ping/pong) frames automatically.
func readGQLFrame(t *testing.T, r *bufio.Reader) gqlMessage {
	t.Helper()
	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(r, header); err != nil {
			t.Fatalf("read frame header: %v", err)
		}
		opcode := header[0] & 0x0F
		payloadLen := int(header[1] & 0x7F)
		if payloadLen == 126 {
			b := make([]byte, 2)
			if _, err := io.ReadFull(r, b); err != nil {
				t.Fatalf("read extended length: %v", err)
			}
			payloadLen = int(binary.BigEndian.Uint16(b))
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			t.Fatalf("read frame payload: %v", err)
		}
		if opcode != 0x1 && opcode != 0x2 {
			continue // skip ping / pong frames
		}
		var msg gqlMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatalf("unmarshal GQL message: %v", err)
		}
		return msg
	}
}

func TestGQLHandler_AuthAndDeliver(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return k == "valid-key" }
	conn, r := testGQLPipe(t, hub, validateKey)

	// connection_init with valid key → connection_ack
	if err := writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":"valid-key"}`),
	}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}
	ack := readGQLFrame(t, r)
	if ack.Type != "connection_ack" {
		t.Fatalf("expected connection_ack, got %q", ack.Type)
	}

	// subscribe
	subPayload, _ := json.Marshal(map[string]interface{}{
		"query":     "subscription { contractEvents(contractId: \"CTEST\") { id } }",
		"variables": map[string]string{"contractId": "CTEST"},
	})
	if err := writeGQLFrame(conn, gqlMessage{
		Type:    "subscribe",
		ID:      "1",
		Payload: json.RawMessage(subPayload),
	}); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}

	// Wait for the subscription to register in the hub.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Broadcast an event from the hub.
	event := []byte(`{"id":"evt1","contract_id":"CTEST","transaction_hash":"tx1","topics":["transfer"]}`)
	hub.Broadcast("CTEST", event)

	// Expect a next message.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "next" {
		t.Fatalf("expected next, got %q", msg.Type)
	}
	if msg.ID != "1" {
		t.Fatalf("expected id=1, got %q", msg.ID)
	}

	var outer struct {
		Data struct {
			ContractEvents struct {
				ID         string `json:"id"`
				ContractID string `json:"contractId"`
			} `json:"contractEvents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg.Payload, &outer); err != nil {
		t.Fatalf("unmarshal next payload: %v", err)
	}
	if outer.Data.ContractEvents.ID != "evt1" {
		t.Errorf("event id: got %q, want evt1", outer.Data.ContractEvents.ID)
	}
	if outer.Data.ContractEvents.ContractID != "CTEST" {
		t.Errorf("contractId remap: got %q", outer.Data.ContractEvents.ContractID)
	}
}

func TestGQLHandler_SubscriptionCapPerConnection(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return k == "key" }
	conn, r := testGQLPipe(t, hub, validateKey)

	if err := writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":"key"}`),
	}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if ack := readGQLFrame(t, r); ack.Type != "connection_ack" {
		t.Fatalf("expected connection_ack, got %q", ack.Type)
	}

	subscribe := func(id, contractID string) gqlMessage {
		payload, _ := json.Marshal(map[string]interface{}{
			"variables": map[string]string{"contractId": contractID},
		})
		if err := writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: id, Payload: payload}); err != nil {
			t.Fatalf("write subscribe %s: %v", id, err)
		}
		return readGQLFrame(t, r)
	}

	// Fill the connection up to the cap — none of these should error.
	for i := 0; i < gqlMaxSubsPerConn; i++ {
		// A successful subscribe produces no immediate message (only "next"
		// on a broadcast), so instead of reading here we just don't expect
		// an "error" frame — verified by the (cap+1)th subscribe below,
		// which reads the first frame off the wire and must be that error,
		// proving nothing earlier queued one up.
		id := "sub" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		payload, _ := json.Marshal(map[string]interface{}{
			"variables": map[string]string{"contractId": "C"},
		})
		if err := writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: id, Payload: payload}); err != nil {
			t.Fatalf("write subscribe %s: %v", id, err)
		}
	}

	// One more subscription over the cap must be rejected with an error
	// frame carrying the original operation id.
	msg := subscribe("over-cap", "C")
	if msg.Type != "error" {
		t.Fatalf("expected error frame once the per-connection subscription cap is exceeded, got %q (payload %s)", msg.Type, msg.Payload)
	}
	if msg.ID != "over-cap" {
		t.Fatalf("expected error id over-cap, got %q", msg.ID)
	}
}

func TestGQLHandler_InvalidKeyClosesWithError(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return false }
	conn, r := testGQLPipe(t, hub, validateKey)

	if err := writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":"bad-key"}`),
	}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "connection_error" {
		t.Fatalf("expected connection_error, got %q", msg.Type)
	}
}

func TestGQLHandler_Topic0Filter(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return k == "key" }
	conn, r := testGQLPipe(t, hub, validateKey)

	_ = writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":"key"}`),
	})
	readGQLFrame(t, r) // discard ack

	// Subscribe with topic0 filter "wanted".
	subPayload, _ := json.Marshal(map[string]interface{}{
		"variables": map[string]string{"contractId": "CF", "topic0": "wanted"},
		"query":     "subscription($cid:String!,$t0:String){contractEvents(contractId:$cid,topic0:$t0){id}}",
	})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "sub1", Payload: json.RawMessage(subPayload)})

	// Wait for registration.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Broadcast event with non-matching topic — must be filtered.
	hub.Broadcast("CF", []byte(`{"id":"e0","contract_id":"CF","topics":["ignored"]}`))
	// Broadcast event with matching topic — must arrive.
	hub.Broadcast("CF", []byte(`{"id":"e1","contract_id":"CF","topics":["wanted"]}`))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.Type != "next" {
		t.Fatalf("expected next, got %q", msg.Type)
	}

	var outer struct {
		Data struct {
			ContractEvents struct {
				ID string `json:"id"`
			} `json:"contractEvents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg.Payload, &outer); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outer.Data.ContractEvents.ID != "e1" {
		t.Errorf("expected filtered event e1, got %q", outer.Data.ContractEvents.ID)
	}
}

func TestGQLHandler_CompleteStopsDelivery(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return true }
	conn, r := testGQLPipe(t, hub, validateKey)

	_ = writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":""}`),
	})
	readGQLFrame(t, r) // discard ack

	subPayload, _ := json.Marshal(map[string]interface{}{
		"variables": map[string]string{"contractId": "CX"},
		"query":     "subscription($cid:String!){contractEvents(contractId:$cid){id}}",
	})
	_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: "s1", Payload: json.RawMessage(subPayload)})

	// Wait for registration.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Send complete — hub must unregister the subscriber.
	_ = writeGQLFrame(conn, gqlMessage{Type: "complete", ID: "s1"})

	// Wait for hub to process the complete.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	hub.mu.RLock()
	remaining := len(hub.clients)
	hub.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("expected 0 hub subscribers after complete, got %d", remaining)
	}
}

func TestGQLHandler_TwoSubscriptionsOneSendPerEvent(t *testing.T) {
	hub := NewHub()
	validateKey := func(k string) bool { return true }
	conn, r := testGQLPipe(t, hub, validateKey)

	_ = writeGQLFrame(conn, gqlMessage{
		Type:    "connection_init",
		Payload: json.RawMessage(`{"Authorization":""}`),
	})
	readGQLFrame(t, r) // ack

	for _, id := range []string{"sub-a", "sub-b"} {
		p, _ := json.Marshal(map[string]interface{}{
			"variables": map[string]string{"contractId": id + "-contract"},
			"query":     "subscription($cid:String!){contractEvents(contractId:$cid){id}}",
		})
		_ = writeGQLFrame(conn, gqlMessage{Type: "subscribe", ID: id, Payload: json.RawMessage(p)})
	}

	// Wait for both subscriptions.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		n := len(hub.clients)
		hub.mu.RUnlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Broadcast to only sub-a's contract; sub-b must not receive it.
	hub.Broadcast("sub-a-contract", []byte(`{"id":"only-a","contract_id":"sub-a-contract","topics":[]}`))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg := readGQLFrame(t, r)
	if msg.ID != "sub-a" {
		t.Errorf("expected delivery to sub-a, got id=%q", msg.ID)
	}
}
