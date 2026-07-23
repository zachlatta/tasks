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

// Tasks returns every task ordered by workflow state then newest-first,
// mirroring the fixed projection the web UI reads through in production.
func (r *Repository) Tasks(ctx context.Context) ([]task.Task, error) {
	items, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Status != items[j].Status {
			return statusOrder(items[i].Status) < statusOrder(items[j].Status)
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
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
	return item
}
