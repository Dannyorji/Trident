package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

type mockDB struct {
	pingErr     error
	queryRowErr error
	lastLedger  int64
}

func (m *mockDB) Ping(ctx context.Context) error {
	return m.pingErr
}

func (m *mockDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockRow{val: m.lastLedger, err: m.queryRowErr}
}

func (m *mockDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

type mockRow struct {
	val int64
	err error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(**int64); ok {
			*p = &r.val
		}
	}
	return nil
}

type mockRedis struct {
	pingErr error
}

func (m *mockRedis) Ping(ctx context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	if m.pingErr != nil {
		cmd.SetErr(m.pingErr)
	} else {
		cmd.SetVal("PONG")
	}
	return cmd
}

type mockGRPC struct {
	err error
}

func (m *mockGRPC) ListEvents(ctx context.Context, in *gen.ListEventsRequest, opts ...grpc.CallOption) (*gen.ListEventsResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &gen.ListEventsResponse{}, nil
}

func TestHealthAndReadinessProbes(t *testing.T) {
	t.Run("liveness probe succeeds without checking external dependencies (#443)", func(t *testing.T) {
		h := handlers.Health()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		rec := httptest.NewRecorder()

		h(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}

		var body map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "ok" {
			t.Fatalf("expected status ok, got %v", body["status"])
		}
	})

	t.Run("readiness probe fails when Postgres is down but liveness remains healthy (#443)", func(t *testing.T) {
		db := &mockDB{pingErr: errors.New("connection refused")}
		rdb := &mockRedis{}
		grpcClient := &mockGRPC{}

		readyHandler := handlers.Ready(db, rdb, grpcClient)
		req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
		rec := httptest.NewRecorder()

		readyHandler(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d", rec.Code)
		}

		var resp handlers.ReadyResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Status != "degraded" {
			t.Fatalf("expected degraded status, got %s", resp.Status)
		}
		if resp.Checks.Postgres == "ok" {
			t.Fatal("expected postgres check to report error")
		}
	})

	t.Run("readiness probe passes 200 when all dependencies are healthy (#443)", func(t *testing.T) {
		db := &mockDB{lastLedger: 500000}
		rdb := &mockRedis{}
		grpcClient := &mockGRPC{}

		readyHandler := handlers.Ready(db, rdb, grpcClient)
		req := httptest.NewRequest(http.MethodGet, "/v1/ready", nil)
		rec := httptest.NewRecorder()

		readyHandler(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
