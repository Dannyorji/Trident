package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/contracttest"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/getkin/kin-openapi/openapi3"
)

// Issue #513: the OpenAPI spec and the implemented routes must be the same
// set. A spec that drifts from the implementation is worse than no spec,
// because SDKs and users trust it — so this test fails when a route exists
// without a spec entry, or a spec entry without a route, in either direction.
//
// The route side comes from routeInventory() (routes.go), the same table
// main() registers from, so the comparison can never silently miss a route.
// Routes deliberately excluded from the public v1 surface carry an explicit
// exemption reason in the table; there is no third state.

// paramPattern collapses path-parameter names so /v1/events/{id} and
// /v1/events/{eventId} compare equal — the shape is the contract, the
// parameter name is documentation.
var paramPattern = regexp.MustCompile(`\{[^}]+\}`)

func normalizePath(p string) string {
	return paramPattern.ReplaceAllString(p, "{}")
}

func opKey(method, path string) string {
	return strings.ToUpper(method) + " " + normalizePath(path)
}

func TestEveryRouteIsDocumentedOrExempted(t *testing.T) {
	doc := contracttest.LoadSpec(t)

	specOps := make(map[string]bool)
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			specOps[opKey(method, path)] = true
		}
	}

	routeOps := make(map[string]bool)
	for _, route := range routeInventory() {
		if !route.Documented {
			if strings.TrimSpace(route.ExemptionReason) == "" {
				t.Errorf("route %s %s is undocumented with no exemption reason — document it in api/openapi.yaml or state why it is excluded",
					route.Method, route.Path)
			}
			continue
		}
		if route.Method == "" {
			t.Errorf("route %s is marked documented but has no method — OpenAPI operations are method-scoped", route.Path)
			continue
		}
		routeOps[opKey(route.Method, route.Path)] = true
	}

	var missingFromSpec, missingFromRouter []string
	for op := range routeOps {
		if !specOps[op] {
			missingFromSpec = append(missingFromSpec, op)
		}
	}
	for op := range specOps {
		if !routeOps[op] {
			missingFromRouter = append(missingFromRouter, op)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(missingFromRouter)

	for _, op := range missingFromSpec {
		t.Errorf("implemented route has no spec entry: %s — add it to api/openapi.yaml (then regenerate SDK models) or exempt it in routes.go with a reason", op)
	}
	for _, op := range missingFromRouter {
		t.Errorf("spec documents an operation no route implements: %s — remove it from api/openapi.yaml or mount the route", op)
	}
}

// Beyond paths: every documented operation must state its status codes — at
// least one success and at least one error — and every JSON error response
// must use the canonical ErrorResponse envelope. Covering "status codes and
// error envelopes, not just paths and happy-path shapes" is half of #513:
// an SDK generated from an operation with no error contract invents one.
func TestEveryOperationDocumentsStatusCodesAndErrorEnvelope(t *testing.T) {
	// Operations with no documented error response, each with the reason it
	// is acceptable. Kept deliberately tiny — new operations must document
	// their error contract.
	noErrorResponseAllowed := map[string]string{
		"GET /v1/health":  "liveness probe: unauthenticated, returns 200 by design; a failure is a transport error, not an API response",
		"GET /metrics":    "Prometheus exposition endpoint; scrapers treat any non-200 as scrape failure",
		"GET /v1/version": "static build metadata with no failure mode of its own",
	}

	// Error responses whose JSON body is deliberately NOT the canonical
	// envelope, each with the reason. Anything else gets flagged.
	nonEnvelopeErrorAllowed := map[string]string{
		"GET /v1/ready 503": "readiness failure returns the ReadyResponse check detail so probes can see WHICH dependency failed",
	}

	doc := contracttest.LoadSpec(t)

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			key := strings.ToUpper(method) + " " + path
			var hasSuccess, hasError bool
			for statusStr, ref := range op.Responses.Map() {
				if ref == nil || ref.Value == nil {
					continue
				}
				var status int
				if _, err := fmt.Sscanf(statusStr, "%d", &status); err != nil {
					continue
				}
				switch {
				case status >= 200 && status < 400:
					hasSuccess = true
				case status >= 400:
					hasError = true
					media := ref.Value.Content.Get("application/json")
					if media == nil {
						continue
					}
					if _, ok := nonEnvelopeErrorAllowed[key+" "+statusStr]; ok {
						continue
					}
					if media.Schema == nil || !strings.HasSuffix(media.Schema.Ref, "/ErrorResponse") {
						t.Errorf("%s: response %s has a JSON body that is not the canonical ErrorResponse envelope (ref %q)",
							key, statusStr, refOf(media.Schema))
					}
				}
			}
			if !hasSuccess {
				t.Errorf("%s: no success (2xx/3xx) response documented", key)
			}
			if !hasError {
				if _, ok := noErrorResponseAllowed[key]; !ok {
					t.Errorf("%s: no error (4xx/5xx) response documented — SDKs and users need the error contract, not just the happy path", key)
				}
			}
		}
	}
}

