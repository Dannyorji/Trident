package handlers

import (
	"testing"

	"github.com/Depo-dev/trident/services/api/cursor"
)

// TestStatsKeysetRoundTrip verifies a stats cursor survives encode -> decode
// with both components intact. The contract_id half is what makes the keyset
// unique; dropping it would silently skip contracts that tie on event_count.
func TestStatsKeysetRoundTrip(t *testing.T) {
	cases := []statsKeyset{
		{EventCount: 0, ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM"},
		{EventCount: 9223372036854775807, ContractID: "CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBD2KM"},
		{EventCount: 42, ContractID: "C:WITH:COLONS"},
	}

	for _, want := range cases {
		token, err := cursor.Decode(encodeStatsCursor(want))
		if err != nil {
			t.Fatalf("cursor.Decode(%v): %v", want, err)
		}
		got, err := decodeStatsKeyset(token)
		if err != nil {
			t.Fatalf("decodeStatsKeyset(%q): %v", token, err)
		}
		if got.EventCount != want.EventCount || got.ContractID != want.ContractID {
			t.Errorf("round trip: want %+v, got %+v", want, *got)
		}
	}
}

// TestDecodeStatsKeysetRejectsMalformed verifies a malformed cursor is an
// error rather than a silent reset to page one, which would make a paging
// client loop over the first page forever.
func TestDecodeStatsKeysetRejectsMalformed(t *testing.T) {
	for _, tok := range []string{"", "123", "notanumber:CABC", ":CABC", "123:"} {
		if _, err := decodeStatsKeyset(tok); err == nil {
			t.Errorf("decodeStatsKeyset(%q): want error, got nil", tok)
		}
	}
}
