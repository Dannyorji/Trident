package middleware

import "context"

type contextKey string

const (
	contextKeyAPIKeyID contextKey = "api_key_id"
	contextKeyNetwork  contextKey = "network"
)

// LegacyEnvKeyID is the sentinel APIKeyIDFromContext value attached to a
// request authenticated via the legacy API_KEY_HASHES env-var path (issue
// #616). It is deliberately not a UUID: legacy env-var keys have no row in
// api_keys, so there is nothing for a real key id to reference. Callers that
// key behaviour off "is this a DB-backed key" (audit_log.api_key_id,
// per-key usage rollups) must check for this sentinel before treating
// APIKeyIDFromContext's return value as a parseable UUID.
const LegacyEnvKeyID = "legacy-env-key"

// LegacyEnvNetwork is the network attached to a request authenticated via
// the legacy env-var path. Legacy keys carry no network of their own, so
// this makes the fallback an explicit, named decision on the auth path
// instead of NetworkFromContext's implicit default kicking in silently
// several layers downstream (issue #616).
const LegacyEnvNetwork = "testnet"

// APIKeyIDFromContext returns the authenticated API key UUID, or empty string
// when the request was authenticated via the legacy env-var path.
func APIKeyIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(contextKeyAPIKeyID).(string)
	return v
}

// WithAPIKeyID attaches an authenticated API key id to ctx for
// APIKeyIDFromContext to retrieve downstream. Set by NewDBAuth on successful
// DB-backed authentication; exported so tests can simulate an authenticated
// request without going through the full auth middleware.
func WithAPIKeyID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKeyAPIKeyID, id)
}

// NetworkFromContext returns the network associated with the authenticated API
// key. Defaults to "testnet" when the request is unauthenticated or the key
// has no network set.
func NetworkFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKeyNetwork).(string); ok && v != "" {
		return v
	}
	return "testnet"
}

// WithNetwork attaches an authenticated key's network to ctx for
// NetworkFromContext to retrieve downstream. The counterpart to
// WithAPIKeyID, exported for the same reason: so a test can simulate an
// authenticated request on a given network without driving the whole auth
// middleware.
func WithNetwork(ctx context.Context, network string) context.Context {
	return context.WithValue(ctx, contextKeyNetwork, network)
}

// requestLogState is a mutable holder for request facts that are only known
// deep inside the middleware chain but must appear on StructuredLogging's
// end-of-request log line (issue #239).
//
// A plain context.WithValue cannot carry this: StructuredLogging is
// deliberately the outermost middleware (see main.go) so it logs every
// response, including ones NewDBAuth rejects before authenticating — but
// that same positioning means NewDBAuth's `ctx = WithAPIKeyID(...)` runs on
// a context descended from StructuredLogging's, not an ancestor of it, so it
// can never flow back up to the *http.Request StructuredLogging still holds
// when it builds the log line. Installing one mutable pointer up front and
// having downstream code write through it sidesteps that: a context lookup
// walks up the parent chain no matter how many further WithValue/WithContext
// calls happened in between, so every descendant resolves the same pointer.
type requestLogState struct {
	apiKeyID string
}

type logStateKey struct{}

// withLogState installs a fresh requestLogState into ctx, returning both the
// new context and the holder for the installing middleware to read later.
func withLogState(ctx context.Context) (context.Context, *requestLogState) {
	st := &requestLogState{}
	return context.WithValue(ctx, logStateKey{}, st), st
}

// SetLogAPIKeyID records the authenticated API key id for the current
// request's structured log line. Called by NewDBAuth once authentication
// succeeds; a no-op if StructuredLogging did not install a log-state holder
// on this request (e.g. in a test that exercises a handler directly).
func SetLogAPIKeyID(ctx context.Context, apiKeyID string) {
	if st, ok := ctx.Value(logStateKey{}).(*requestLogState); ok {
		st.apiKeyID = apiKeyID
	}
}
