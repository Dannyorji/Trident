package ws

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Schema snapshot tests (issue #223).
//
// The snapshot lives in testdata/graphql_schema.graphql. A change to the
// published GraphQL surface has to update it deliberately — which is the
// point: the SDKs generate against this schema, so a field renamed or
// dropped by accident is a client break, and the diff in a review is where
// that gets caught.
//
// Regenerate with: go test ./ws/ -run TestGraphQLSchemaSnapshot -update
//
// A test flag rather than an env var: scripts/check-env-reference.sh scans
// services/api for os.Getenv and requires a docs/ENVIRONMENT.md row for
// everything it finds, and a snapshot-regeneration knob is not a deployment
// setting that belongs in that table. -update is also the standard Go
// convention for golden files.

var updateSnapshot = flag.Bool("update", false,
	"rewrite testdata/graphql_schema.graphql from the current GraphQLSchema")

const schemaSnapshotPath = "testdata/graphql_schema.graphql"

func TestGraphQLSchemaSnapshot(t *testing.T) {
	if *updateSnapshot {
		if err := os.MkdirAll(filepath.Dir(schemaSnapshotPath), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(schemaSnapshotPath, []byte(GraphQLSchema), 0o644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		t.Logf("snapshot written to %s", schemaSnapshotPath)
		return
	}

	want, err := os.ReadFile(schemaSnapshotPath)
	if err != nil {
		t.Fatalf("read snapshot (regenerate with `go test ./ws/ -run TestGraphQLSchemaSnapshot -update`): %v", err)
	}

	if normaliseSchema(string(want)) != normaliseSchema(GraphQLSchema) {
		t.Errorf("the published GraphQL schema changed but %s was not updated.\n"+
			"If the change is intended, regenerate with:\n"+
			"    go test ./ws/ -run TestGraphQLSchemaSnapshot -update\n"+
			"and review the diff — the SDKs generate against this schema, so a\n"+
			"renamed or removed field is a client break.", schemaSnapshotPath)
	}
}

// normaliseSchema ignores trailing whitespace and line-ending differences, so
// the snapshot does not fail spuriously on a Windows checkout.
func normaliseSchema(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// Schema / resolver agreement
// ---------------------------------------------------------------------------

var reRootField = regexp.MustCompile(`(?m)^\s{2}([a-zA-Z][a-zA-Z0-9_]*)\s*[(:]`)

// schemaRootFields extracts the root field names declared under one type in
// the SDL.
func schemaRootFields(t *testing.T, typeName string) []string {
	t.Helper()
	start := strings.Index(GraphQLSchema, "type "+typeName+" {")
	if start < 0 {
		t.Fatalf("schema has no type %s", typeName)
	}
	body := GraphQLSchema[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}

	var fields []string
	for _, m := range reRootField.FindAllStringSubmatch(body, -1) {
		fields = append(fields, m[1])
	}
	return fields
}

// TestSchemaMatchesDispatchedOperations is the drift guard the snapshot alone
// cannot give: it checks the schema and the resolver agree about which
// operations exist, in both directions.
func TestSchemaMatchesDispatchedOperations(t *testing.T) {
	t.Run("Query", func(t *testing.T) {
		assertSameSet(t, schemaRootFields(t, "Query"), gqlSupportedQueries, "Query")
	})
	t.Run("Subscription", func(t *testing.T) {
		assertSameSet(t, schemaRootFields(t, "Subscription"), gqlSupportedSubscriptions, "Subscription")
	})
}

func assertSameSet(t *testing.T, schema, dispatched []string, label string) {
	t.Helper()
	inSchema := map[string]bool{}
	for _, f := range schema {
		inSchema[f] = true
	}
	inDispatch := map[string]bool{}
	for _, f := range dispatched {
		inDispatch[f] = true
	}

	for f := range inSchema {
		if !inDispatch[f] {
			t.Errorf("%s.%s is declared in the schema but no resolver dispatches it — "+
				"a client can request it and get an unsupported-operation error", label, f)
		}
	}
	for f := range inDispatch {
		if !inSchema[f] {
			t.Errorf("%s.%s is dispatched by the resolver but missing from the schema — "+
				"the SDKs generate from the schema, so it would be unreachable", label, f)
		}
	}
}

// TestEveryDispatchedQueryResolves closes the loop on the schema/resolver
// agreement: each operation the schema advertises must actually be dispatched
// rather than falling through to "unsupported operation".
func TestEveryDispatchedQueryResolves(t *testing.T) {
	backend := &fakeBackend{
		event: map[string]any{"id": "e1"},
		stats: []map[string]any{{"contract_id": "C1"}},
	}

	// Minimal valid documents for each supported query, with whatever
	// required arguments the operation needs.
	docs := map[string]string{
		"events":        `query { events { events { id } } }`,
		"event":         `query { event(id: "11111111-1111-4111-8111-111111111111") { id } }`,
		"contractStats": `query { contractStats { contractId } }`,
	}

	for _, name := range gqlSupportedQueries {
		t.Run(name, func(t *testing.T) {
			doc, ok := docs[name]
			if !ok {
				t.Fatalf("no probe document for dispatched operation %q — add one so this "+
					"test keeps covering every supported operation", name)
			}
			op, err := gqlParseQuery(payload(t, doc, nil))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if op.name != name {
				t.Fatalf("probe document parsed as operation %q, want %q", op.name, name)
			}
			data, err := gqlResolveQuery(context.Background(), backend, op, "testnet")
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			if _, ok := data[name]; !ok {
				t.Errorf("resolved data has no %q field: %#v", name, data)
			}
		})
	}
}
