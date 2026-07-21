package query

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zachlatta/task-tracker/internal/task"
)

func TestReadModelSyncAndQuery(t *testing.T) {
	t.Parallel()

	model, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { model.Close() })
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	items := []task.Task{
		{ID: "write-tests", Title: "Write tests", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now},
		{
			ID:           "ship-feature",
			Title:        "Ship feature",
			Description:  "Release it",
			Status:       task.StatusTodo,
			Dependencies: []string{"write-tests"},
			Attachments:  []task.Attachment{{Key: "ship-feature/diagram.png", Name: "diagram.png", ContentType: "image/png"}},
			CreatedAt:    now,
			UpdatedAt:    now,
		},
	}
	if err := model.Sync(context.Background(), items); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	result, err := model.Query(context.Background(), `
		SELECT t.id, t.status, d.depends_on_id, i.name AS image
		FROM tasks t
		LEFT JOIN dependencies d ON d.task_id = t.id
		LEFT JOIN images i ON i.task_id = t.id
		WHERE t.id = 'ship-feature'
	`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(result.Rows))
	}
	row := result.Rows[0]
	if row["depends_on_id"] != "write-tests" || row["image"] != "diagram.png" {
		t.Fatalf("row = %#v", row)
	}
	overview, err := model.Query(context.Background(), "SELECT blocked, dependency_count, image_count FROM task_overview WHERE id = 'ship-feature'")
	if err != nil {
		t.Fatalf("query task_overview: %v", err)
	}
	if got := overview.Rows[0]; got["blocked"] != int64(0) || got["dependency_count"] != int64(1) || got["image_count"] != int64(1) {
		t.Fatalf("task_overview row = %#v", got)
	}
}

func TestReadModelHasIntuitiveSchema(t *testing.T) {
	t.Parallel()

	model, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { model.Close() })
	result, err := model.Query(context.Background(), `
		SELECT name FROM sqlite_schema
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("Query schema: %v", err)
	}
	want := []string{"dependencies", "images", "task_overview", "tasks"}
	got := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		got = append(got, row["name"].(string))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("schema objects = %v, want %v", got, want)
	}
}

func TestReadModelRejectsWrites(t *testing.T) {
	t.Parallel()

	model, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { model.Close() })
	if err := model.Sync(context.Background(), []task.Task{{ID: "safe", Title: "Safe", Status: task.StatusTodo}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	for _, statement := range []string{
		"DELETE FROM tasks",
		"WITH changed AS (DELETE FROM tasks RETURNING *) SELECT * FROM changed",
		"PRAGMA user_version = 2",
	} {
		if _, err := model.Query(context.Background(), statement); err == nil {
			t.Fatalf("Query(%q) succeeded, want read-only error", statement)
		}
	}
	result, err := model.Query(context.Background(), "SELECT count(*) AS count FROM tasks")
	if err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["count"] != int64(1) {
		t.Fatalf("rows = %#v, want one preserved task", result.Rows)
	}
}

func TestReadModelLimitsRows(t *testing.T) {
	t.Parallel()

	model, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { model.Close() })
	model.SetMaxRows(2)
	result, err := model.Query(context.Background(), "SELECT 1 AS n UNION ALL SELECT 2 UNION ALL SELECT 3")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Rows) != 2 || !result.Truncated {
		t.Fatalf("result = %#v, want two rows and truncated", result)
	}
}

func TestTasksIsNotLimitedByPublicQueryCap(t *testing.T) {
	t.Parallel()

	model, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { model.Close() })
	model.SetMaxRows(1)
	if err := model.Sync(context.Background(), []task.Task{
		{ID: "one", Title: "One", Status: task.StatusTodo},
		{ID: "two", Title: "Two", Status: task.StatusTodo},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	items, err := model.Tasks(context.Background())
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("Tasks returned %d items, want all 2", len(items))
	}
}
