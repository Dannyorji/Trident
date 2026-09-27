package validation

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
)

const validContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func TestValidateContractID(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty is optional", "", false},
		{"valid strkey", validContractID, false},
		{"lowercase rejected", strings.ToLower(validContractID), true},
		{"wrong prefix", "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ", true},
		{"too short", "CA7QYNF7", true},
		{"invalid base32 char", "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSG1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verr := ValidateContractID("contractId", tt.value)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr != nil && !strings.Contains(verr.Message, "contractId") {
				t.Errorf("message must name the field, got %q", verr.Message)
			}
		})
	}
}

func TestValidateRequiredContractID(t *testing.T) {
	if verr := ValidateRequiredContractID("contractId", ""); verr == nil {
		t.Fatal("empty contract id must be rejected when required")
	}
	if verr := ValidateRequiredContractID("contractId", validContractID); verr != nil {
		t.Fatalf("valid contract id rejected: %v", verr)
	}
}

func TestValidateUUID(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid v4", "550e8400-e29b-41d4-a716-446655440000", false},
		{"uppercase accepted", "550E8400-E29B-41D4-A716-446655440000", false},
		{"empty rejected", "", true},
		{"not a uuid", "not-a-uuid", true},
		{"v1 uuid rejected", "550e8400-e29b-11d4-a716-446655440000", true},
		{"missing dashes", "550e8400e29b41d4a716446655440000", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verr := ValidateUUID("id", tt.value)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got %v, wantErr=%v", verr, tt.wantErr)
			}
		})
	}
}

func TestValidateLedgerRange(t *testing.T) {
	tests := []struct {
		name      string
		from, to  string
		wantErr   bool
		wantField string
	}{
		{name: "both absent"},
		{name: "from only", from: "10"},
		{name: "to only", to: "10"},
		{name: "equal bounds", from: "10", to: "10"},
		{name: "ascending", from: "10", to: "20"},
		{name: "inverted", from: "20", to: "10", wantErr: true, wantField: "to_ledger"},
		{name: "negative from", from: "-1", wantErr: true, wantField: "from_ledger"},
		{name: "non numeric to", to: "abc", wantErr: true, wantField: "to_ledger"},
		{name: "float rejected", from: "1.5", wantErr: true, wantField: "from_ledger"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, verr := ValidateLedgerRange("from_ledger", "to_ledger", tt.from, tt.to)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr != nil && verr.Field != tt.wantField {
				t.Errorf("field: got %q, want %q", verr.Field, tt.wantField)
			}
		})
	}
}

