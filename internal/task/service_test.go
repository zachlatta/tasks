package task

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestCreateAndCompleteTaskWithDependencies(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ids := []string{"write-tests", "ship-feature"}
	service := NewService(repo, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})

	first, err := service.Create(context.Background(), CreateInput{Title: "Write tests"})
	if err != nil {
		t.Fatalf("Create first task: %v", err)
	}
	second, err := service.Create(context.Background(), CreateInput{
		Title:        "Ship feature",
		Dependencies: []string{first.ID},
	})
	if err != nil {
		t.Fatalf("Create dependent task: %v", err)
	}

	if _, err := service.Complete(context.Background(), second.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Complete blocked task error = %v, want ErrBlocked", err)
	}
	if _, err := service.Complete(context.Background(), first.ID); err != nil {
		t.Fatalf("Complete dependency: %v", err)
	}
	completed, err := service.Complete(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("Complete dependent task: %v", err)
	}
	if completed.Status != StatusDone {
		t.Fatalf("status = %q, want %q", completed.Status, StatusDone)
	}
}

func TestStartTaskMovesItIntoProgress(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "build-board" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Build the board"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	now = now.Add(time.Hour)
	started, err := service.Start(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status != StatusInProgress {
		t.Fatalf("status = %q, want %q", started.Status, StatusInProgress)
	}
	if started.Version != 2 || !started.UpdatedAt.Equal(now) {
		t.Fatalf("started task version/time = %d/%v, want 2/%v", started.Version, started.UpdatedAt, now)
	}

	startedAgain, err := service.Start(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Start again: %v", err)
	}
	if startedAgain.Version != started.Version {
		t.Fatalf("repeated Start version = %d, want unchanged %d", startedAgain.Version, started.Version)
	}
}

func TestListOrdersTasksByWorkflowStateThenNewest(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	base := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	for _, item := range []Task{
		{ID: "done", Title: "Done", Status: StatusDone, CreatedAt: base.Add(4 * time.Hour)},
		{ID: "active", Title: "Active", Status: StatusInProgress, CreatedAt: base.Add(3 * time.Hour)},
		{ID: "older-todo", Title: "Older todo", Status: StatusTodo, CreatedAt: base},
		{ID: "newer-todo", Title: "Newer todo", Status: StatusTodo, CreatedAt: base.Add(time.Hour)},
	} {
		if err := repo.Create(context.Background(), item); err != nil {
			t.Fatalf("seed task %q: %v", item.ID, err)
		}
	}
	service := NewService(repo, time.Now, func() string { return "unused" })

	items, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(items))
	for index, item := range items {
		got[index] = item.ID
	}
	want := []string{"newer-todo", "older-todo", "active", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("task order = %v, want %v", got, want)
	}
}

func TestCreateRejectsUnknownDependency(t *testing.T) {
	t.Parallel()

	service := NewService(newMemoryRepository(), time.Now, func() string { return "new-task" })
	_, err := service.Create(context.Background(), CreateInput{
		Title:        "Blocked task",
		Dependencies: []string{"missing"},
	})
	if !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("Create error = %v, want ErrDependencyNotFound", err)
	}
}

func TestCreateRejectsBlankTitle(t *testing.T) {
	t.Parallel()

	service := NewService(newMemoryRepository(), time.Now, func() string { return "new-task" })
	if _, err := service.Create(context.Background(), CreateInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create error = %v, want ErrInvalid", err)
	}
}

