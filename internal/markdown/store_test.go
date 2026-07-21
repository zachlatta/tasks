package markdown

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/task-tracker/internal/task"
)

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewStore(dir)
	want := task.Task{
		ID:           "ship-feature",
		Title:        "Ship feature",
		Description:  "Keep the shared backend small.\n",
		Status:       task.StatusTodo,
		Dependencies: []string{"write-tests"},
		Attachments:  []task.Attachment{{Key: "ship-feature/diagram.png", Name: "diagram\n---\n.png", ContentType: "image/png"}},
		CreatedAt:    time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, time.July, 16, 12, 5, 0, 0, time.UTC),
	}

	if err := store.Create(context.Background(), want); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != want.Title || got.Description != want.Description || len(got.Dependencies) != 1 || len(got.Attachments) != 1 {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}

	contents, err := os.ReadFile(filepath.Join(dir, want.ID+".md"))
	if err != nil {
		t.Fatalf("Read task file: %v", err)
	}
	if !strings.Contains(string(contents), "title: Ship feature") || !strings.Contains(string(contents), "Keep the shared backend small.") {
		t.Fatalf("task file is not readable Markdown:\n%s", contents)
	}
}

func TestStoreRejectsUnsafeID(t *testing.T) {
	t.Parallel()

	store := NewStore(t.TempDir())
	err := store.Create(context.Background(), task.Task{ID: "../escape", Title: "Escape"})
	if err == nil {
		t.Fatal("Create unsafe ID succeeded, want error")
	}
}
