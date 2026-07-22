// Package pgtest provisions throwaway PostgreSQL databases for tests. Each call
// to URL creates an isolated database and drops it during test cleanup, so
// Postgres-backed tests never share state. Tests are skipped unless
// TASK_TRACKER_TEST_DATABASE_URL points at a reachable server.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

var counter int64

// URL creates a fresh, empty database and returns a connection string for it.
// It skips the test when TASK_TRACKER_TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TASK_TRACKER_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TASK_TRACKER_TEST_DATABASE_URL to run Postgres-backed tests")
	}
	ctx := context.Background()
	name := fmt.Sprintf("tt_test_%d_%d", os.Getpid(), atomic.AddInt64(&counter, 1))

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database %s: %v", name, err)
	}
	admin.Close(ctx)

	t.Cleanup(func() {
		cleanup, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer cleanup.Close(ctx)
		_, _ = cleanup.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	return withDatabase(base, name)
}

func withDatabase(base, name string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}
	parsed.Path = "/" + name
	return parsed.String()
}
