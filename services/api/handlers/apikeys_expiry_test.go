package handlers

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOptionalExpiry_Unmarshal(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name    string
		body    string
		wantSet bool
		wantNil bool
		wantErr bool
	}{
		{"absent", `{}`, false, true, false},
		{"null clears", `{"expires_at":null}`, true, true, false},
		{"future", `{"expires_at":"` + future + `"}`, true, false, false},
		{"past rejected", `{"expires_at":"` + past + `"}`, true, false, true},
		{"not a timestamp", `{"expires_at":"tomorrow"}`, true, false, true},
		{"not a string", `{"expires_at":12}`, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req struct {
				ExpiresAt optionalExpiry `json:"expires_at"`
			}
			err := json.Unmarshal([]byte(c.body), &req)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if req.ExpiresAt.Set != c.wantSet {
				t.Errorf("Set = %v, want %v", req.ExpiresAt.Set, c.wantSet)
			}
			if (req.ExpiresAt.Value == nil) != c.wantNil {
				t.Errorf("Value nil = %v, want %v", req.ExpiresAt.Value == nil, c.wantNil)
			}
		})
	}
}
