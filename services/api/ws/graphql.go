package ws

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
)

const (
	gqlProtocol     = "graphql-transport-ws"
	gqlInitTimeout  = 10 * time.Second
	gqlPingInterval = 30 * time.Second
	gqlMaxFrameSize = 1 << 20 // 1 MiB per frame

	// gqlMaxQueryLen and gqlMaxSubsPerConn are the "depth/complexity" guard
	// equivalents for this protocol (issue #317). There is no general
	// GraphQL execution engine here — subscribe payloads are parsed by
	// gqlParseSubscribe into a fixed shape (contractId, topic0), so there is
	// no query tree to compute a depth/complexity score over. What actually
	// exists to abuse is (a) an arbitrarily long `query` string being sent
	// repeatedly, and (b) a single connection opening unbounded numbers of
	// concurrent subscriptions, each with its own forwarding goroutine and
	// hub registration. Both are capped directly instead.
	gqlMaxQueryLen    = 8 << 10 // 8 KiB — generous for a hand-written subscribe query string
	gqlMaxSubsPerConn = 50
)

// gqlMessage is a graphql-transport-ws protocol envelope.
type gqlMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// gqlSub is a GraphQL subscription registered with the Hub.
type gqlSub struct {
	cid  string
	send chan []byte
	// onSlow is called at most once if this subscription's send buffer stays
	// full for maxConsecutiveDrops broadcasts in a row (issue #224). A single
	// connection can multiplex several subscriptions, so the callback closes
	// the whole connection's shared closeSlow channel, not just this op.
	onSlow func()
}

func (s *gqlSub) getContractID() string { return s.cid }
func (s *gqlSub) trySend(msg []byte) bool {
	select {
	case s.send <- msg:
		return true
	default:
		return false
	}
}
func (s *gqlSub) shutdown() { close(s.send) }
func (s *gqlSub) disconnect() {
	if s.onSlow == nil {
		return // not wired up (e.g. a test double) — nothing to signal.
	}
	s.onSlow()
}

// gqlConn serialises writes to a hijacked WebSocket connection.
type gqlConn struct {
	bufrw *bufio.ReadWriter
	mu    sync.Mutex
}

func (c *gqlConn) write(msg gqlMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeTextFrame(c.bufrw, data)
}

func (c *gqlConn) writeClose(code uint16, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeCloseFrame(c.bufrw, code, reason)
}

func (c *gqlConn) writePong(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeFrame(c.bufrw, 0x8A, payload) // FIN + opcode 0xA (pong)
}

// GraphQLDeps carries what the GraphQL transport needs beyond the event hub
// (issue #223). Every field is optional so an incompletely-wired process
// degrades rather than panicking: with no Backend the query operations answer
// UNAVAILABLE while subscriptions keep working, and with no Auth or
// RateLimiter the corresponding check is skipped exactly as the REST chain
// skips it when its own dependency is absent.
type GraphQLDeps struct {
	// Auth resolves a raw API key to the key's identity. It replaces the
	// bare func(string) bool this handler used to take, because a boolean
	// cannot carry the two things parity needs: which key is connected (to
	// rate-limit it) and which network it is scoped to (to scope its reads).
	Auth GraphQLAuthFunc

	// RateLimiter is consulted per operation, not per connection. A
	// WebSocket client sends no X-API-Key header, so the HTTP-level
	// TieredRateLimit middleware sees no key and lets the upgrade through
	// unmetered; without this, a GraphQL connection could issue unlimited
	// operations while a REST caller with the same key is throttled.
	RateLimiter GraphQLRateLimiter

	// Backend resolves the query operations. Subscriptions do not use it.
	Backend EventsBackend
}

// GraphQLAuthFunc validates an API key presented in connection_init and
// returns the key's identity. ok is false for a missing, unknown or revoked
// key. keyID identifies the key for rate limiting; network is the key's
// network, which scopes every read on the connection.
type GraphQLAuthFunc func(ctx context.Context, key string) (keyID, network string, ok bool)

// GraphQLRateLimiter reports whether one more operation is allowed for keyID.
// It returns the same allow/deny decision the REST tiered limiter makes for
// the same key, so a client cannot escape its tier by switching transport.
type GraphQLRateLimiter func(ctx context.Context, keyID string) (allowed bool, err error)

// gqlSession is the authenticated state of one connection: established at
// connection_init and read by every operation afterwards.
type gqlSession struct {
	keyID   string
	network string
}

