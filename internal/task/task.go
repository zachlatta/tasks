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
	// WakeAt holds a task back from the board until the date arrives. It is the
	// half of a wake condition a clock can settle; unfinished dependencies are
	// the other half, and callers combine both.
	WakeAt *time.Time `json:"wake_at,omitempty" yaml:"wake_at,omitempty"`
	// WaitingOn names who or what the task is waiting for, in one line. It is
	// the reason a wake date exists and is shown wherever the task is.
	WaitingOn string `json:"waiting_on,omitempty" yaml:"waiting_on,omitempty"`
	// OnTimeout is the default action to take if the wait has not resolved by
	// WakeAt. Keeping it with the wait makes the review a decision, not another
	// round of reconstructing context.
	OnTimeout string `json:"on_timeout,omitempty" yaml:"on_timeout,omitempty"`
	// SnoozeCount counts how many times the wake date has been pushed out
	// without the task being finished, so a thread nobody is going to answer
	// stops looking like one that is merely early.
	SnoozeCount int `json:"snooze_count,omitempty" yaml:"snooze_count,omitempty"`
	// Context is a lightweight execution constraint such as home, office, or
	// online. It is deliberately a tag rather than a precise location.
	Context string `json:"context,omitempty" yaml:"context,omitempty"`
	// ContextCheckedAt records when a person last verified the task's
	// supporting context. Ordinary task edits do not change it.
	ContextCheckedAt *time.Time `json:"context_checked_at,omitempty" yaml:"context_checked_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at" yaml:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" yaml:"updated_at"`
	Version          int64      `json:"version" yaml:"version"`
	// DeletedAt marks a soft-deleted task. The row and its whole revision
	// history stay; the task simply leaves the board and every read until it is
	// restored. Nil means the task is live.
	DeletedAt *time.Time `json:"deleted_at,omitempty" yaml:"deleted_at,omitempty"`
}

// Deleted reports whether the task has been soft-deleted.
func (t Task) Deleted() bool {
	return t.DeletedAt != nil
}

// Snoozed reports whether the task's wake date is still in the future at now.
// It answers only the clock half of the wake condition; a caller that also
// knows the task's dependencies treats an unfinished one as asleep too.
func (t Task) Snoozed(now time.Time) bool {
	return t.WakeAt != nil && now.Before(*t.WakeAt)
}

// StaleWait reports whether the wait has been pushed out enough times that it
// is more likely dead than early.
func (t Task) StaleWait() bool {
	return t.SnoozeCount >= staleSnoozeCount
}

// staleSnoozeCount is how many pushes turn a wait into a prompt to escalate,
// proceed without the other party, or drop the thread.
const staleSnoozeCount = 3

// WakeAtLayouts are the wake-date spellings every interface accepts. A bare
// date means midnight UTC, so a task set to wake on a day is back on the board
// for all of it rather than partway through.
var WakeAtLayouts = []string{time.RFC3339, "2006-01-02"}

// ParseWakeAt turns a supplied wake date into an optional instant. An empty or
// whitespace-only value means no date at all.
func ParseWakeAt(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	for _, layout := range WakeAtLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			parsed = parsed.UTC()
			return &parsed, nil
		}
	}
	return nil, fmt.Errorf(
		"%w: wake date %q must be YYYY-MM-DD or an RFC 3339 timestamp",
		ErrInvalid, value,
	)
}

