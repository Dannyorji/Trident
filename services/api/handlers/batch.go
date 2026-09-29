package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
)

const batchEventsMaxIDs = 100

// batchPerRequestConcurrency bounds how many gRPC GetEvent calls a single
// BatchGetEvents request runs at once. Without it, one request already fans
// out up to batchEventsMaxIDs unbounded goroutines/gRPC calls (issue #646);
// this matches the concurrency the webhook delivery path already bounds
// itself to (globalDeliverySem in webhooks.go, issue #454).
const batchPerRequestConcurrency = 20

// batchGlobalSem bounds total concurrent gRPC calls across every in-flight
// BatchGetEvents request in the process, so many concurrent batch requests
// each fanning out batchPerRequestConcurrency calls still can't collectively
// spike gRPC connection/stream pressure without limit (issue #646).
var batchGlobalSem = make(chan struct{}, 100)

type batchRequest struct {
	IDs []string `json:"ids"`
}

// BatchEventsResponse is the response envelope for POST /v1/events/batch.
type BatchEventsResponse struct {
	Events  []*EventJSON `json:"events"`
	Missing []string     `json:"missing"`
}

// BatchGetEvents handles POST /v1/events/batch.
//
// Accepts a JSON body `{"ids": ["uuid1", ...]}`, validates each ID as a UUID v4,
// fetches up to batchEventsMaxIDs events in parallel via gRPC GetEvent, and
// returns found events plus a missing array for any IDs that were not indexed.
//
// Batch contract (issue #228): events and missing both preserve the request
// order of ids; duplicate ids are deduplicated on first occurrence; more than
// batchEventsMaxIDs ids (duplicates included) is INVALID_ARGUMENT.
func BatchGetEvents(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if middleware.IsBodyTooLarge(err) {
			middleware.WriteBodyTooLarge(w, r)
			return
		}
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "request body must be valid JSON")
		return
	}

	if len(req.IDs) == 0 {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "ids must be a non-empty array")
		return
	}

	// The limit applies to the ids as sent, duplicates included: a request
	// over the cap is a client bug either way (issue #228).

	if len(req.IDs) > batchEventsMaxIDs {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT,
			fmt.Sprintf("maximum %d IDs per request", batchEventsMaxIDs))
		return
	}

	// Validate all UUIDs up front; collect invalid ones and return 400.
	var invalid []string
	for _, id := range req.IDs {
		if err := validation.ValidateEventID(id); err != nil {
			invalid = append(invalid, id)
		}
	}
	if len(invalid) > 0 {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT,
			fmt.Sprintf("one or more IDs are not valid UUID v4: %s", strings.Join(invalid, ", ")))
		return
	}

	// Backend availability is checked after validation: a malformed body is a
	// 400 whether or not the backend is up (issue #222).
	if eventsClient == nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "gRPC backend unavailable")
		return
	}

	// Duplicate ids are deduplicated preserving first-occurrence order: each
	// unique id is fetched once and appears at most once in the response,
	// in events or in missing (issue #228; documented in the OpenAPI spec).
	seen := make(map[string]struct{}, len(req.IDs))
	ids := make([]string, 0, len(req.IDs))
	for _, id := range req.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	type result struct {
		id    string
		event *gen.Event
		found bool
	}

	// Network enforced from authenticated API key context, matching the
	// single-event lookup (events.go) — otherwise a single id batched
	// through this route bypasses the network scope GET /v1/events/{id}
	// enforces (issue #613).
	network := middleware.NetworkFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), grpcCallTimeout)
	defer cancel()

	results := make([]result, len(ids))
	localSem := make(chan struct{}, batchPerRequestConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		localSem <- struct{}{}
		batchGlobalSem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			event, err := eventsClient.GetEvent(ctx, &gen.GetEventRequest{Id: id, Network: network})
			defer func() { <-localSem }()
			defer func() { <-batchGlobalSem }()
			event, err := grpcclient.CallWithRetry(ctx, 1, func(ctx context.Context) (*gen.Event, error) {
				return eventsClient.GetEvent(ctx, &gen.GetEventRequest{Id: id, Network: network})
			})
			if err != nil {
				results[i] = result{id: id, found: false}
				return
			}
			results[i] = result{id: id, event: event, found: true}
		}(i, id)
	}
	wg.Wait()

	events := make([]*EventJSON, 0, len(ids))
	var missing []string
	for _, r := range results {
		if r.found {
			events = append(events, protoEventToJSON(r.event))
		} else {
			missing = append(missing, r.id)
		}
	}
	if missing == nil {
		missing = []string{}
	}

	writeJSON(w, http.StatusOK, BatchEventsResponse{
		Events:  events,
		Missing: missing,
	})
}
