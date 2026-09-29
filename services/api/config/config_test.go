package config_test

import (
	"os"
	"testing"

	"github.com/Depo-dev/trident/services/api/config"
)

func setEnv(t *testing.T, pairs map[string]string) {
	t.Helper()
	for k, v := range pairs {
		t.Setenv(k, v)
	}
}

func allRequired(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL":  "postgres://localhost/test",
		"REDIS_URL":     "redis://localhost:6379",
		"API_GRPC_ADDR": "localhost:50051",
	})
}

// loadSafe wraps config.Load so tests can capture the os.Exit call.
// Since Load calls os.Exit on failure, we test individual collection
// logic via collect helpers instead.
func TestLoad_AllVarsPresent(t *testing.T) {
	allRequired(t)
	cfg := config.Load()
	if cfg.DatabaseURL != "postgres://localhost/test" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.RedisURL != "redis://localhost:6379" {
		t.Errorf("RedisURL = %q", cfg.RedisURL)
	}
	if cfg.APIGrpcAddr != "localhost:50051" {
		t.Errorf("APIGrpcAddr = %q", cfg.APIGrpcAddr)
	}
}

// TestCollectMissing verifies the missing-var collection logic via the
// exported CollectMissing helper (see config.go).
func TestMissingVarsIdentified(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("REDIS_URL")
	_ = os.Unsetenv("API_GRPC_ADDR")

	missing := config.CollectMissing()
	if len(missing) != 3 {
		t.Fatalf("expected 3 missing vars, got %d: %v", len(missing), missing)
	}
	mustContain := []string{"DATABASE_URL", "REDIS_URL", "API_GRPC_ADDR"}
	for _, want := range mustContain {
		found := false
		for _, got := range missing {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing list does not contain %q: %v", want, missing)
		}
	}
}

func TestMissingVars_OnlyDatabaseURL(t *testing.T) {
	_ = os.Unsetenv("DATABASE_URL")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	t.Setenv("API_GRPC_ADDR", "localhost:50051")

	missing := config.CollectMissing()
	if len(missing) != 1 || missing[0] != "DATABASE_URL" {
		t.Errorf("expected [DATABASE_URL], got %v", missing)
	}
}

func TestMissingVars_OnlyRedisURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	_ = os.Unsetenv("REDIS_URL")
	t.Setenv("API_GRPC_ADDR", "localhost:50051")

	missing := config.CollectMissing()
	if len(missing) != 1 || missing[0] != "REDIS_URL" {
		t.Errorf("expected [REDIS_URL], got %v", missing)
	}
}

func TestMissingVars_OnlyAPIGrpcAddr(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	_ = os.Unsetenv("API_GRPC_ADDR")

	missing := config.CollectMissing()
	if len(missing) != 1 || missing[0] != "API_GRPC_ADDR" {
		t.Errorf("expected [API_GRPC_ADDR], got %v", missing)
	}
}
