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
	ErrHasDependents      = errors.New("other tasks depend on this task")
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
	Position     float64      `json:"position" yaml:"position"`
	Dependencies []string     `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty" yaml:"attachments,omitempty"`
	CreatedAt    time.Time    `json:"created_at" yaml:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at" yaml:"updated_at"`
	Version      int64        `json:"version" yaml:"version"`
	// DeletedAt marks a soft-deleted task. The row and its whole revision
	// history stay; the task simply leaves the board and every read until it is
	// restored. Nil means the task is live.
	DeletedAt *time.Time `json:"deleted_at,omitempty" yaml:"deleted_at,omitempty"`
}

// Deleted reports whether the task has been soft-deleted.
func (t Task) Deleted() bool {
	return t.DeletedAt != nil
}

// positionGap is the spacing a task claims when it lands at the top or bottom
// of a column. Landing between two cards takes the midpoint of its neighbors
// instead, so a single row changes per move.
const positionGap = 1024

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
		if _, err := s.live(ctx, dependency); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Task{}, fmt.Errorf("%w: %s", ErrDependencyNotFound, dependency)
			}
			return Task{}, err
		}
	}
	todo, err := s.column(ctx, StatusTodo)
	if err != nil {
		return Task{}, err
	}
	position, _ := positionAt(todo, 0)
	now := s.now().UTC()
	created := Task{
		ID:           s.newID(),
		Title:        input.Title,
		Description:  input.Description,
		Status:       StatusTodo,
		Position:     position,
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
	current, err := s.live(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Status == StatusDone {
		return clone(current), nil
	}
	return s.Move(ctx, id, StatusDone, 0)
}

func (s *Service) Start(ctx context.Context, id string) (Task, error) {
	current, err := s.live(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Status == StatusInProgress {
		return clone(current), nil
	}
	if current.Status != StatusTodo {
		return Task{}, fmt.Errorf("%w: only todo tasks can be started", ErrInvalid)
	}
	return s.Move(ctx, id, StatusInProgress, 0)
}

// Move places a task at index within the column for status, which is how the
// board's drag and drop reorders a column and moves work between columns. An
// index outside the column clamps to its nearest end, and a move that changes
// nothing is a no-op. Moving into done still requires completed dependencies.
func (s *Service) Move(ctx context.Context, id string, status Status, index int) (Task, error) {
	switch status {
	case StatusTodo, StatusInProgress, StatusDone:
	default:
		return Task{}, fmt.Errorf("%w: unknown status %q", ErrInvalid, status)
	}
	current, err := s.live(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if status == StatusDone {
		for _, dependency := range current.Dependencies {
			required, err := s.live(ctx, dependency)
			if err != nil {
				return Task{}, err
			}
			if required.Status != StatusDone {
				return Task{}, fmt.Errorf("%w: %s", ErrBlocked, dependency)
			}
		}
	}
	column, err := s.column(ctx, status)
	if err != nil {
		return Task{}, err
	}
	from := slices.IndexFunc(column, func(item Task) bool { return item.ID == id })
	if from >= 0 {
		column = slices.Delete(column, from, from+1)
	}
	index = min(max(index, 0), len(column))
	if current.Status == status && from == index {
		return clone(current), nil
	}
	position, ok := positionAt(column, index)
	if !ok {
		// Neighboring positions have no value between them, which also covers
		// tasks imported before positions existed and still sharing zero.
		if column, err = s.rebalance(ctx, column); err != nil {
			return Task{}, err
		}
		if position, ok = positionAt(column, index); !ok {
			return Task{}, fmt.Errorf("%w: cannot place task in the %s column", ErrInvalid, status)
		}
	}
	action := "reorder"
	if current.Status != status {
		action = statusAction(status)
	}
	current.Status = status
	current.Position = position
	current.UpdatedAt = s.now().UTC()
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, action), current); err != nil {
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

	current, err := s.live(ctx, id)
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

// Delete soft-deletes a task. The stored row and its whole revision history
// stay behind; the task leaves the board and every other read until Restore
// brings it back exactly where it was. Deleting a task that live tasks still
// depend on is refused, which keeps every live dependency resolvable. Deleting
// an already-deleted task changes nothing.
func (s *Service) Delete(ctx context.Context, id string) (Task, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if current.Deleted() {
		return clone(current), nil
	}
	items, err := s.repository.List(ctx)
	if err != nil {
		return Task{}, err
	}
	for _, item := range items {
		if item.ID == id || item.Deleted() {
			continue
		}
		if slices.Contains(item.Dependencies, id) {
			return Task{}, fmt.Errorf("%w: %s depends on it", ErrHasDependents, item.ID)
		}
	}
	deletedAt := s.now().UTC()
	current.DeletedAt = &deletedAt
	current.UpdatedAt = deletedAt
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, "delete"), current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

// Restore returns a soft-deleted task to the board with its status, board
// position, text, dependencies, and attachments intact. A task whose own
// dependencies are still deleted cannot be restored, so its prerequisites come
// back first. Restoring a live task changes nothing.
func (s *Service) Restore(ctx context.Context, id string) (Task, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if !current.Deleted() {
		return clone(current), nil
	}
	for _, dependency := range current.Dependencies {
		if _, err := s.live(ctx, dependency); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Task{}, fmt.Errorf("%w: %s; restore it first", ErrDependencyNotFound, dependency)
			}
			return Task{}, err
		}
	}
	current.DeletedAt = nil
	current.UpdatedAt = s.now().UTC()
	current.Version++
	if err := s.repository.Update(withAuditAction(ctx, "restore"), current); err != nil {
		return Task{}, err
	}
	return clone(current), nil
}

// live loads a task that is still on the board. A soft-deleted task reads as
// missing to every operation except Delete and Restore.
func (s *Service) live(ctx context.Context, id string) (Task, error) {
	item, err := s.repository.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if item.Deleted() {
		return Task{}, ErrNotFound
	}
	return item, nil
}

// column returns the tasks in one board column, top first.
func (s *Service) column(ctx context.Context, status Status) ([]Task, error) {
	items, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	column := make([]Task, 0, len(items))
	for _, item := range items {
		if item.Status == status && !item.Deleted() {
			column = append(column, clone(item))
		}
	}
	sort.SliceStable(column, func(i, j int) bool { return beforeInColumn(column[i], column[j]) })
	return column, nil
}

// rebalance spreads a column's positions back out. It is the rare fallback for
// a gap that can no longer be split, so it writes every other card in the
// column while leaving their user-visible fields alone.
func (s *Service) rebalance(ctx context.Context, column []Task) ([]Task, error) {
	for index := range column {
		column[index].Position = float64(index+1) * positionGap
		column[index].Version++
		if err := s.repository.Update(withAuditAction(ctx, "rebalance"), column[index]); err != nil {
			return nil, err
		}
	}
	return column, nil
}

// positionAt returns the position a task needs to sit at index in column, which
// must exclude the task being placed. It reports false when the neighboring
// positions leave no value in between.
func positionAt(column []Task, index int) (float64, bool) {
	switch {
	case len(column) == 0:
		return 0, true
	case index <= 0:
		return column[0].Position - positionGap, true
	case index >= len(column):
		return column[len(column)-1].Position + positionGap, true
	}
	previous, next := column[index-1].Position, column[index].Position
	middle := previous + (next-previous)/2
	if !(middle > previous && middle < next) {
		return 0, false
	}
	return middle, true
}

func statusAction(status Status) string {
	switch status {
	case StatusTodo:
		return "reopen"
	case StatusInProgress:
		return "start"
	case StatusDone:
		return "complete"
	}
	return "move"
}

// beforeInColumn orders one column: by hand-set position, then newest first for
// tasks that have never been dragged and still share a position.
func beforeInColumn(first, second Task) bool {
	if first.Position != second.Position {
		return first.Position < second.Position
	}
	if !first.CreatedAt.Equal(second.CreatedAt) {
		return first.CreatedAt.After(second.CreatedAt)
	}
	return first.ID < second.ID
}

func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	item, err := s.live(ctx, id)
	return clone(item), err
}

// List returns every live task. Soft-deleted tasks are left out; they are read
// back through the store's own deleted-task projection.
func (s *Service) List(ctx context.Context) ([]Task, error) {
	stored, err := s.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Task, 0, len(stored))
	for _, item := range stored {
		if !item.Deleted() {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Status != items[j].Status {
			return statusOrder(items[i].Status) < statusOrder(items[j].Status)
		}
		return beforeInColumn(items[i], items[j])
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
	current, err := s.live(ctx, id)
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
		if !item.Deleted() {
			byID[item.ID] = item
		}
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
	if item.DeletedAt != nil {
		deletedAt := *item.DeletedAt
		item.DeletedAt = &deletedAt
	}
	return item
}
