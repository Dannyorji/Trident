// Package contracttest validates live HTTP responses against
// api/openapi.yaml (issue #242), so a header or body shape drifting from
// the documented contract fails a test instead of surfacing only in
// production against real clients.
package contracttest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

var registerSSEDecoder sync.Once

// specPath resolves api/openapi.yaml relative to this source file (not the
// test's working directory), so callers in any package under services/api
// find the same spec regardless of `go test`'s per-package cwd.
func specPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "api", "openapi.yaml")
}

// LoadSpec loads and validates api/openapi.yaml. Fails the test immediately
// on a malformed spec, since every contract test depends on it.
func LoadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	registerSSEDecoder.Do(func() {
		openapi3filter.RegisterBodyDecoder("text/event-stream", func(body io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
			data, err := io.ReadAll(body)
			return string(data), err
		})
	})
	loader := &openapi3.Loader{IsExternalRefsAllowed: false}
	doc, err := loader.LoadFromFile(specPath())
	if err != nil {
		t.Fatalf("contracttest: load api/openapi.yaml: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("contracttest: api/openapi.yaml failed its own validation: %v", err)
	}
	return doc
}

// NewRouter builds a router used to resolve an *http.Request to the
// operation (and its documented responses) it matches in doc.
func NewRouter(t *testing.T, doc *openapi3.T) routers.Router {
	t.Helper()
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("contracttest: build router: %v", err)
	}
	return router
}

// ValidateResponse asserts that status/header/body for req's matched
// operation conform to what api/openapi.yaml documents — the response code
// is a documented one, every documented header for that response is
// present and matches its schema, and the body matches the documented
// content schema. Fails the test (via t.Error, not Fatal, so multiple
// contract violations in a suite are all reported) on any mismatch.
func ValidateResponse(t *testing.T, router routers.Router, req *http.Request, status int, header http.Header, body []byte) {
	t.Helper()

	route, pathParams, err := router.FindRoute(req)
	if err != nil {
		t.Errorf("contracttest: %s %s does not match any documented route: %v", req.Method, req.URL.Path, err)
		return
	}

	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
	}

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 status,
		Header:                 header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
	}

	if err := openapi3filter.ValidateResponse(context.Background(), respInput); err != nil {
		t.Errorf("contracttest: %s %s -> %d response does not conform to api/openapi.yaml: %v", req.Method, req.URL.Path, status, err)
	}
}
