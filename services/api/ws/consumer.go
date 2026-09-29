package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// StreamKey is the Redis Stream the indexer publishes events to. Exported
// so other consumers of the same stream (e.g. middleware.StartCacheInvalidator,
// issue #221) have one source of truth for the name rather than a second
// copy of the literal that could drift from this one.
const StreamKey = "trident:events"

const groupName = "trident-api"

// streamKey is a private alias for StreamKey, kept so this file's many
// internal references don't all need renaming.
const streamKey = StreamKey

// streamEntry is the JSON structure expected in each Redis Stream message value.
type streamEntry struct {
	ContractID string          `json:"contract_id"`
	Payload    json.RawMessage `json:"payload"`
}

// StartConsumer begins a Redis Streams consumer group reader. It creates the
// consumer group idempotently, uses a unique consumer name per process, and
// reads messages using XREADGROUP. Each message is acknowledged (XACK) after
// successful broadcast to the hub. PEL recovery via XAUTOCLAIM runs on startup
// and every 30s.
func StartConsumer(ctx context.Context, rdb *redis.Client, hub *Hub) {
	// Consumer name: prefer HOSTNAME, fall back to UUID if unavailable.
	consumerName, _ := os.Hostname()
	if consumerName == "" {
		consumerName = uuid.NewString()
	}

	slog.Info("ws: Redis Streams consumer starting (consumer group)", "stream", streamKey, "group", groupName, "consumer", consumerName)

	// Create consumer group idempotently: MKSTREAM ensures stream exists.
	if err := rdb.XGroupCreateMkStream(ctx, streamKey, groupName, "$").Err(); err != nil {
		// BUSYGROUP means group already exists — ignore.
		if strings.Contains(err.Error(), "BUSYGROUP") {
			slog.Info("ws: consumer group already exists, continuing")
		} else {
			slog.Error("ws: failed to create consumer group", "err", err)
			// Not fatal — continue and attempt to consume; downstream errors will surface.
		}
	}

	// Run PEL recovery (XAUTOCLAIM) once at startup.
	recoverPending(ctx, rdb, hub, consumerName)

	// Periodic PEL recovery every 30 seconds in background.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				recoverPending(ctx, rdb, hub, consumerName)
			case <-ctx.Done():
				slog.Info("ws: PEL recovery goroutine stopping")
				return
			}
		}
	}()

	for {
		// Block until messages are available or the context is cancelled.
		entries, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    groupName,
			Consumer: consumerName,
			Streams:  []string{streamKey, ">"},
			Count:    100,
			Block:    5000 * time.Millisecond,
		}).Result()

		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled — clean shutdown.
				slog.Info("ws: Redis Streams consumer stopped")
				return
			}
			slog.Error("ws: XReadGroup error", "err", err)
			// Back off briefly on transient errors to avoid a tight error loop.
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}

		for _, stream := range entries {
			for _, msg := range stream.Messages {
				// msg.ID is the message id we must XACK after processing.
				raw, ok := msg.Values["data"]
				if !ok {
					slog.Warn("ws: stream message missing 'data' field", "id", msg.ID)
					// Acknowledge to avoid stuck messages, since we can't process it.
					if err := rdb.XAck(ctx, streamKey, groupName, msg.ID).Err(); err != nil {
						slog.Warn("ws: failed to XACK malformed message", "id", msg.ID, "err", err)
					}
					continue
				}

				rawStr, ok := raw.(string)
				if !ok {
					slog.Warn("ws: stream message 'data' is not a string", "id", msg.ID)
					if err := rdb.XAck(ctx, streamKey, groupName, msg.ID).Err(); err != nil {
						slog.Warn("ws: failed to XACK malformed message", "id", msg.ID, "err", err)
					}
					continue
				}

				var entry streamEntry
				if err := json.Unmarshal([]byte(rawStr), &entry); err != nil {
					slog.Warn("ws: failed to unmarshal stream entry", "id", msg.ID, "err", err)
					if err := rdb.XAck(ctx, streamKey, groupName, msg.ID).Err(); err != nil {
						slog.Warn("ws: failed to XACK malformed message", "id", msg.ID, "err", err)
					}
					continue
				}

				if entry.ContractID == "" {
					slog.Warn("ws: stream entry missing contract_id", "id", msg.ID)
					if err := rdb.XAck(ctx, streamKey, groupName, msg.ID).Err(); err != nil {
						slog.Warn("ws: failed to XACK malformed message", "id", msg.ID, "err", err)
					}
					continue
				}

				hub.Broadcast(entry.ContractID, entry.Payload)

				// Acknowledge the message after successful broadcast.
				if err := rdb.XAck(ctx, streamKey, groupName, msg.ID).Err(); err != nil {
					slog.Warn("ws: failed to XACK message", "id", msg.ID, "err", err)
				}
			}
		}
	}
}

// recoverPending claims pending messages older than 30s using XAUTOCLAIM and
// reprocesses them locally so no messages stay stuck in the PEL.
func recoverPending(ctx context.Context, rdb *redis.Client, hub *Hub, consumerName string) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	minIdle := 30 * time.Second
	messages, _, err := rdb.XAutoClaim(ctx2, &redis.XAutoClaimArgs{
		Stream:   streamKey,
		Group:    groupName,
		Consumer: consumerName,
		MinIdle:  minIdle,
		Start:    "0-0",
	}).Result()
	if err != nil {
		// If no entries, Redis may return empty result — treat as non-fatal.
		if err != redis.Nil {
			slog.Warn("ws: XAUTOCLAIM failed", "err", err)
		}
		return
	}

	for _, msg := range messages {
		raw, ok := msg.Values["data"]
		if !ok {
			slog.Warn("ws: recovered message missing 'data'", "id", msg.ID)
			// Acknowledge to avoid loops.
			if err := rdb.XAck(ctx2, streamKey, groupName, msg.ID).Err(); err != nil {
				slog.Warn("ws: failed to XACK recovered malformed message", "id", msg.ID, "err", err)
			}
			continue
		}

		rawStr, ok := raw.(string)
		if !ok {
			slog.Warn("ws: recovered message 'data' not string", "id", msg.ID)
			if err := rdb.XAck(ctx2, streamKey, groupName, msg.ID).Err(); err != nil {
				slog.Warn("ws: failed to XACK recovered malformed message", "id", msg.ID, "err", err)
			}
			continue
		}

		var entry streamEntry
		if err := json.Unmarshal([]byte(rawStr), &entry); err != nil {
			slog.Warn("ws: failed to unmarshal recovered entry", "id", msg.ID, "err", err)
			if err := rdb.XAck(ctx2, streamKey, groupName, msg.ID).Err(); err != nil {
				slog.Warn("ws: failed to XACK recovered malformed message", "id", msg.ID, "err", err)
			}
			continue
		}

		if entry.ContractID == "" {
			slog.Warn("ws: recovered entry missing contract_id", "id", msg.ID)
			if err := rdb.XAck(ctx2, streamKey, groupName, msg.ID).Err(); err != nil {
				slog.Warn("ws: failed to XACK recovered malformed message", "id", msg.ID, "err", err)
			}
			continue
		}

		hub.Broadcast(entry.ContractID, entry.Payload)
		if err := rdb.XAck(ctx2, streamKey, groupName, msg.ID).Err(); err != nil {
			slog.Warn("ws: failed to XACK recovered message", "id", msg.ID, "err", err)
		}
	}
}
