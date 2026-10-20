package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// sseSubscriberBuffer bounds the events queued for one SSE client. A client
// that falls this far behind is disconnected rather than allowed to grow an
// unbounded queue; it resumes with Last-Event-ID.
const sseSubscriberBuffer = 256

// sseEvent is one stream entry, decoded once and shared by every subscriber.
type sseEvent struct {
	StreamID   string
	ContractID string
	Network    string
	Topic0     string
	Payload    []byte
}

// newSSEEvent decodes a stream entry. The payload carries the event UUID as
// "id" (falling back to "event_id") so a streamed event can be correlated with
// GET /v1/events/{id} and de-duplicated across reconnects.
func newSSEEvent(msg redis.XMessage) (sseEvent, bool) {
	values := make(map[string]any, len(msg.Values)+1)
	maps.Copy(values, msg.Values)
	if redisString(values["id"]) == "" {
		if id := redisString(values["event_id"]); id != "" {
			values["id"] = id
		}
	}
	payload, err := json.Marshal(values)
	if err != nil {
		slog.Warn("sse: failed to marshal stream event", "id", msg.ID, "err", err)
		return sseEvent{}, false
	}
	var topics []string
	_ = json.Unmarshal([]byte(redisString(values["topics"])), &topics)
	ev := sseEvent{
		StreamID:   msg.ID,
		ContractID: redisString(values["contract_id"]),
		Network:    redisString(values["network"]),
		Payload:    payload,
	}
	if len(topics) > 0 {
		ev.Topic0 = topics[0]
	}
	return ev, true
}

type sseSubscriber struct {
	ch chan sseEvent
	// overflowed is set (under the fanout lock) before ch is closed because the
	// subscriber stopped keeping up.
	overflowed bool
}

// streamFanout runs a single blocking XREAD for the whole process and hands
// each entry to every connected SSE client, so the number of Redis connections
// held does not grow with the number of clients. The reader runs only while at
// least one client is connected.
type streamFanout struct {
	rdb streamRedisClient

	mu      sync.Mutex
	subs    map[*sseSubscriber]struct{}
	cancel  context.CancelFunc
	running bool
}

func newStreamFanout(rdb streamRedisClient) *streamFanout {
	return &streamFanout{rdb: rdb, subs: make(map[*sseSubscriber]struct{})}
}

// subscribe registers a client and starts the shared reader if needed. The
// client receives every entry published after the reader's cursor.
func (f *streamFanout) subscribe(ctx context.Context) (*sseSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		start, err := latestStreamID(ctx, f.rdb)
		if err != nil {
			return nil, err
		}
		readCtx, cancel := context.WithCancel(context.Background())
		f.cancel = cancel
		f.running = true
		go f.read(readCtx, start)
	}
	s := &sseSubscriber{ch: make(chan sseEvent, sseSubscriberBuffer)}
	f.subs[s] = struct{}{}
	return s, nil
}

func (f *streamFanout) unsubscribe(s *sseSubscriber) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subs[s]; ok {
		delete(f.subs, s)
		close(s.ch)
	}
	if len(f.subs) == 0 && f.running {
		f.cancel()
		f.running = false
	}
}

// wasOverflowed reports whether the hub dropped s for falling behind.
func (f *streamFanout) wasOverflowed(s *sseSubscriber) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return s.overflowed
}

func (f *streamFanout) read(ctx context.Context, lastID string) {
	for ctx.Err() == nil {
		streams, err := f.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{eventStreamKey, lastID},
			Count:   100,
			Block:   streamReadWait,
		}).Result()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, redis.Nil) {
				continue
			}
			slog.Warn("sse: Redis XREAD failed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for _, stream := range streams {
			for _, msg := range stream.Messages {
				lastID = msg.ID
				if ev, ok := newSSEEvent(msg); ok {
					f.broadcast(ctx, ev)
				}
			}
		}
	}
}

func (f *streamFanout) broadcast(ctx context.Context, ev sseEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		// A superseded reader must not feed clients of its successor.
		return
	}
	for s := range f.subs {
		select {
		case s.ch <- ev:
		default:
			s.overflowed = true
			delete(f.subs, s)
			close(s.ch)
		}
	}
	if len(f.subs) == 0 && f.running {
		f.cancel()
		f.running = false
	}
}

// streamIDAfter reports whether stream id a is strictly after b. IDs are
// "<ms>-<seq>"; an empty b is before everything.
func streamIDAfter(a, b string) bool {
	if b == "" {
		return true
	}
	am, as := splitStreamID(a)
	bm, bs := splitStreamID(b)
	if am != bm {
		return am > bm
	}
	return as > bs
}

func splitStreamID(id string) (int64, int64) {
	ms, seq, _ := strings.Cut(id, "-")
	m, _ := strconv.ParseInt(ms, 10, 64)
	s, _ := strconv.ParseInt(seq, 10, 64)
	return m, s
}
