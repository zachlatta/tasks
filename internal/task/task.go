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
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
)

var (
	ErrAlreadyExists      = errors.New("task already exists")
	ErrBlocked            = errors.New("task is blocked by incomplete dependencies")
	ErrDependencyNotFound = errors.New("task dependency not found")
	ErrConflict           = errors.New("task was changed by another writer")
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
	Version      int64        `json:"version" yaml:"version"`
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

type TextField string

const (
	TextFieldTitle       TextField = "title"
	TextFieldDescription TextField = "description"
)

// TextReplacement is a guarded, literal replacement applied to one text
// field. Unless ReplaceAll is true, OldText must occur exactly once.
type TextReplacement struct {
	Field      TextField `json:"field" jsonschema:"Text field to edit. Must be title or description."`
	OldText    string    `json:"old_text" jsonschema:"Exact non-empty text to find. By default it must occur exactly once."`
	NewText    string    `json:"new_text" jsonschema:"Literal replacement text. May be empty to delete the matched text."`
	ReplaceAll bool      `json:"replace_all,omitempty" jsonschema:"Replace every occurrence instead of requiring exactly one match."`
}

// EditInput describes one atomic task edit. Pointer fields distinguish an
// omitted field from a request to clear it. Replacements run in order after
// any whole-field values have been applied.
type EditInput struct {
	Title           *string
	Description     *string
	Dependencies    *[]string
	Replacements    []TextReplacement
	ExpectedVersion *int64
}

// AuditMetadata describes who initiated a task mutation and through which
// interface. Service methods add the semantic action before the repository
// persists the mutation.
type AuditMetadata struct {
	Action    string
	ActorKind string
	ActorID   string
	Source    string
	RequestID string
}

type auditMetadataContextKey struct{}

// WithAuditMetadata associates mutation attribution with a request context.
func WithAuditMetadata(ctx context.Context, metadata AuditMetadata) context.Context {
	return context.WithValue(ctx, auditMetadataContextKey{}, metadata)
}

// AuditMetadataFromContext returns mutation attribution previously attached to
// ctx. The zero value means no interface supplied attribution.
func AuditMetadataFromContext(ctx context.Context) AuditMetadata {
	metadata, _ := ctx.Value(auditMetadataContextKey{}).(AuditMetadata)
	return metadata
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
		Version:      1,
	}
	if created.ID == "" {
		return Task{}, fmt.Errorf("%w: generated ID is empty", ErrInvalid)
	}
	if err := s.repository.Create(withAuditAction(ctx, "create"), created); err != nil {
		return Task{}, err
	}
	return clone(created), nil
}

