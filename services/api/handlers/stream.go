package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/redis/go-redis/v9"
)

const (
	eventStreamKey = "trident:events"
	streamReadWait = time.Second

	// sseWriteDeadline bounds a single SSE write. A stalled client (full TCP
	// send buffer) must not block this connection's goroutine forever
	// (issue #224); the deadline turns a stuck socket into a write error so
	// the handler returns and cleans up instead of leaking.
	sseWriteDeadline = 10 * time.Second
)

// sseHeartbeatInterval is how often an idle stream emits a comment frame. It
// stays well under the 60s idle timeouts of the Helm nginx config and common
// cloud load balancers.
var sseHeartbeatInterval = 15 * time.Second

type streamRedisClient interface {
	XRead(ctx context.Context, a *redis.XReadArgs) *redis.XStreamSliceCmd
	XRevRangeN(ctx context.Context, key, start, stop string, count int64) *redis.XMessageSliceCmd
}

// eventStreamGapEvent is the documented SSE event sent when a requested
// Last-Event-ID is older than the stream retention window.
const eventStreamGapEvent = `event: gap\ndata: {"message":"requested Last-Event-ID is outside the retention window; resuming from oldest available"}\n\n`

// Stream returns an SSE handler that forwards new Redis Stream events for one
// contract. All connections share one blocking XREAD (see streamFanout), so
// Redis connection use does not grow with the number of clients, and an idle
// stream emits a `: ping` comment every sseHeartbeatInterval so proxies do not
// time it out.
//
// Events are limited to the authenticated key's network, and every payload
// carries the event UUID as `id` (issue #741).
//
// It honours the standard SSE Last-Event-ID header (issue #235):
//   - On first connect: tail the stream from the latest id.
//   - On reconnect with Last-Event-ID: resume from that id + 1.
//   - If the requested id is older than the retention window, emit a `gap` event
//     and resume from the oldest available id.
//   - Every SSE event includes an `id:` field so the browser automatically sends
//     Last-Event-ID on reconnect.
func Stream(rdb streamRedisClient) http.HandlerFunc {
	fanout := newStreamFanout(rdb)
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if verr := validation.RejectUnknownParams(q, "contractId", "topic0"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		// The stream filters server-side on this id, so a malformed one would
		// otherwise silently match nothing for the life of the connection.
		contractID := q.Get("contractId")
		if verr := validation.ValidateRequiredContractID("contractId", contractID); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		rc := http.NewResponseController(w)

		// Subscribe before resolving the resume point so nothing published
		// while the replay runs is missed; live entries already replayed are
		// dropped by id below.
		sub, err := fanout.subscribe(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			slog.Error("sse: failed to read Redis Stream tail", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "event stream is unavailable")
			return
		}
		defer fanout.unsubscribe(sub)

		// Honour Last-Event-ID for resumption (issue #235).
		replayFrom := ""
		if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
			// Verify the requested id exists in the stream. If not, emit a gap
			// signal and resume from the oldest available.
			msgs, lookupErr := rdb.XRevRangeN(r.Context(), eventStreamKey, lastEventID, lastEventID, 1).Result()
			if lookupErr != nil || len(msgs) == 0 {
				if _, writeErr := fmt.Fprint(w, eventStreamGapEvent); writeErr != nil {
					slog.Warn("sse: write failed, disconnecting slow consumer", "contractId", contractID, "err", writeErr)
					return
				}
				_ = rc.Flush()

				oldest, err := earliestStreamID(r.Context(), rdb)
				if err != nil {
					if r.Context().Err() != nil {
						return
					}
					slog.Error("sse: failed to read earliest stream id", "err", err)
					httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "event stream is unavailable")
					return
				}
				replayFrom = oldest
			} else {
				replayFrom = lastEventID
			}
		}

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if err := rc.Flush(); err != nil {
			slog.Warn("sse: response writer cannot flush", "err", err)
			return
		}

		topic0 := q.Get("topic0")
		network := middleware.NetworkFromContext(r.Context())

		// write emits one frame under the write deadline. It reports false when
		// the client is gone or too slow and the handler must return.
		write := func(frame string) bool {
			if err := rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline)); err != nil &&
				!errors.Is(err, http.ErrNotSupported) {
				slog.Warn("sse: failed to set write deadline", "err", err)
			}
			_, err := fmt.Fprint(w, frame)
			if err == nil {
				// The flush is where a stalled socket usually times out, so its
				// error must count as a failed write too.
				err = rc.Flush()
			}
			if err != nil {
				metricSSESlowConsumerDisconnects.Add(1)
				slog.Warn("sse: write failed, disconnecting slow consumer", "contractId", contractID, "err", err)
				return false
			}
			return true
		}
		wanted := func(ev sseEvent) bool {
			return ev.ContractID == contractID && ev.Network == network && (topic0 == "" || ev.Topic0 == topic0)
		}
		// Emit SSE id: field so browsers auto-send Last-Event-ID on reconnect.
		send := func(ev sseEvent) bool {
			return write(fmt.Sprintf("id: %s\ndata: %s\n\n", ev.StreamID, ev.Payload))
		}

		lastSent := ""
		if replayFrom != "" {
			cursor := replayFrom
			for {
				streams, readErr := rdb.XRead(r.Context(), &redis.XReadArgs{
					Streams: []string{eventStreamKey, cursor},
					Count:   100,
					Block:   -1,
				}).Result()
				if readErr != nil || len(streams) == 0 {
					if readErr != nil && !errors.Is(readErr, redis.Nil) && r.Context().Err() == nil {
						slog.Warn("sse: replay read failed", "err", readErr)
					}
					break
				}
				progressed := false
				for _, stream := range streams {
					for _, msg := range stream.Messages {
						cursor, lastSent, progressed = msg.ID, msg.ID, true
						if ev, ok := newSSEEvent(msg); ok && wanted(ev) && !send(ev) {
							return
						}
					}
				}
				if !progressed {
					break
				}
			}
		}

		heartbeat := time.NewTicker(sseHeartbeatInterval)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				// A comment frame keeps idle-timeout proxies and load
				// balancers from closing a quiet stream.
				if !write(": ping\n\n") {
					return
				}
			case ev, ok := <-sub.ch:
				if !ok {
					if fanout.wasOverflowed(sub) {
						metricSSESlowConsumerDisconnects.Add(1)
						slog.Warn("sse: client fell behind the shared reader, disconnecting", "contractId", contractID)
					}
					return
				}
				if lastSent != "" && !streamIDAfter(ev.StreamID, lastSent) {
					continue
				}
				if wanted(ev) && !send(ev) {
					return
				}
			}
		}
	}
}

func latestStreamID(ctx context.Context, rdb streamRedisClient) (string, error) {
	messages, err := rdb.XRevRangeN(ctx, eventStreamKey, "+", "-", 1).Result()
	if err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "0-0", nil
	}
	return messages[0].ID, nil
}

func earliestStreamID(ctx context.Context, rdb streamRedisClient) (string, error) {
	// XRange with start "-" and stop "+" returns messages in ascending order.
	// Limit 1 gives us the oldest message in the stream.
	cmd := rdb.XRevRangeN(ctx, eventStreamKey, "+", "-", 1)
	messages, err := cmd.Result()
	if err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "0-0", nil
	}
	return messages[0].ID, nil
}

func redisString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}
