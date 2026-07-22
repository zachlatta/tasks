package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zachlatta/task-tracker/internal/pgtest"
	"github.com/zachlatta/task-tracker/internal/task"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestStoreRoundTripLoadsChildren(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

	for _, id := range []string{"write-tests", "write-docs"} {
		if err := store.Create(ctx, task.Task{ID: id, Title: id, Description: "", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	want := task.Task{
		ID:           "ship-feature",
		Title:        "Ship feature",
		Description:  "Release it",
		Status:       task.StatusTodo,
		Dependencies: []string{"write-tests", "write-docs"},
		Attachments: []task.Attachment{
			{Key: "ship-feature/a.png", Name: "a.png", ContentType: "image/png"},
			{Key: "ship-feature/b.png", Name: "b.png", ContentType: "image/png"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := store.Get(ctx, "ship-feature")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != want.Title || got.Description != want.Description || got.Status != want.Status {
		t.Fatalf("scalar fields = %#v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %v / %v", got.CreatedAt, got.UpdatedAt)
	}
	// Dependencies come back ordered by depends_on_id.
	if len(got.Dependencies) != 2 || got.Dependencies[0] != "write-docs" || got.Dependencies[1] != "write-tests" {
		t.Fatalf("dependencies = %v", got.Dependencies)
	}
	if len(got.Attachments) != 2 || got.Attachments[0].Key != "ship-feature/a.png" || got.Attachments[1].Key != "ship-feature/b.png" {
		t.Fatalf("attachments = %#v", got.Attachments)
	}
}

func TestStoreCreateDuplicateReturnsAlreadyExists(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	item := task.Task{ID: "dup", Title: "Dup", Status: task.StatusTodo, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(ctx, item); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := store.Create(ctx, item); !errors.Is(err, task.ErrAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrAlreadyExists", err)
	}
}

func TestStoreGetAndUpdateMissingReturnNotFound(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.Get(ctx, "nope"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
	missing := task.Task{ID: "nope", Title: "Nope", Status: task.StatusTodo, UpdatedAt: time.Now()}
	if err := store.Update(ctx, missing); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("update err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateReplacesStatusAndChildren(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, task.Task{ID: "dep", Title: "Dep", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create dep: %v", err)
	}
	if err := store.Create(ctx, task.Task{
		ID: "main", Title: "Main", Status: task.StatusTodo,
		Dependencies: []string{"dep"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create main: %v", err)
	}
	updated := task.Task{ID: "main", Title: "Main", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now.Add(time.Hour)}
	if err := store.Update(ctx, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.Get(ctx, "main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if len(got.Dependencies) != 0 {
		t.Fatalf("dependencies = %v, want cleared", got.Dependencies)
	}
}

func TestTasksOrdersTodoFirstThenNewest(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, task.Task{ID: "old-todo", Title: "Old todo", Status: task.StatusTodo, CreatedAt: base, UpdatedAt: base})
	mustCreate(t, store, task.Task{ID: "new-todo", Title: "New todo", Status: task.StatusTodo, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)})
	mustCreate(t, store, task.Task{ID: "done-task", Title: "Done", Status: task.StatusDone, CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)})

	items, err := store.Tasks(ctx)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	order := []string{items[0].ID, items[1].ID, items[2].ID}
	want := []string{"new-todo", "old-todo", "done-task"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("Tasks order = %v, want %v", order, want)
		}
	}
}

func TestQueryOverviewRejectsWritesAndCaps(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, task.Task{ID: "a", Title: "A", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now})
	mustCreate(t, store, task.Task{
		ID: "b", Title: "B", Status: task.StatusTodo, Dependencies: []string{"a"},
		Attachments: []task.Attachment{{Key: "b/x.png", Name: "x.png", ContentType: "image/png"}},
		CreatedAt:   now, UpdatedAt: now,
	})

	// task_overview exposes computed columns; b's only dependency is done, so
	// b is not blocked.
	overview, err := store.Query(ctx, "SELECT blocked, dependency_count, image_count FROM task_overview WHERE id = 'b'")
	if err != nil {
		t.Fatalf("query task_overview: %v", err)
	}
	row := overview.Rows[0]
	if fmt.Sprint(row["blocked"]) != "0" || fmt.Sprint(row["dependency_count"]) != "1" || fmt.Sprint(row["image_count"]) != "1" {
		t.Fatalf("task_overview row = %#v", row)
	}

	if _, err := store.Query(ctx, "DELETE FROM tasks"); err == nil {
		t.Fatal("DELETE accepted, want rejection")
	}
	if _, err := store.Query(ctx, "WITH gone AS (DELETE FROM tasks RETURNING id) SELECT * FROM gone"); err == nil {
		t.Fatal("CTE write accepted, want read-only transaction to block it")
	}

	store.SetMaxRows(1)
	capped, err := store.Query(ctx, "SELECT id FROM tasks")
	if err != nil {
		t.Fatalf("capped select: %v", err)
	}
	if !capped.Truncated || len(capped.Rows) != 1 {
		t.Fatalf("capped result = %#v, want 1 row truncated", capped)
	}
}

func mustCreate(t *testing.T, store *Store, item task.Task) {
	t.Helper()
	if err := store.Create(context.Background(), item); err != nil {
		t.Fatalf("create %s: %v", item.ID, err)
	}
}