// GraphQLHandler returns an http.HandlerFunc serving the
// graphql-transport-ws protocol for contract event queries and subscriptions
// (issues #223, #317).
//
// The handler shares the same Hub as the REST WebSocket endpoint — only one
// Redis XREADGROUP reader runs per process, regardless of how many
// connections are open.
//
// Authentication happens in the connection_init phase via an Authorization
// field in the payload. Invalid or missing keys receive connection_error and
// the connection is closed. The key's network is captured at that point and
// applied to every subsequent operation, so a caller cannot read another
// network's data by naming it in a query — the same server-side enforcement
// the REST handlers apply via the API key context.
func GraphQLHandler(hub *Hub, deps GraphQLDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "WebSocket upgrade required", http.StatusUpgradeRequired)
			return
		}
		if !strings.Contains(r.Header.Get("Sec-Websocket-Protocol"), gqlProtocol) {
			http.Error(w, "graphql-transport-ws protocol required", http.StatusBadRequest)
			return
		}

		conn, bufrw, err := upgradeGQLWebSocket(w, r)
		if err != nil {
			slog.Error("graphql: WebSocket upgrade failed", "err", err)
			return
		}

		serveGQL(conn, bufrw, hub, deps)
	}
}

// serveGQL runs the graphql-transport-ws protocol loop on an already-upgraded
// connection. Extracted for testability.
func serveGQL(conn net.Conn, bufrw *bufio.ReadWriter, hub *Hub, deps GraphQLDeps) {
	gc := &gqlConn{bufrw: bufrw}

	type frameMsg struct {
		data   []byte
		opcode byte
		err    error
	}
	reads := make(chan frameMsg)
	done := make(chan struct{})
	// Shared by every gqlSub opened on this connection: one connection can
	// multiplex several subscriptions, and any one of them exceeding the
	// drop threshold closes the whole socket (issue #224).
	closeSlow := make(chan struct{})
	var closeSlowOnce sync.Once

	// conn.Close() runs last so the read goroutine unblocks via I/O error.
	defer func() { _ = conn.Close() }()
	// Closing done signals the read goroutine to exit via select.
	defer close(done)

	go func() {
		for {
			data, opcode, err := readClientFrame(bufrw.Reader)
			select {
			case reads <- frameMsg{data, opcode, err}:
				if err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	subs := make(map[string]*gqlSub)
	// Unregister all active subscriptions first so forwarding goroutines
	// receive the channel-close signal before the connection is torn down.
	defer func() {
		for _, sub := range subs {
			hub.unregister(sub)
		}
	}()

	initTimer := time.NewTimer(gqlInitTimeout)
	defer initTimer.Stop()
	pingTicker := time.NewTicker(gqlPingInterval)
	defer pingTicker.Stop()

	authenticated := false
	// Captured at connection_init and applied to every later operation, so
	// the key's network scopes its reads and its id identifies it to the
	// rate limiter.
	var session gqlSession

	for {
		select {
		case <-initTimer.C:
			// Timer fired: close if client never authenticated.
			if !authenticated {
				_ = gc.write(gqlMessage{Type: "connection_error"})
				return
			}
			// Rare race: timer channel arrived after Stop() returned false.

		case <-pingTicker.C:
			if err := gc.write(gqlMessage{Type: "ping"}); err != nil {
				return
			}

		case <-closeSlow:
			// Fill-policy disconnect (issue #224): a subscription on this
			// connection exceeded the consecutive-drop threshold.
			_ = gc.writeClose(closeStatusPolicyViolation, "slow consumer: buffer exceeded")
			return

		case fm := <-reads:
			if fm.err != nil {
				return
			}

			switch fm.opcode {
			case 0x8: // WebSocket close
				return
			case 0x9: // WebSocket ping → pong
				_ = gc.writePong(fm.data)
				continue
			case 0xA: // WebSocket pong — ignore
				continue
			}

			if fm.opcode != 0x1 && fm.opcode != 0x2 {
				continue // skip non-text/binary frames
			}

			var msg gqlMessage
			if err := json.Unmarshal(fm.data, &msg); err != nil {
				slog.Debug("graphql: malformed message", "err", err)
				return
			}

			switch msg.Type {
			case "connection_init":
				key := gqlExtractKey(msg.Payload)
				keyID, network, ok := gqlAuthenticate(context.Background(), deps.Auth, key)
				if !ok {
					_ = gc.write(gqlMessage{Type: "connection_error"})
					return
				}
				authenticated = true
				session = gqlSession{keyID: keyID, network: network}
				initTimer.Stop()
				if err := gc.write(gqlMessage{Type: "connection_ack"}); err != nil {
					return
				}

			case "subscribe":
				if !authenticated {
					_ = gc.write(gqlMessage{Type: "connection_error"})
					return
				}

				// Rate limit every operation against the connected key's own
				// tier (issue #223). The HTTP-level TieredRateLimit
				// middleware cannot do this: it keys off the X-API-Key
				// header, which a WebSocket client does not send — it
				// authenticates in the connection_init payload instead — so
				// without this check a GraphQL connection would issue
				// unlimited operations while a REST caller holding the same
				// key is throttled.
				if allowed, err := gqlCheckRateLimit(context.Background(), deps.RateLimiter, session.keyID); !allowed {
					_ = gc.write(gqlCodedErrorMsg(msg.ID, err))
					continue
				}

				// A "subscribe" message carries either a subscription or a
				// query — the graphql-transport-ws protocol uses the same
				// message type for both, and the operation's own document
				// says which it is. Queries resolve once and complete;
				// subscriptions register with the hub and stream.
				if !gqlIsSubscription(msg.Payload) {
					if _, exists := subs[msg.ID]; exists {
						_ = gc.write(gqlErrorMsg(msg.ID, "subscription id already in use"))
						continue
					}
					op, err := gqlParseQuery(msg.Payload)
					if err != nil {
						_ = gc.write(gqlCodedErrorMsg(msg.ID, gqlErrf(httputil.INVALID_ARGUMENT, "%s", err.Error())))
						continue
					}
					data, rerr := gqlResolveQuery(context.Background(), deps.Backend, op, session.network)
					if rerr != nil {
						_ = gc.write(gqlCodedErrorMsg(msg.ID, rerr))
						continue
					}
					if err := gc.write(gqlDataMsg(msg.ID, data)); err != nil {
						return
					}
					// A query is a single-result operation: one next, then
					// complete. Nothing is registered with the hub, so there
					// is nothing to unregister on the client's behalf.
					if err := gc.write(gqlMessage{Type: "complete", ID: msg.ID}); err != nil {
						return
					}
					continue
				}

				contractID, topic0, err := gqlParseSubscribe(msg.Payload)
				if err != nil {
					_ = gc.write(gqlErrorMsg(msg.ID, err.Error()))
					continue
				}
				if _, exists := subs[msg.ID]; exists {
					_ = gc.write(gqlErrorMsg(msg.ID, "subscription id already in use"))
					continue
				}
				// Cap concurrent subscriptions per connection (issue #317):
				// each subscription holds a hub registration and a forwarding
				// goroutine, so an unbounded number of them on one socket is
				// a resource-exhaustion vector even though no single one is
				// individually "complex".
				if len(subs) >= gqlMaxSubsPerConn {
					_ = gc.write(gqlErrorMsg(msg.ID, fmt.Sprintf("maximum %d concurrent subscriptions per connection", gqlMaxSubsPerConn)))
					continue
				}
				sub := &gqlSub{
					cid:    contractID,
					send:   make(chan []byte, sendBufSize),
					onSlow: func() { closeSlowOnce.Do(func() { close(closeSlow) }) },
				}
				hub.register(sub)
				subs[msg.ID] = sub
				go gqlForward(sub, msg.ID, topic0, gc)

			case "complete":
				if sub, ok := subs[msg.ID]; ok {
					hub.unregister(sub)
					delete(subs, msg.ID)
				}

			case "ping":
				_ = gc.write(gqlMessage{Type: "pong", Payload: msg.Payload})

			case "pong":
				// keep-alive acknowledgement — no-op
			}
		}
	}
}

// gqlForward reads events from sub and writes graphql-transport-ws next
// messages to gc. It exits when sub.send is closed (on hub unregister) or
// when a write fails (connection dead).
func gqlForward(sub *gqlSub, opID, topic0 string, gc *gqlConn) {
	for raw := range sub.send {
		if topic0 != "" && !gqlMatchesTopic0(raw, topic0) {
			continue
		}
		if err := gc.write(gqlNextMsg(opID, raw)); err != nil {
			return
		}
	}
}

// upgradeGQLWebSocket performs the RFC 6455 handshake and negotiates the
// graphql-transport-ws subprotocol in the 101 response.
func upgradeGQLWebSocket(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, nil, http.ErrNotSupported
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return nil, nil, http.ErrNotSupported
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + computeAccept(key) + "\r\n" +
		"Sec-WebSocket-Protocol: " + gqlProtocol + "\r\n\r\n"
	if _, err := bufrw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := bufrw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	return conn, bufrw, nil
}

// readClientFrame reads one RFC 6455 frame from a client.
// Client→server frames are always masked per spec.
func readClientFrame(r *bufio.Reader) ([]byte, byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, 0, err
	}

	opcode := header[0] & 0x0F
	masked := header[1]&0x80 != 0
	payloadLen := int64(header[1] & 0x7F)

	switch payloadLen {
	case 126:
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, 0, err
		}
		payloadLen = int64(binary.BigEndian.Uint16(b))
	case 127:
		b := make([]byte, 8)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, 0, err
		}
		payloadLen = int64(binary.BigEndian.Uint64(b))
	}

	if payloadLen > gqlMaxFrameSize {
		return nil, 0, fmt.Errorf("frame payload too large: %d bytes", payloadLen)
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return nil, 0, err
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, err
	}

	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	return payload, opcode, nil
}