// ParseContextCheckedAt turns an RFC 3339 timestamp into an optional instant.
// Unlike a wake date, a context check describes an event that already happened,
// so a bare calendar date would be needlessly ambiguous.
func ParseContextCheckedAt(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: context checked timestamp %q must be RFC 3339",
			ErrInvalid, value,
		)
	}
	parsed = parsed.UTC()
	return &parsed, nil
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
	// WakeAt starts the task asleep until the date arrives, for work that is
	// captured already waiting on someone.
	WakeAt *time.Time `json:"wake_at,omitempty"`
	// WaitingOn names who or what the new task is waiting for.
	WaitingOn string `json:"waiting_on,omitempty"`
	// OnTimeout is what to do if the wait is unresolved at WakeAt.
	OnTimeout string `json:"on_timeout,omitempty"`
	// Context is an optional, lightweight execution-context tag.
	Context string `json:"context,omitempty"`
	// ContextCheckedAt is when the supporting context was last verified.
	ContextCheckedAt *time.Time `json:"context_checked_at,omitempty"`
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
	Title        *string
	Description  *string
	Dependencies *[]string
	// WakeAt supplies a new wake date when non-nil. A zero time clears the
	// date and returns the task to the board, mirroring how an empty
	// description or dependency list clears those fields.
	WakeAt *time.Time
	// WaitingOn supplies a new one-line reason when non-nil; an empty string
	// clears it.
	WaitingOn *string
	// OnTimeout supplies the default action at the review date when non-nil; an
	// empty string clears it.
	OnTimeout *string
	// Context supplies a new execution-context tag when non-nil; an empty string
	// clears it.
	Context *string
	// ContextCheckedAt supplies a new verification timestamp when non-nil. A
	// zero time clears it.
	ContextCheckedAt *time.Time
	Replacements     []TextReplacement
	ExpectedVersion  *int64
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
	contextName, err := normalizeContext(input.Context)
	if err != nil {
		return Task{}, err
	}
	created := Task{
		Title:        input.Title,
		Description:  input.Description,
		Status:       StatusTodo,
		Position:     position,
		Dependencies: dependencies,
		WaitingOn:    strings.TrimSpace(input.WaitingOn),
		OnTimeout:    strings.TrimSpace(input.OnTimeout),
		Context:      contextName,
		CreatedAt:    now,
		UpdatedAt:    now,
		Version:      1,
	}
	if input.WakeAt != nil && !input.WakeAt.IsZero() {
		wake := input.WakeAt.UTC()
		created.WakeAt = &wake
		// Capturing something already asleep is the first time it was set aside.
		created.SnoozeCount = 1
	}
	if input.ContextCheckedAt != nil && !input.ContextCheckedAt.IsZero() {
		checkedAt := input.ContextCheckedAt.UTC()
		created.ContextCheckedAt = &checkedAt
	}
	if err := validateWait(created); err != nil {
		return Task{}, err
	}
	created.ID = s.newID()
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
		input.WakeAt == nil &&
		input.WaitingOn == nil &&
		input.OnTimeout == nil &&
		input.Context == nil &&
		input.ContextCheckedAt == nil &&
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
	if input.WakeAt != nil {
		if input.WakeAt.IsZero() {
			edited.WakeAt = nil
		} else {
			wake := input.WakeAt.UTC()
			edited.WakeAt = &wake
		}
	}
	if input.WaitingOn != nil {
		edited.WaitingOn = strings.TrimSpace(*input.WaitingOn)
	}
	if input.OnTimeout != nil {
		edited.OnTimeout = strings.TrimSpace(*input.OnTimeout)
	}
	if input.Context != nil {
		contextName, err := normalizeContext(*input.Context)
		if err != nil {
			return Task{}, err
		}
		edited.Context = contextName
	}
	if input.ContextCheckedAt != nil {
		if input.ContextCheckedAt.IsZero() {
			edited.ContextCheckedAt = nil
		} else {
			checkedAt := input.ContextCheckedAt.UTC()
			edited.ContextCheckedAt = &checkedAt
		}
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
	if input.Dependencies != nil || input.WakeAt != nil || input.WaitingOn != nil || input.OnTimeout != nil {
		if err := validateWait(edited); err != nil {
			return Task{}, err
		}
	}
	if edited.Title == current.Title &&
		edited.Description == current.Description &&
		slices.Equal(edited.Dependencies, current.Dependencies) &&
		sameInstant(edited.WakeAt, current.WakeAt) &&
		edited.WaitingOn == current.WaitingOn &&
		edited.OnTimeout == current.OnTimeout &&
		edited.Context == current.Context &&
		sameInstant(edited.ContextCheckedAt, current.ContextCheckedAt) {
		return clone(current), nil
	}

	switch {
	case edited.WakeAt != nil && !sameInstant(edited.WakeAt, current.WakeAt):
		// Every push to a new date is one more time this was set aside.
		edited.SnoozeCount = current.SnoozeCount + 1
	case edited.WakeAt == nil && current.WakeAt != nil:
		// Clearing the date ends the wait, so the tally starts over.
		edited.SnoozeCount = 0
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

// sameInstant compares two optional wake dates, treating "no date" as equal to
// itself so an edit that re-supplies the stored date stays a no-op.
func sameInstant(first, second *time.Time) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Equal(*second)
}

func validateWait(item Task) error {
	hasReview := item.WakeAt != nil || len(item.Dependencies) > 0
	if item.WaitingOn != "" && !hasReview {
		return fmt.Errorf(
			"%w: a task waiting on someone needs a wake date or dependency review trigger",
			ErrInvalid,
		)
	}
	if item.OnTimeout != "" && !hasReview {
		return fmt.Errorf(
			"%w: an on-timeout action needs a wake date or dependency review trigger",
			ErrInvalid,
		)
	}
	return nil
}

func normalizeContext(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimSpace(strings.TrimPrefix(value, "@"))
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%w: context must fit on one line", ErrInvalid)
	}
	if len([]rune(value)) > 64 {
		return "", fmt.Errorf("%w: context must be 64 characters or fewer", ErrInvalid)
	}
	return strings.ToLower(value), nil
}

func clone(item Task) Task {
	item.Dependencies = slices.Clone(item.Dependencies)
	item.Attachments = slices.Clone(item.Attachments)
	if item.DeletedAt != nil {
		deletedAt := *item.DeletedAt
		item.DeletedAt = &deletedAt
	}
	if item.WakeAt != nil {
		wake := *item.WakeAt
		item.WakeAt = &wake
	}
	if item.ContextCheckedAt != nil {
		checkedAt := *item.ContextCheckedAt
		item.ContextCheckedAt = &checkedAt
	}
	return item
}
