package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/Depo-dev/trident/services/api/handlers"
)

// Issue #575: every list endpoint must serialise next_cursor identically.
//
// Two list endpoints used to disagree: /v1/admin/contracts tagged the field
// `omitempty`, so it vanished from the JSON on the last page, while
// /v1/api-keys and /v1/events emitted an explicit null. A strictly-typed SDK
// then had to model one field as optional-and-maybe-absent and the other as
// nullable-but-present — exactly the per-endpoint special-casing the uniform
// envelope (issue #220) set out to remove.
//
// Explicit null is the normative form (see api/openapi.yaml, "Pagination"):
// it distinguishes "no next page" from "this server version doesn't send the
// field", which an absent key cannot.

// marshalledKeys reports the top-level keys of v's JSON encoding, and whether
// next_cursor is present and null.
func marshalledCursor(t *testing.T, v any) (present bool, isNull bool, raw string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal %T into generic map: %v", v, err)
	}
	val, ok := generic["next_cursor"]
	if !ok {
		return false, false, string(b)
	}
	return true, string(val) == "null", string(b)
}

// TestNextCursor_ExplicitNullOnLastPage is the acceptance criterion: with no
// next page, every list response must carry next_cursor as an explicit null
// rather than dropping the key.
func TestNextCursor_ExplicitNullOnLastPage(t *testing.T) {
	cases := []struct {
		name string
		resp any
	}{
		{"ListContractsResponse", handlers.ListContractsResponse{
			Contracts:  []handlers.ContractResponse{},
			HasMore:    false,
			NextCursor: nil,
		}},
		{"ListAPIKeysResponse", handlers.ListAPIKeysResponse{
			APIKeys:    []handlers.APIKeyResponse{},
			HasMore:    false,
			NextCursor: nil,
		}},
		{"ListEventsResponse", handlers.ListEventsResponse{
			Events:     []*handlers.EventJSON{},
			HasMore:    false,
			NextCursor: nil,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, isNull, raw := marshalledCursor(t, tc.resp)
			if !present {
				t.Fatalf("next_cursor key is absent — it must serialise as an explicit null (drop `omitempty`). Got: %s", raw)
			}
			if !isNull {
				t.Fatalf("next_cursor must be null when there is no next page. Got: %s", raw)
			}
		})
	}
}

// TestNextCursor_PresentWhenSet guards the other half: a set cursor still
// serialises as its string value on every endpoint.
func TestNextCursor_PresentWhenSet(t *testing.T) {
	cursor := "b3BhcXVlLWN1cnNvcg"

	cases := []struct {
		name string
		resp any
	}{
		{"ListContractsResponse", handlers.ListContractsResponse{HasMore: true, NextCursor: &cursor}},
		{"ListAPIKeysResponse", handlers.ListAPIKeysResponse{HasMore: true, NextCursor: &cursor}},
		{"ListEventsResponse", handlers.ListEventsResponse{HasMore: true, NextCursor: &cursor}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, isNull, raw := marshalledCursor(t, tc.resp)
			if !present || isNull {
				t.Fatalf("next_cursor must carry the cursor string when has_more=true. Got: %s", raw)
			}
			if want := `"` + cursor + `"`; !jsonFieldEquals(raw, "next_cursor", want) {
				t.Fatalf("next_cursor = %s, want %s", raw, want)
			}
		})
	}
}

func jsonFieldEquals(raw, field, want string) bool {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return false
	}
	return string(generic[field]) == want
}
