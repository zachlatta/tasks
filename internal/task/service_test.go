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
