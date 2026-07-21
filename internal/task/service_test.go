package task

import (
	"context"
	"errors"
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