func TestValidateLimit(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{"absent uses default", "", 50, false},
		{"minimum accepted", "1", 1, false},
		{"maximum accepted", "200", 200, false},
		{"below minimum", "0", 0, true},
		{"above maximum", "201", 0, true},
		{"negative", "-5", 0, true},
		{"non numeric", "ten", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, verr := ValidateLimit("limit", tt.value, 1, 200, 50)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got err %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr == nil && got != tt.want {
				t.Errorf("limit: got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateCursor(t *testing.T) {
	encoded := cursor.Encode("paging-token-42")

	tests := []struct {
		name      string
		value     string
		wantToken string
		wantErr   bool
	}{
		{name: "absent", value: ""},
		{name: "valid", value: encoded, wantToken: "paging-token-42"},
		{name: "not base64", value: "!!!not-base64!!!", wantErr: true},
		{name: "base64 but not a cursor", value: "aGVsbG8", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, verr := ValidateCursor("cursor", tt.value)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got err %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr == nil && token != tt.wantToken {
				t.Errorf("token: got %q, want %q", token, tt.wantToken)
			}
		})
	}
}

func TestValidateNetwork(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{"absent uses default", "", "testnet", false},
		{"testnet", "testnet", "testnet", false},
		{"mainnet", "mainnet", "mainnet", false},
		{"case insensitive", "MAINNET", "mainnet", false},
		{"unknown network", "futurenet", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, verr := ValidateNetwork("network", tt.value, "testnet")
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got err %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr == nil && got != tt.want {
				t.Errorf("network: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateEventType(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{"absent means no filter", "", "", false},
		{"contract", "contract", "contract", false},
		{"system", "system", "system", false},
		{"diagnostic", "DIAGNOSTIC", "diagnostic", false},
		{"unknown", "transfer", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, verr := ValidateEventType("event_type", tt.value)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got err %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr == nil && got != tt.want {
				t.Errorf("event_type: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateTimeRange(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		wantErr  bool
	}{
		{name: "valid window", from: "2024-01-01T00:00:00Z", to: "2024-01-02T00:00:00Z"},
		{name: "equal bounds", from: "2024-01-01T00:00:00Z", to: "2024-01-01T00:00:00Z"},
		{name: "inverted", from: "2024-01-02T00:00:00Z", to: "2024-01-01T00:00:00Z", wantErr: true},
		{name: "missing from", to: "2024-01-01T00:00:00Z", wantErr: true},
		{name: "missing to", from: "2024-01-01T00:00:00Z", wantErr: true},
		{name: "not rfc3339", from: "01/02/2024", to: "2024-01-01T00:00:00Z", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, verr := ValidateTimeRange("from", "to", tt.from, tt.to, 31*24*time.Hour)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got %v, wantErr=%v", verr, tt.wantErr)
			}
		})
	}

	const from, wide = "2024-01-01T00:00:00Z", "2024-06-01T00:00:00Z"

	t.Run("exceeds max duration", func(t *testing.T) {
		if _, _, verr := ValidateTimeRange("from", "to", from, wide, 31*24*time.Hour); verr == nil {
			t.Fatal("expected a window wider than the cap to be rejected")
		}
	})

	t.Run("zero max duration means uncapped", func(t *testing.T) {
		if _, _, verr := ValidateTimeRange("from", "to", from, wide, 0); verr != nil {
			t.Fatalf("got %v, want no error when the cap is disabled", verr)
		}
	})
}

func TestRejectUnknownParams(t *testing.T) {
	allowed := []string{"limit", "cursor", "contractId"}

	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"empty query", "", false},
		{"all known", "limit=10&cursor=abc", false},
		{"single unknown", "limitt=10", true},
		{"known plus unknown", "limit=10&colour=red", true},
		{"unknown with empty value", "typo=", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			verr := RejectUnknownParams(q, allowed...)
			if (verr != nil) != tt.wantErr {
				t.Fatalf("got %v, wantErr=%v", verr, tt.wantErr)
			}
			if verr != nil && !strings.Contains(verr.Message, "supported:") {
				t.Errorf("message should list supported params, got %q", verr.Message)
			}
		})
	}
}

// Every validator must name the offending field in the message so an SDK can
// surface it without parsing prose (issue #222).
func TestMessagesNameTheField(t *testing.T) {
	cases := []*ValidationError{
		ValidateContractID("contractId", "nope"),
		ValidateUUID("id", "nope"),
		mustErr(func() *ValidationError { _, verr := ValidateLimit("limit", "0", 1, 10, 5); return verr }),
		mustErr(func() *ValidationError { _, verr := ValidateCursor("cursor", "!!!"); return verr }),
		mustErr(func() *ValidationError { _, verr := ValidateNetwork("network", "x", "testnet"); return verr }),
		mustErr(func() *ValidationError { _, verr := ValidateEventType("event_type", "x"); return verr }),
	}

	for _, verr := range cases {
		if verr == nil {
			t.Fatal("expected a validation error")
		}
		if !strings.Contains(verr.Message, verr.Field) {
			t.Errorf("message %q should contain field %q", verr.Message, verr.Field)
		}
	}
}

func mustErr(f func() *ValidationError) *ValidationError {
	return f()
}
