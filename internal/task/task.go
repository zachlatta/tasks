package task

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type Status string

const (
	StatusTodo Status = "todo"
	StatusDone Status = "done"
)

var (
	ErrAlreadyExists      = errors.New("task already exists")
	ErrBlocked            = errors.New("task is blocked by incomplete dependencies")
	ErrDependencyNotFound = errors.New("task dependency not found")
	ErrInvalid            = errors.New("invalid task")
	ErrNotFound           = errors.New("task not found")
)

type Attachment struct {
	Key         string `json:"key" yaml:"key"`
	Name        string `json:"name" yaml:"name"`
	ContentType string `json:"content_type" yaml:"content_type"`
}

type Task struct {
	ID           string       `json:"id" yaml:"id"`
	Title        string       `json:"title" yaml:"title"`
	Description  string       `json:"description" yaml:"-"`
	Status       Status       `json:"status" yaml:"status"`
	Dependencies []string     `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty" yaml:"attachments,omitempty"`
	CreatedAt    time.Time    `json:"created_at" yaml:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at" yaml:"updated_at"`
}

type Repository interface {
	Create(context.Context, Task) error
	Update(context.Context, Task) error
	Get(context.Context, string) (Task, error)
	List(context.Context) ([]Task, error)
}

type CreateInput struct {
	Title        string   `json:"title"`
	Description  string   `json:"description,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Service struct {
	repository Repository
	now        func() time.Time
	newID      func() string
}

func NewService(repository Repository, now func() time.Time, newID func() string) *Service {
	return &Service{repository: repository, now: now, newID: newID}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (Task, error) {
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return Task{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	dependencies := uniqueNonEmpty(input.Dependencies)
	for _, dependency := range dependencies {
		if _, err := s.repository.Get(ctx, dependency); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Task{}, fmt.Errorf("%w: %s", ErrDependencyNotFound, dependency)
			}
			return Task{}, err
		}
	}
	now := s.now().UTC()
	created := Task{
		ID:           s.newID(),
		Title:        input.Title,
		Description:  input.Description,
		Status:       StatusTodo,
		Dependencies: dependencies,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if created.ID == "" {
		return Task{}, fmt.Errorf("%w: generated ID is empty", ErrInvalid)
	}
	if err := s.repository.Create(ctx, created); err != nil {
		return Task{}, err
	}
	return clone(created), nil
}

func (s *Service) Complete(ctx context.Context, id string) (Task, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	for _, dependency := range current.Dependencies {
		required, err := s.repository.Get(ctx, dependency)
		if err != nil {
			return Task{}, err
		}
		if required.Status != StatusDone {
			return Task{}, fmt.Errorf("%w: %s", ErrBlocked, dependency)
		}
	}
	current.Status = StatusDone
	current.UpdatedAt = s.now().UTC()
	if err := s.repository.Update(ctx, current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	item, err := s.repository.Get(ctx, id)
	return clone(item), err
}

func (s *Service) List(ctx context.Context) ([]Task, error) {
	items, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Status != items[j].Status {
			return items[i].Status == StatusTodo
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	for i := range items {
		items[i] = clone(items[i])
	}
	return items, nil
}

func (s *Service) AddAttachment(ctx context.Context, id string, attachment Attachment) (Task, error) {
	if strings.TrimSpace(attachment.Key) == "" || strings.TrimSpace(attachment.Name) == "" {
		return Task{}, fmt.Errorf("%w: attachment key and name are required", ErrInvalid)
	}
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	current.Attachments = append(current.Attachments, attachment)
	current.UpdatedAt = s.now().UTC()
	if err := s.repository.Update(ctx, current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

func uniqueNonEmpty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func clone(item Task) Task {
	item.Dependencies = slices.Clone(item.Dependencies)
	item.Attachments = slices.Clone(item.Attachments)
	return item
}
