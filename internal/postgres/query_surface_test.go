package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/tasks/internal/pgtest"
	"github.com/zachlatta/tasks/internal/task"
)

// Agents repeatedly guessed a dependencies column on task_overview and then
// fell back to joining the dependencies table by hand. The view exposes the
// prerequisite IDs directly so one read answers "what does this task wait on".
func TestTaskOverviewListsDependencyIDs(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, mkTask("first", now))
	mustCreate(t, store, mkTask("second", now))
	dependent := mkTask("dependent", now)
	dependent.Dependencies = []string{"second", "first"}
	mustCreate(t, store, dependent)

	result, err := store.Query(ctx, "SELECT id, depends_on FROM task_overview ORDER BY id")
	if err != nil {
		t.Fatalf("query task_overview: %v", err)
	}
	got := make(map[string]string, len(result.Rows))
	for _, row := range result.Rows {
		got[fmt.Sprint(row["id"])] = fmt.Sprint(row["depends_on"])
	}
	if got["dependent"] != "[first second]" {
		t.Errorf("dependent depends_on = %s, want [first second]", got["dependent"])
	}
	if got["first"] != "[]" {
		t.Errorf("first depends_on = %s, want []", got["first"])
	}
}

// A wrong column or table guess should come back with enough schema guidance
// to fix the query in one step instead of a discovery round-trip.
func TestQueryUndefinedNamesIncludeSchemaHint(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	_, err := store.Query(ctx, "SELECT id FROM task_overview WHERE deleted_at IS NULL")
	if err == nil {
		t.Fatal("undefined column accepted, want error")
	}
	for _, want := range []string{"does not exist", "information_schema.columns", "task_overview lists live tasks only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("undefined column error %q does not mention %q", err, want)
		}
	}

	_, err = store.Query(ctx, "SELECT * FROM task_list")
	if err == nil {
		t.Fatal("undefined table accepted, want error")
	}
	if !strings.Contains(err.Error(), "information_schema.columns") {
		t.Errorf("undefined table error %q does not mention information_schema.columns", err)
	}

	// Errors without a schema cause stay unadorned.
	_, err = store.Query(ctx, "DELETE FROM tasks")
	if err == nil {
		t.Fatal("DELETE accepted, want rejection")
	}
	if strings.Contains(err.Error(), "information_schema.columns") {
		t.Errorf("rejection %q should not carry the schema hint", err)
	}
}

// The agent SQL surface is for task data. The OAuth and browser-session tables
// live in the same database but must stay out of reach, so a prompted or
// prompt-injected agent cannot enumerate clients, token hashes, or CSRF
// values.
func TestQueryCannotReadAuthTables(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if err := store.ReaderRoleError(); err != nil {
		t.Fatalf("reader role not active: %v", err)
	}

	for _, table := range []string{"oauth_clients", "oauth_codes", "oauth_tokens", "oauth_refresh_tokens", "web_sessions"} {
		_, err := store.Query(ctx, "SELECT count(*) FROM "+table)
		if err == nil {
			t.Errorf("agent SQL read %s, want permission denied", table)
			continue
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("reading %s failed with %q, want permission denied", table, err)
		}
	}

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, mkTask("readable", now))
	for _, statement := range []string{
		"SELECT id FROM tasks",
		"SELECT id, depends_on FROM task_overview",
		"SELECT task_id FROM dependencies",
		"SELECT task_id FROM images",
		"SELECT task_id, action FROM task_revisions",
		"EXPLAIN SELECT id FROM tasks",
		"SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' GROUP BY 1",
	} {
		if _, err := store.Query(ctx, statement); err != nil {
			t.Errorf("task read %q failed: %v", statement, err)
		}
	}

	// With the reader role active, schema discovery no longer surfaces the
	// auth tables at all.
	listing, err := store.Query(ctx, "SELECT DISTINCT table_name FROM information_schema.columns WHERE table_schema = 'public'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for _, row := range listing.Rows {
		if strings.HasPrefix(fmt.Sprint(row["table_name"]), "oauth_") {
			t.Errorf("information_schema still lists %v", row["table_name"])
		}
	}
}

// Reopening the same database must keep the reader role working: the view is
// recreated on every open, so its grant has to be re-issued.
func TestReaderRoleSurvivesReopen(t *testing.T) {
	url := pgtest.URL(t)
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()

	reopened, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if err := reopened.ReaderRoleError(); err != nil {
		t.Fatalf("reader role not active after reopen: %v", err)
	}
	if _, err := reopened.Query(context.Background(), "SELECT id, depends_on FROM task_overview"); err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
}

func mkTask(id string, now time.Time) task.Task {
	return task.Task{ID: id, Title: strings.ToUpper(id[:1]) + id[1:], Status: task.StatusTodo, CreatedAt: now, UpdatedAt: now}
}