var (
	reContractID = regexp.MustCompile(`contractId\s*:\s*"([^"]+)"`)
	reTopic0     = regexp.MustCompile(`topic0\s*:\s*"([^"]+)"`)
)

// gqlParseSubscribe extracts contractId and topic0 from a subscribe payload.
// Variables take precedence over inline query arguments.
func gqlParseSubscribe(payload json.RawMessage) (contractID, topic0 string, err error) {
	if payload == nil {
		return "", "", fmt.Errorf("missing subscribe payload")
	}
	var p struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	if jsonErr := json.Unmarshal(payload, &p); jsonErr != nil {
		return "", "", fmt.Errorf("invalid subscribe payload")
	}

	// Reject an abusively long query string outright rather than regex-
	// matching against it — the "depth/complexity" guard equivalent for a
	// protocol with no query executor (issue #317).
	if len(p.Query) > gqlMaxQueryLen {
		return "", "", fmt.Errorf("query exceeds maximum length of %d bytes", gqlMaxQueryLen)
	}

	if p.Variables != nil {
		if v, ok := p.Variables["contractId"].(string); ok {
			contractID = v
		}
		if v, ok := p.Variables["topic0"].(string); ok {
			topic0 = v
		}
	}

	if contractID == "" {
		if m := reContractID.FindStringSubmatch(p.Query); len(m) == 2 {
			contractID = m[1]
		}
	}
	if topic0 == "" {
		if m := reTopic0.FindStringSubmatch(p.Query); len(m) == 2 {
			topic0 = m[1]
		}
	}

	if contractID == "" {
		return "", "", fmt.Errorf("contractId is required")
	}
	return contractID, topic0, nil
}