func TestEditUpdatesFieldsAndAppliesGuardedTextReplacements(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	ids := []string{"dependency", "editable"}
	service := NewService(repo, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	dependency, err := service.Create(context.Background(), CreateInput{Title: "Dependency"})
	if err != nil {
		t.Fatalf("Create dependency: %v", err)
	}
	created, err := service.Create(context.Background(), CreateInput{
		Title:       "Research sources",
		Description: "Find source. Summarize source.",
	})
	if err != nil {
		t.Fatalf("Create editable task: %v", err)
	}

	now = now.Add(time.Hour)
	title := "Research primary sources"
	description := "Find primary source. Summarize primary source."
	dependencies := []string{dependency.ID, dependency.ID, ""}
	expectedVersion := created.Version
	updated, err := service.Edit(context.Background(), created.ID, EditInput{
		Title:           &title,
		Description:     &description,
		Dependencies:    &dependencies,
		ExpectedVersion: &expectedVersion,
	})
	if err != nil {
		t.Fatalf("Edit fields: %v", err)
	}
	if updated.Title != title || updated.Description != description {
		t.Fatalf("updated text = %q / %q", updated.Title, updated.Description)
	}
	if !slices.Equal(updated.Dependencies, []string{dependency.ID}) {
		t.Fatalf("updated dependencies = %v", updated.Dependencies)
	}
	if updated.Version != 2 || !updated.UpdatedAt.Equal(now) {
		t.Fatalf("updated version/time = %d/%v, want 2/%v", updated.Version, updated.UpdatedAt, now)
	}

	now = now.Add(time.Hour)
	updated, err = service.Edit(context.Background(), created.ID, EditInput{
		Replacements: []TextReplacement{
			{Field: TextFieldDescription, OldText: "primary source", NewText: "primary source document", ReplaceAll: true},
			{Field: TextFieldTitle, OldText: "primary sources", NewText: "primary source documents"},
		},
	})
	if err != nil {
		t.Fatalf("Edit replacements: %v", err)
	}
	if updated.Title != "Research primary source documents" {
		t.Fatalf("replacement title = %q", updated.Title)
	}
	if updated.Description != "Find primary source document. Summarize primary source document." {
		t.Fatalf("replacement description = %q", updated.Description)
	}
	if updated.Version != 3 || !updated.UpdatedAt.Equal(now) {
		t.Fatalf("replacement version/time = %d/%v, want 3/%v", updated.Version, updated.UpdatedAt, now)
	}
}

func TestEditRejectsAmbiguousOrStaleChangesAtomically(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	service := NewService(repo, time.Now, func() string { return "editable" })
	created, err := service.Create(context.Background(), CreateInput{
		Title:       "Review sources",
		Description: "source and source",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = service.Edit(context.Background(), created.ID, EditInput{
		Replacements: []TextReplacement{
			{Field: TextFieldTitle, OldText: "Review", NewText: "Inspect"},
			{Field: TextFieldDescription, OldText: "source", NewText: "document"},
		},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous Edit error = %v, want ErrInvalid", err)
	}
	afterAmbiguous, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous edit: %v", err)
	}
	if afterAmbiguous.Title != created.Title || afterAmbiguous.Description != created.Description || afterAmbiguous.Version != created.Version {
		t.Fatalf("task changed after ambiguous edit: %#v", afterAmbiguous)
	}

	staleVersion := created.Version + 1
	title := "Stale title"
	_, err = service.Edit(context.Background(), created.ID, EditInput{
		Title:           &title,
		ExpectedVersion: &staleVersion,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Edit error = %v, want ErrConflict", err)
	}
}

func TestEditRejectsInvalidTitleAndDependencyCycles(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	ids := []string{"first", "second"}
	service := NewService(repo, time.Now, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	first, err := service.Create(context.Background(), CreateInput{Title: "First"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := service.Create(context.Background(), CreateInput{
		Title:        "Second",
		Dependencies: []string{first.ID},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	blank := "   "
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Title: &blank}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank title Edit error = %v, want ErrInvalid", err)
	}
	unknown := []string{"missing"}
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Dependencies: &unknown}); !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("unknown dependency Edit error = %v, want ErrDependencyNotFound", err)
	}
	cycle := []string{second.ID}
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Dependencies: &cycle}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cyclic dependency Edit error = %v, want ErrInvalid", err)
	}
}

func TestEditRequiresARequestedChangeAndSkipsNoOpWrites(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	service := NewService(repo, time.Now, func() string { return "editable" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Same"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.Edit(context.Background(), created.ID, EditInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty Edit error = %v, want ErrInvalid", err)
	}
	sameTitle := created.Title
	unchanged, err := service.Edit(context.Background(), created.ID, EditInput{Title: &sameTitle})
	if err != nil {
		t.Fatalf("no-op Edit: %v", err)
	}
	if unchanged.Version != created.Version || !unchanged.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("no-op Edit changed version/time: %#v", unchanged)
	}
}

type memoryRepository struct {
	tasks map[string]Task
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{tasks: make(map[string]Task)}
}

func (r *memoryRepository) Create(_ context.Context, task Task) error {
	if _, ok := r.tasks[task.ID]; ok {
		return ErrAlreadyExists
	}
	r.tasks[task.ID] = task
	return nil
}

func (r *memoryRepository) Update(_ context.Context, task Task) error {
	if _, ok := r.tasks[task.ID]; !ok {
		return ErrNotFound
	}
	r.tasks[task.ID] = task
	return nil
}

func (r *memoryRepository) Get(_ context.Context, id string) (Task, error) {
	task, ok := r.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return task, nil
}

func (r *memoryRepository) List(_ context.Context) ([]Task, error) {
	tasks := make([]Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		tasks = append(tasks, task)
	}
	return tasks, nil
}
