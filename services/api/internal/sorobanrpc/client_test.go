package sorobanrpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #735: a provider that authenticates on a header must be usable when
// the key is configured, and must fail clearly (not silently) when it is not.
func TestClientSendsConfiguredHeaders(t *testing.T) {
	const apiKey = "provider-key"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"sequence": 42},
		})
	}))
	defer server.Close()

	configured := NewClientWithHeaders(server.URL, map[string]string{"x-api-key": apiKey})
	var result struct {
		Sequence uint64 `json:"sequence"`
	}
	if err := configured.Call(context.Background(), "getLatestLedger", nil, &result); err != nil {
		t.Fatalf("configured client must authenticate: %v", err)
	}
	if result.Sequence != 42 {
		t.Fatalf("sequence = %d, want 42", result.Sequence)
	}

	unconfigured := NewClientWithHeaders(server.URL, nil)
	err := unconfigured.Call(context.Background(), "getLatestLedger", nil, nil)
	if err == nil {
		t.Fatal("a missing header must fail, not silently succeed")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error should carry the HTTP status: %v", err)
	}
}

func TestHeadersFromEnvParsesJSONObject(t *testing.T) {
	t.Setenv("STELLAR_RPC_HEADERS", `{"Authorization":"Bearer token","x-api-key":"abc"}`)

	headers, err := HeadersFromEnv()
	if err != nil {
		t.Fatalf("HeadersFromEnv: %v", err)
	}
	if headers["Authorization"] != "Bearer token" || headers["x-api-key"] != "abc" {
		t.Fatalf("unexpected headers: %v", headers)
	}
}

func TestHeadersFromEnvEmptyMeansNone(t *testing.T) {
	t.Setenv("STELLAR_RPC_HEADERS", "  ")

	headers, err := HeadersFromEnv()
	if err != nil {
		t.Fatalf("HeadersFromEnv: %v", err)
	}
	if headers != nil {
		t.Fatalf("expected no headers, got %v", headers)
	}
}

func TestHeadersFromEnvRejectsInvalidSpecWithoutLeakingValues(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"not json", `x-api-key=abc`},
		{"not an object", `["abc"]`},
		{"non-string value", `{"x-api-key":42}`},
		{"invalid name", `{"bad name":"leak-me"}`},
		{"control character", `{"x-api-key":"leak-me\n"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STELLAR_RPC_HEADERS", tc.spec)

			_, err := HeadersFromEnv()
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "leak-me") {
				t.Fatalf("error must not echo the header value: %v", err)
			}
		})
	}
}