// gqlExtractKey extracts an API key from a connection_init payload.
// Accepts {"Authorization":"tk_..."} or {"Authorization":"Bearer tk_..."}.
func gqlExtractKey(payload json.RawMessage) string {
	if payload == nil {
		return ""
	}
	var p struct {
		Authorization string `json:"Authorization"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return strings.TrimPrefix(p.Authorization, "Bearer ")
}

// gqlNextMsg wraps raw event bytes in a graphql-transport-ws next envelope.
// Snake-case payload fields are remapped to the camelCase GraphQL schema names.
func gqlNextMsg(opID string, raw []byte) gqlMessage {
	var src map[string]interface{}
	if err := json.Unmarshal(raw, &src); err != nil {
		src = map[string]interface{}{}
	}

	ev := make(map[string]interface{}, 10)
	pick := func(dst, key string) {
		if v, ok := src[key]; ok {
			ev[dst] = v
		}
	}
	pick("id", "id")
	pick("contractId", "contract_id")
	pick("ledgerSequence", "ledger_sequence")
	pick("ledgerTimestamp", "ledger_timestamp")
	pick("txHash", "transaction_hash")
	pick("eventIndex", "event_index")
	pick("eventType", "event_type")
	pick("topics", "topics")
	pick("data", "data")
	pick("createdAt", "created_at")

	inner, _ := json.Marshal(map[string]interface{}{"contractEvents": ev})
	return gqlMessage{
		Type:    "next",
		ID:      opID,
		Payload: json.RawMessage(`{"data":` + string(inner) + `}`),
	}
}

// gqlErrorMsg builds an error message for a subscription operation.
func gqlErrorMsg(id, message string) gqlMessage {
	errs, _ := json.Marshal([]map[string]interface{}{{"message": message}})
	return gqlMessage{Type: "error", ID: id, Payload: json.RawMessage(errs)}
}

// gqlMatchesTopic0 reports whether the first topic in a raw event payload
// equals topic0. Handles both JSON-array and JSON-string-encoded-array formats.
func gqlMatchesTopic0(payload []byte, topic0 string) bool {
	var ev struct {
		Topics interface{} `json:"topics"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	switch t := ev.Topics.(type) {
	case []interface{}:
		return len(t) > 0 && fmt.Sprint(t[0]) == topic0
	case string:
		var arr []string
		if err := json.Unmarshal([]byte(t), &arr); err != nil {
			return false
		}
		return len(arr) > 0 && arr[0] == topic0
	}
	return false
}
