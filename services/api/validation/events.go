// Package validation provides request parameter validation for the Trident
// REST API before parameters are forwarded to the gRPC backend.
package validation

import (
	"fmt"
	"regexp"
)

// Validation limits for GET /v1/events.
const (
	LimitMin     = 1
	LimitMax     = 200
	LimitDefault = 50
)

// validEventTypes holds the accepted values for the ?event_type filter.
var validEventTypes = map[string]bool{
	"contract":   true,
	"system":     true,
	"diagnostic": true,
}

// stellarContractRE matches a Stellar contract strkey: C followed by 55
// uppercase base32 characters (total 56 chars).
var stellarContractRE = regexp.MustCompile(`^C[A-Z2-7]{55}$`)

// uuidV4RE matches a UUID v4 in canonical lowercase form.
var uuidV4RE = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

// ValidationError carries a structured error to be returned as 400 Bad Request.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error on %q: %s", e.Field, e.Message)
}

// QueryEventsParams holds validated parameters for GET /v1/events.
type QueryEventsParams struct {
	Limit      int
	LedgerFrom *int64
	LedgerTo   *int64
	ContractID string
	Cursor     string
	EventType  string // empty = no filter; otherwise "contract", "system", or "diagnostic"
}

// ValidateQueryEvents parses and validates query-string values for GET /v1/events.
// It returns populated QueryEventsParams on success, or a *ValidationError on the
// first validation failure.
//
// Validation rules:
//   - limit:      integer in [1, 200]; defaults to 50 if absent
//   - ledgerFrom: non-negative integer if present
//   - ledgerTo:   non-negative integer if present; must be >= ledgerFrom when both present
//   - contractId: valid Stellar contract strkey (C…, 56 chars) if present
//   - cursor:     non-empty string if present (opaque; no further validation)
//   - eventType:  one of "contract", "system", "diagnostic" (case-insensitive) if present
func ValidateQueryEvents(
	limitStr, ledgerFromStr, ledgerToStr, contractID, cursor, eventTypeStr string,
) (*QueryEventsParams, *ValidationError) {
	p := &QueryEventsParams{
		ContractID: contractID,
		Cursor:     cursor,
	}

	limit, verr := ValidateLimit("limit", limitStr, LimitMin, LimitMax, LimitDefault)
	if verr != nil {
		return nil, verr
	}
	p.Limit = int(limit)

	from, to, verr := ValidateLedgerRange("ledgerFrom", "ledgerTo", ledgerFromStr, ledgerToStr)
	if verr != nil {
		return nil, verr
	}
	p.LedgerFrom, p.LedgerTo = from, to

	if verr := ValidateContractID("contractId", contractID); verr != nil {
		return nil, verr
	}

	// The cursor stays opaque here. The handler decodes it via ValidateCursor
	// because it needs the resulting paging token, and a malformed cursor is
	// rejected there with the same INVALID_ARGUMENT envelope.

	eventType, verr := ValidateEventType("event_type", eventTypeStr)
	if verr != nil {
		return nil, verr
	}
	p.EventType = eventType

	return p, nil
}

// ValidateEventID validates the :id path parameter for GET /v1/events/:id.
// Returns a *ValidationError if the value is not a valid UUID v4.
func ValidateEventID(id string) *ValidationError {
	return ValidateUUID("id", id)
}

// Validation limits for GET /v1/stats/contracts.
const (
	StatsLimitMin     = 1
	StatsLimitMax     = 100
	StatsLimitDefault = 50

	// StatsMaxLedgerRange caps the width of an explicit from_ledger/to_ledger
	// window on GET /v1/stats/contracts. Any explicit range bypasses the
	// maintained rollup and falls back to a live aggregation over
	// soroban_events (issue #654); without a cap that live scan grows
	// unbounded with total historical event count. ~7 days at Stellar's
	// ~5s ledger close time.
	StatsMaxLedgerRange = 120_000
)

// validNetworks holds the accepted values for the ?network filter.
var validNetworks = map[string]bool{
	"testnet": true,
	"mainnet": true,
}

// QueryStatsParams holds validated parameters for GET /v1/stats/contracts.
//
// Network is deliberately not populated by ValidateQueryStats (issue #612):
// unlike every other field here, it is never client-supplied. The caller
// must set it from middleware.NetworkFromContext after validation succeeds,
// matching how every other data endpoint enforces the key's network scope.
type QueryStatsParams struct {
	FromLedger    int64
	FromLedgerPtr *int64 // nil if not specified (for SQL NULL handling)
	ToLedger      int64
	ToLedgerPtr   *int64 // nil if not specified (for SQL NULL handling)
	Network       string
	Limit         int64
}

// ValidateQueryStats parses and validates query-string values for GET /v1/stats/contracts.
// It returns populated QueryStatsParams on success, or a *ValidationError on the
// first validation failure.
//
// Validation rules:
//   - from_ledger: non-negative integer if present; default 0 (all time)
//   - to_ledger:   non-negative integer if present; default latest indexed
//   - limit:       integer in [1, 100]; default 50
//
// network is not a parameter here: it is derived server-side from the
// authenticated key's context, not from the query string (issue #612).
func ValidateQueryStats(
	fromLedgerStr, toLedgerStr, limitStr string,
) (*QueryStatsParams, *ValidationError) {
	p := &QueryStatsParams{}

	from, to, verr := ValidateLedgerRange("from_ledger", "to_ledger", fromLedgerStr, toLedgerStr)
	if verr != nil {
		return nil, verr
	}
	p.FromLedgerPtr, p.ToLedgerPtr = from, to
	if from != nil {
		p.FromLedger = *from
	}
	if to != nil {
		p.ToLedger = *to
	}

	// An explicit range on either side bypasses the maintained rollup and
	// falls back to live aggregation (issue #654's queryContractStats), so a
	// one-sided or overly wide range must be rejected rather than left to
	// scan an unbounded slice of soroban_events. Only the fully-default,
	// unfiltered case (both nil) is exempt — that path is served entirely
	// from the rollup.
	if from != nil || to != nil {
		if from == nil || to == nil {
			return nil, Errorf("from_ledger", "from_ledger and to_ledger must both be set when either is provided")
		}
		if *to-*from > StatsMaxLedgerRange {
			return nil, Errorf("to_ledger", "range (to_ledger - from_ledger) must not exceed %d ledgers", StatsMaxLedgerRange)
		}
	}

	limit, verr := ValidateLimit("limit", limitStr, StatsLimitMin, StatsLimitMax, StatsLimitDefault)
	if verr != nil {
		return nil, verr
	}
	p.Limit = limit

	return p, nil
}
