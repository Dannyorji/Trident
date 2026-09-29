// Package ws implements a WebSocket hub that fans out contract events from
// Redis Streams to connected browser/client subscribers.
package ws

import (
	"log/slog"
	"sync"

	"github.com/Depo-dev/trident/services/api/internal/logsampling"
	"github.com/Depo-dev/trident/services/api/internal/metrics"
)

// maxConsecutiveDrops is the fill policy threshold (issue #224): a subscriber
// whose send buffer is full this many broadcasts in a row is a stalled
// consumer, not a momentary blip, and is force-disconnected with a close
// code rather than left to buffer drops indefinitely.
const maxConsecutiveDrops = 5

// registerLogSampler and unregisterLogSampler thin the per-connection
// register/unregister debug logs (issue #239): metrics.WSConnectsTotal /
// WSDisconnectsTotal already give exact counts, so these logs exist for
// occasional manual debugging, not for counting — logging every one of a
// busy deployment's churn is pure log volume with no analytical benefit and
// can drown out other debug output. Separate samplers so each event type
// gets its own predictable 1-in-50 coverage rather than sharing one counter
// and letting a burst of one type starve the other's sample.
var (
	registerLogSampler   = logsampling.New(50)
	unregisterLogSampler = logsampling.New(50)
)

// subscriber is the registration interface shared by REST WebSocket clients
// and GraphQL subscription channels.
type subscriber interface {
	getContractID() string
	trySend(msg []byte) bool // false = dropped (slow consumer)
	shutdown()               // called by Hub.unregister to signal cleanup
	disconnect()             // called by Hub when the drop threshold is hit
}

// client is a REST WebSocket subscriber.
type client struct {
	contractID string
	send       chan []byte
	// closeSlow is closed exactly once, by disconnect(), to tell the
	// connection's write loop to send a close frame and exit. It is separate
	// from send/shutdown because shutdown() is hub-unregister cleanup (may
	// run after the connection is already gone), while disconnect() is the
	// hub telling a still-live connection to close itself.
	closeSlow chan struct{}
	once      sync.Once
}

func (c *client) getContractID() string { return c.contractID }

func (c *client) trySend(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

func (c *client) shutdown() { close(c.send) }

func (c *client) disconnect() {
	if c.closeSlow == nil {
		return // not wired up (e.g. a test double) — nothing to signal.
	}
	c.once.Do(func() { close(c.closeSlow) })
}

// Hub manages all active subscribers and routes broadcast messages to
// the correct subscribers based on contractId.
type Hub struct {
	mu         sync.RWMutex
	clients    map[subscriber]struct{}
	dropStreak map[subscriber]int
}

// NewHub constructs a Hub ready to use.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[subscriber]struct{}),
		dropStreak: make(map[subscriber]int),
	}
}

// register adds s to the hub's active set. Safe to call concurrently.
func (h *Hub) register(s subscriber) {
	h.mu.Lock()
	h.clients[s] = struct{}{}
	h.mu.Unlock()
	metrics.WSActiveConnections.Inc()
	metrics.WSConnectsTotal.Inc()
	if registerLogSampler.Allow() {
		slog.Debug("ws: client registered", "contractId", s.getContractID())
	}
}

// unregister removes s from the hub and calls shutdown so its goroutine
// can exit cleanly.
func (h *Hub) unregister(s subscriber) {
	h.mu.Lock()
	_, ok := h.clients[s]
	if ok {
		delete(h.clients, s)
		delete(h.dropStreak, s)
		s.shutdown()
	}
	h.mu.Unlock()
	if ok {
		metrics.WSActiveConnections.Dec()
		metrics.WSDisconnectsTotal.Inc()
	}
	if unregisterLogSampler.Allow() {
		slog.Debug("ws: client unregistered", "contractId", s.getContractID())
	}
}

// Broadcast delivers msg to every subscriber watching contractID.
// It is safe to call from any goroutine.
//
// Fill policy (issue #224): a full send buffer drops that one message rather
// than blocking the broadcaster. A subscriber that drops maxConsecutiveDrops
// broadcasts in a row is treated as a stalled consumer and disconnected via
// disconnect(), which the connection's write loop turns into a close frame.
// A successful send resets the streak, so an occasional single drop under
// bursty load never disconnects a otherwise-healthy client.
func (h *Hub) Broadcast(contractID string, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for s := range h.clients {
		if s.getContractID() != contractID {
			continue
		}
		if s.trySend(msg) {
			h.dropStreak[s] = 0
			metrics.WSMessagesTotal.WithLabelValues("sent").Inc()
			continue
		}

		metricMessagesDropped.Add(1)
		metrics.WSMessagesTotal.WithLabelValues("dropped").Inc()
		h.dropStreak[s]++
		slog.Warn("ws: dropping message for slow client", "contractId", contractID, "streak", h.dropStreak[s])

		if h.dropStreak[s] >= maxConsecutiveDrops {
			metricSlowConsumerDisconnects.Add(1)
			slog.Warn("ws: disconnecting slow consumer", "contractId", contractID, "streak", h.dropStreak[s])
			delete(h.dropStreak, s)
			s.disconnect()
		}
	}
}

// ClientCount returns the number of currently active WebSocket clients.
// Safe to call from any goroutine.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ShutdownAll closes all active client connections by calling shutdown on each
// subscriber, so in-flight SSE/WS streams terminate cleanly instead of being
// dropped by a TCP RST. This is called during graceful shutdown to ensure
// clients receive a clean close. Takes the write lock so concurrent
// register/unregister calls do not race on the client map.
func (h *Hub) ShutdownAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for s := range h.clients {
		s.shutdown()
		delete(h.clients, s)
	}
	slog.Info("ws: shutdown complete, all clients disconnected")
}