func refOf(s *openapi3.SchemaRef) string {
	if s == nil {
		return "<none>"
	}
	return s.Ref
}

// TestUsageEndpointsAreRouted guards against issue #615: KeyUsage and
// AdminKeyUsageRollup were fully implemented, documented in their own doc
// comments as live endpoints, and had their backing table (usage_rollup)
// actively maintained by RunUsageRollupLoop, but neither was ever registered
// in routeInventory() (routes.go) — a client hitting either documented path
// got a 404 from the mux, not from the handler.
func TestUsageEndpointsAreRouted(t *testing.T) {
	routes := routeInventory()

	want := []string{
		opKey("GET", "/v1/usage"),
		opKey("GET", "/v1/admin/keys/{id}/usage-rollup"),
	}

	for _, w := range want {
		found := false
		for _, r := range routes {
			if opKey(r.Method, r.Path) == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: not present in routeInventory() — implemented but never routed", w)
		}
	}
}

// TestErrorCodeEnumMatchesHttputil is the other half of issue #232's
// acceptance criteria beyond route parity: the spec's ErrorResponse.error.code
// enum must list exactly the same set of values as httputil.ErrorCode, in
// both directions — a code the spec omits is one an SDK's generated enum
// type can't represent, and a code the spec lists that Go never actually
// returns is a documentation lie an integrator could code a branch against
// that never fires.
func TestErrorCodeEnumMatchesHttputil(t *testing.T) {
	doc := contracttest.LoadSpec(t)

	schema := doc.Components.Schemas["ErrorResponse"]
	if schema == nil || schema.Value == nil {
		t.Fatal("api/openapi.yaml has no ErrorResponse schema")
	}
	codeProp, ok := schema.Value.Properties["error"]
	if !ok || codeProp.Value == nil {
		t.Fatal("ErrorResponse schema has no 'error' property")
	}
	codeSchema, ok := codeProp.Value.Properties["code"]
	if !ok || codeSchema.Value == nil {
		t.Fatal("ErrorResponse.error schema has no 'code' property")
	}

	specCodes := make(map[string]bool, len(codeSchema.Value.Enum))
	for _, v := range codeSchema.Value.Enum {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("ErrorResponse.error.code enum contains a non-string value: %#v", v)
		}
		specCodes[s] = true
	}
	if len(specCodes) == 0 {
		t.Fatal("ErrorResponse.error.code has no enum constraint — a bare 'type: string' lets an SDK generate an untyped field and lets any typo pass validation")
	}

	goCodes := map[string]bool{
		string(httputil.NOT_FOUND):         true,
		string(httputil.UNAUTHORIZED):      true,
		string(httputil.RATE_LIMITED):      true,
		string(httputil.INVALID_ARGUMENT):  true,
		string(httputil.UNAVAILABLE):       true,
		string(httputil.INTERNAL):          true,
		string(httputil.PAYLOAD_TOO_LARGE): true,
		string(httputil.FORBIDDEN):         true,
		string(httputil.CONFLICT):          true,
	}

	var missingFromSpec, missingFromGo []string
	for code := range goCodes {
		if !specCodes[code] {
			missingFromSpec = append(missingFromSpec, code)
		}
	}
	for code := range specCodes {
		if !goCodes[code] {
			missingFromGo = append(missingFromGo, code)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(missingFromGo)

	for _, code := range missingFromSpec {
		t.Errorf("httputil.ErrorCode %q is not in the spec's ErrorResponse.error.code enum", code)
	}
	for _, code := range missingFromGo {
		t.Errorf("spec's ErrorResponse.error.code enum lists %q, which is not a httputil.ErrorCode constant", code)
	}
}
