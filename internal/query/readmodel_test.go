package query

import (
	"context"
	"path/filepath"
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
		SELECT t.id, t.status, d.dependency_id, a.name AS attachment
		FROM tasks t
		LEFT JOIN task_dependencies d ON d.task_id = t.id
		LEFT JOIN task_attachments a ON a.task_id = t.id
		WHERE t.id = 'ship-feature'
	`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(result.Rows))
	}
	row := result.Rows[0]
	if row["dependency_id"] != "write-tests" || row["attachment"] != "diagram.png" {
		t.Fatalf("row = %#v", row)
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
