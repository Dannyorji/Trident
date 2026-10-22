// Package sorobanrpc provides a minimal JSON-RPC 2.0 client for the Soroban
// RPC endpoint (STELLAR_RPC_URL). It is transport-only: callers supply the
// method name, params, and a destination for the decoded result.
package sorobanrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds a single RPC call. simulateTransaction runs the
// contract in a sandboxed host and can take noticeably longer than a simple
// query like getLatestLedger (stats.go uses 2s for that), so this client
// defaults to a more generous budget. Callers apply it via
// context.WithTimeout before calling Call.
const DefaultTimeout = 10 * time.Second

// Client calls a single Soroban RPC endpoint.
type Client struct {
	URL        string
	HTTPClient *http.Client
	// Headers are sent with every request. Mainnet providers that authenticate
	// with a header (Authorization: Bearer …, x-api-key: …) are unusable without
	// them (issue #735). Values are provider credentials: never log this map.
	Headers map[string]string
}

// NewClient builds a Client targeting url using http.DefaultClient, applying
// the provider auth headers in STELLAR_RPC_HEADERS (a JSON object of header
// name -> value). A malformed or unsafe spec is an error at startup rather
// than a silent unauthenticated client that fails every contract call.
func NewClient(url string) (*Client, error) {
	headers, err := HeadersFromEnv()
	if err != nil {
		return nil, err
	}
	return NewClientWithHeaders(url, headers), nil
}

// NewClientWithHeaders builds a Client with an explicit header set.
func NewClientWithHeaders(url string, headers map[string]string) *Client {
	return &Client{URL: url, HTTPClient: http.DefaultClient, Headers: headers}
}

// HeadersFromEnv parses STELLAR_RPC_HEADERS. An unset or empty variable means
// no headers. Error messages name the header but never echo its value.
func HeadersFromEnv() (map[string]string, error) {
	spec := strings.TrimSpace(os.Getenv("STELLAR_RPC_HEADERS"))
	if spec == "" {
		return nil, nil
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("soroban rpc: STELLAR_RPC_HEADERS must be a JSON object of header name -> value: %w", err)
	}

	headers := make(map[string]string, len(raw))
	for name, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("soroban rpc: STELLAR_RPC_HEADERS header %q must have a string value", name)
		}
		if err := validateHeader(name, text); err != nil {
			return nil, err
		}
		headers[name] = text
	}
	return headers, nil
}

// validateHeader enforces the RFC 7230 token grammar for names and rejects
// control characters in values (which would let a header value inject another).
func validateHeader(name, value string) error {
	if name == "" {
		return fmt.Errorf("soroban rpc: STELLAR_RPC_HEADERS contains an empty header name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		token := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
		if !token {
			return fmt.Errorf("soroban rpc: STELLAR_RPC_HEADERS header name %q is not a valid HTTP token", name)
		}
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == '\r' || c == '\n' || c == 0 || c == 0x7f {
			return fmt.Errorf("soroban rpc: STELLAR_RPC_HEADERS header %q contains a control character", name)
		}
	}
	return nil
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("soroban rpc error %d: %s", e.Code, e.Message)
}

type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *jsonRPCError   `json:"error"`
}

// Call issues a JSON-RPC 2.0 POST for method with params, decoding the
// "result" field into result. A JSON-RPC error envelope is surfaced as a Go
// error (of concrete type *jsonRPCError, unwrap with errors.As if the
// code/message need to be inspected).
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if c.URL == "" {
		return fmt.Errorf("soroban rpc: no endpoint configured")
	}

	reqBody, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("soroban rpc: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("soroban rpc: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for name, value := range c.Headers {
		// Set (not Add): a configured value replaces the default rather than
		// sending the header twice.
		httpReq.Header.Set(name, value)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("soroban rpc: call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A non-2xx response is an endpoint/auth failure, not a decode failure.
	// Surface it as such: an unauthenticated call (missing configured header)
	// must fail clearly instead of reporting `decode response: EOF` on the
	// provider's empty 401 body.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("soroban rpc: HTTP %d from endpoint", resp.StatusCode)
	}

	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("soroban rpc: decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	if result != nil && len(rpcResp.Result) > 0 {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("soroban rpc: decode result: %w", err)
		}
	}
	return nil
}