func (s *Service) Complete(ctx context.Context, id string) (Task, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Status == StatusDone {
		return clone(current), nil
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
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, "complete"), current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

func (s *Service) Start(ctx context.Context, id string) (Task, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Status == StatusInProgress {
		return clone(current), nil
	}
	if current.Status != StatusTodo {
		return Task{}, fmt.Errorf("%w: only todo tasks can be started", ErrInvalid)
	}
	current.Status = StatusInProgress
	current.UpdatedAt = s.now().UTC()
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, "start"), current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

// Edit atomically replaces whole mutable fields and/or applies guarded text
// replacements. It preserves workflow status, timestamps of creation, and
// attachments.
func (s *Service) Edit(ctx context.Context, id string, input EditInput) (Task, error) {
	if input.Title == nil &&
		input.Description == nil &&
		input.Dependencies == nil &&
		len(input.Replacements) == 0 {
		return Task{}, fmt.Errorf("%w: at least one edit is required", ErrInvalid)
	}
	if input.ExpectedVersion != nil && *input.ExpectedVersion < 1 {
		return Task{}, fmt.Errorf("%w: expected version must be positive", ErrInvalid)
	}

	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if input.ExpectedVersion != nil && current.Version != *input.ExpectedVersion {
		return Task{}, fmt.Errorf(
			"%w: expected version %d, found %d",
			ErrConflict, *input.ExpectedVersion, current.Version,
		)
	}

	edited := clone(current)
	if input.Title != nil {
		edited.Title = *input.Title
	}
	if input.Description != nil {
		edited.Description = *input.Description
	}
	if input.Dependencies != nil {
		edited.Dependencies = uniqueNonEmpty(*input.Dependencies)
	}
	for index, replacement := range input.Replacements {
		if err := applyTextReplacement(&edited, replacement); err != nil {
			return Task{}, fmt.Errorf("replacement %d: %w", index+1, err)
		}
	}

	edited.Title = strings.TrimSpace(edited.Title)
	if edited.Title == "" {
		return Task{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if input.Dependencies != nil {
		if err := s.validateEditedDependencies(ctx, edited.ID, edited.Dependencies); err != nil {
			return Task{}, err
		}
	}
	if edited.Title == current.Title &&
		edited.Description == current.Description &&
		slices.Equal(edited.Dependencies, current.Dependencies) {
		return clone(current), nil
	}

	edited.UpdatedAt = s.now().UTC()
	edited.Version++
	if err := s.repository.Update(withAuditAction(ctx, "edit"), edited); err != nil {
		return Task{}, err
	}
	return clone(edited), nil
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
			return statusOrder(items[i].Status) < statusOrder(items[j].Status)
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	for i := range items {
		items[i] = clone(items[i])
	}
	return items, nil
}

func statusOrder(status Status) int {
	switch status {
	case StatusTodo:
		return 0
	case StatusInProgress:
		return 1
	case StatusDone:
		return 2
	default:
		return 3
	}
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
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, "add_attachment"), current); err != nil {
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

func applyTextReplacement(item *Task, replacement TextReplacement) error {
	if replacement.OldText == "" {
		return fmt.Errorf("%w: old_text is required", ErrInvalid)
	}
	var field *string
	switch replacement.Field {
	case TextFieldTitle:
		field = &item.Title
	case TextFieldDescription:
		field = &item.Description
	default:
		return fmt.Errorf("%w: field must be title or description", ErrInvalid)
	}

	matches := strings.Count(*field, replacement.OldText)
	if matches == 0 {
		return fmt.Errorf("%w: old_text was not found in %s", ErrInvalid, replacement.Field)
	}
	if !replacement.ReplaceAll && matches != 1 {
		return fmt.Errorf(
			"%w: old_text occurs %d times in %s; provide more context or set replace_all",
			ErrInvalid, matches, replacement.Field,
		)
	}
	limit := 1
	if replacement.ReplaceAll {
		limit = -1
	}
	*field = strings.Replace(*field, replacement.OldText, replacement.NewText, limit)
	return nil
}

func (s *Service) validateEditedDependencies(ctx context.Context, taskID string, dependencies []string) error {
	if len(dependencies) == 0 {
		return nil
	}
	items, err := s.repository.List(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]Task, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	for _, dependency := range dependencies {
		if _, ok := byID[dependency]; !ok {
			return fmt.Errorf("%w: %s", ErrDependencyNotFound, dependency)
		}
		if dependencyReaches(dependency, taskID, byID, make(map[string]bool)) {
			return fmt.Errorf("%w: dependency %s would create a cycle", ErrInvalid, dependency)
		}
	}
	return nil
}

func dependencyReaches(currentID, targetID string, tasks map[string]Task, visiting map[string]bool) bool {
	if currentID == targetID {
		return true
	}
	if visiting[currentID] {
		return false
	}
	visiting[currentID] = true
	defer delete(visiting, currentID)
	for _, dependency := range tasks[currentID].Dependencies {
		if dependencyReaches(dependency, targetID, tasks, visiting) {
			return true
		}
	}
	return false
}

func withAuditAction(ctx context.Context, action string) context.Context {
	metadata := AuditMetadataFromContext(ctx)
	metadata.Action = action
	return WithAuditMetadata(ctx, metadata)
}

func clone(item Task) Task {
	item.Dependencies = slices.Clone(item.Dependencies)
	item.Attachments = slices.Clone(item.Attachments)
	return item
}
