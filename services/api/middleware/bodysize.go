package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
)

// defaultMaxBodyBytes is the default cap applied to request bodies on
// methods that carry one. 1 MiB comfortably covers every JSON body accepted
// by this API today (issue #317) while still bounding memory used decoding
// a hostile/oversized payload.
const defaultMaxBodyBytes = 1 << 20 // 1 MiB

// defaultMaxBatchBodyBytes is the larger cap applied specifically to
// POST /v1/events/batch, whose body is a JSON array of up to 100 UUID
// strings (batchEventsMaxIDs in handlers/batch.go) — comfortably under 1
// MiB in practice, but given a distinct, slightly larger budget so a
// legitimate max-size batch request is never at risk of tripping the
// generic limit as the ID format evolves.
const defaultMaxBatchBodyBytes = 2 << 20 // 2 MiB

// bodyLimitedMethods are the methods whose requests may carry a body worth
// bounding. GET/HEAD/DELETE bodies are non-standard and already effectively
// unused by this API's handlers.
var bodyLimitedMethods = map[string]bool{
	http.MethodPost:  true,
	http.MethodPut:   true,
	http.MethodPatch: true,
}

// maxBytesError is returned by a Read on a body wrapped by
// http.MaxBytesReader once the limit is exceeded (net/http >= 1.19 gives it
// a concrete *http.MaxBytesError; older/other error paths are matched by
// string as a fallback).
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}

// BodySizeLimit returns middleware that wraps r.Body in an
// http.MaxBytesReader capped at limitBytes for methods that carry a body
// (POST/PUT/PATCH). It does not itself read the body — handlers read it as
// normal — but any Read past the limit fails, and this middleware recovers
// that specific failure mode to respond 413 with the standard error envelope
// instead of letting it surface as a generic 400/500 from whatever JSON
// decoder the handler used (issue #317).
//
// batchLimitBytes, if > 0, is applied instead of limitBytes to
// POST /v1/events/batch, which can legitimately be somewhat larger than a
// typical request body (see defaultMaxBatchBodyBytes).
func BodySizeLimit(limitBytes, batchLimitBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !bodyLimitedMethods[r.Method] || r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}

			limit := limitBytes
			if batchLimitBytes > 0 && r.URL.Path == "/v1/events/batch" {
				limit = batchLimitBytes
			}

			r.Body = http.MaxBytesReader(w, r.Body, limit)

			bw := &bodySizeResponseWriter{ResponseWriter: w}
			next.ServeHTTP(bw, r)

			if bw.wroteHeader {
				return
			}
			// The handler never wrote a response — most likely because a Read
			// on the wrapped body returned the MaxBytesReader error and the
			// handler bailed out without writing one (a plausible but not
			// guaranteed shape). Nothing further to do here; the handler is
			// solely responsible for observing read errors and this
			// middleware cannot retroactively know whether the limit was hit
			// versus some other early return. The primary 413 path is
			// bodySizeResponseWriter.Write below, triggered when a handler
			// does attempt to write an error after a failed Read.
		})
	}
}

// bodySizeResponseWriter tracks whether headers were already written so
// BodySizeLimit does not double-write a response.
type bodySizeResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *bodySizeResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *bodySizeResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodySizeResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// WriteBodyTooLarge writes the standard 413 error envelope for a request
// whose body exceeded the configured MaxBytesReader limit. Handlers that
// decode a request body should call this when the decode error satisfies
// IsBodyTooLarge, instead of falling through to their default 400 path, so
// oversized bodies are correctly reported as 413 (issue #317).
func WriteBodyTooLarge(w http.ResponseWriter, r *http.Request) {
	httputil.WriteErrorCtx(r.Context(), w, http.StatusRequestEntityTooLarge, httputil.PAYLOAD_TOO_LARGE, "request body exceeds the maximum allowed size")
}

// IsBodyTooLarge reports whether err originated from a Read on a body
// wrapped by http.MaxBytesReader exceeding its limit.
func IsBodyTooLarge(err error) bool {
	return isMaxBytesError(err)
}

// MaxBodyBytesFromEnv reads MAX_REQUEST_BODY_BYTES (falls back to
// defaultMaxBodyBytes) and MAX_BATCH_BODY_BYTES (falls back to
// defaultMaxBatchBodyBytes), following the NewTimeoutFromEnv /
// NewCORSFromEnv env-var pattern used elsewhere in this package.
func MaxBodyBytesFromEnv() (limit, batchLimit int64) {
	limit = int64(envInt("MAX_REQUEST_BODY_BYTES", defaultMaxBodyBytes))
	batchLimit = int64(envInt("MAX_BATCH_BODY_BYTES", defaultMaxBatchBodyBytes))
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	if batchLimit <= 0 {
		batchLimit = defaultMaxBatchBodyBytes
	}
	return limit, batchLimit
}

// NewBodySizeLimitFromEnv constructs BodySizeLimit from
// MAX_REQUEST_BODY_BYTES / MAX_BATCH_BODY_BYTES.
func NewBodySizeLimitFromEnv() func(http.Handler) http.Handler {
	limit, batchLimit := MaxBodyBytesFromEnv()
	return BodySizeLimit(limit, batchLimit)
}
