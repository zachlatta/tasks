// Package tasktest provides an in-memory task.Repository for tests that need a
// task service but not the real PostgreSQL storage (for example, HTTP handler
// tests).
package tasktest

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/zachlatta/tasks/internal/task"
)

// Repository is a goroutine-safe, in-memory implementation of task.Repository.
type Repository struct {
	mu    sync.Mutex
	tasks map[string]task.Task
}

// NewRepository returns an empty in-memory repository.
func NewRepository() *Repository {
	return &Repository{tasks: make(map[string]task.Task)}
}

func (r *Repository) Create(_ context.Context, item task.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[item.ID]; ok {
		return task.ErrAlreadyExists
	}
	r.tasks[item.ID] = clone(item)
	return nil
}

func (r *Repository) Update(_ context.Context, item task.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[item.ID]; !ok {
		return task.ErrNotFound
	}
	r.tasks[item.ID] = clone(item)
	return nil
}

func (r *Repository) Get(_ context.Context, id string) (task.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.tasks[id]
	if !ok {
		return task.Task{}, task.ErrNotFound
	}
	return clone(item), nil
}

func (r *Repository) List(_ context.Context) ([]task.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]task.Task, 0, len(r.tasks))
	for _, item := range r.tasks {
		items = append(items, clone(item))
	}
	return items, nil
}

// Tasks returns every live task ordered by workflow state, then by board
// position with newest-first as the tiebreak, mirroring the fixed projection the
// web UI reads through in production. Soft-deleted tasks are left off the board.
func (r *Repository) Tasks(ctx context.Context) ([]task.Task, error) {
	stored, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]task.Task, 0, len(stored))
	for _, item := range stored {
		if !item.Deleted() {
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Status != items[j].Status {
			return statusOrder(items[i].Status) < statusOrder(items[j].Status)
		}
		if items[i].Position != items[j].Position {
			return items[i].Position < items[j].Position
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

// DeletedTasks returns the soft-deleted tasks, most recently deleted first,
// mirroring the store's own deleted-task projection.
func (r *Repository) DeletedTasks(ctx context.Context) ([]task.Task, error) {
	stored, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]task.Task, 0, len(stored))
	for _, item := range stored {
		if item.Deleted() {
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].DeletedAt.Equal(*items[j].DeletedAt) {
			return items[i].DeletedAt.After(*items[j].DeletedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func statusOrder(status task.Status) int {
	switch status {
	case task.StatusTodo:
		return 0
	case task.StatusInProgress:
		return 1
	case task.StatusDone:
		return 2
	default:
		return 3
	}
}

func clone(item task.Task) task.Task {
	item.Dependencies = slices.Clone(item.Dependencies)
	item.Attachments = slices.Clone(item.Attachments)
	if item.DeletedAt != nil {
		deletedAt := *item.DeletedAt
		item.DeletedAt = &deletedAt
	}
	return item
}
