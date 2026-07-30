// Package taskapi exposes the task operations shared by the MCP server and
// the CLI-facing HTTP API, plus the HTTP client used by the tasks CLI.
package taskapi

import (
	"context"
	"time"

	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/task"
)

const (
	QueryTasksSQLTool = "query_tasks_sql"
	CreateTaskTool    = "create_task"
	EditTaskTextTool  = "edit_task_text"
	UpdateTaskTool    = "update_task"
	CompleteTaskTool  = "complete_task"
	DeleteTaskTool    = "delete_task"
	RestoreTaskTool   = "restore_task"
)

// Reader runs trusted, read-only SQL against the task tables.
type Reader interface {
	Query(ctx context.Context, statement string) (postgres.Result, error)
}

type SQLQueryInput struct {
	SQL string `json:"sql" jsonschema:"A read-only PostgreSQL SELECT, WITH, or EXPLAIN query against the task schema."`
}

type SQLQueryOutput struct {
	Columns   []string         `json:"columns" jsonschema:"Column names in result order."`
	Rows      []map[string]any `json:"rows" jsonschema:"Rows keyed by column name."`
	Truncated bool             `json:"truncated" jsonschema:"Whether more rows existed beyond the server limit."`
}

type CreateTaskInput struct {
	Title            string   `json:"title" jsonschema:"Short, required title for the task."`
	Description      string   `json:"description,omitempty" jsonschema:"Optional Markdown task description."`
	AgentSessionURL  string   `json:"agent_session_url,omitempty" jsonschema:"Optional exact cmux://workspace/<uuid> link to the agent session where the work is happening."`
	Dependencies     []string `json:"dependencies,omitempty" jsonschema:"IDs of tasks that must be done first."`
	WakeAt           string   `json:"wake_at,omitempty" jsonschema:"Optional date the task should return to the board, as YYYY-MM-DD (midnight UTC) or an RFC 3339 timestamp. Until then the task is captured but held off the board."`
	WaitingOn        string   `json:"waiting_on,omitempty" jsonschema:"Optional one-line note naming who or what the task is waiting for. A named wait must also have wake_at or a dependency review trigger."`
	OnTimeout        string   `json:"on_timeout,omitempty" jsonschema:"Optional default action to take if the wait is unresolved at wake_at."`
	Context          string   `json:"context,omitempty" jsonschema:"Optional lightweight execution-context tag such as home, office, or online. A leading @ is optional."`
	ContextCheckedAt string   `json:"context_checked_at,omitempty" jsonschema:"Optional RFC 3339 timestamp recording when the task's supporting context was last verified."`
}

type CompleteTaskInput struct {
	ID string `json:"id" jsonschema:"ID of the task to complete."`
}

type DeleteTaskInput struct {
	ID string `json:"id" jsonschema:"ID of the task to delete. The deletion is soft: the row and its history stay and restore_task brings the task back."`
}

type RestoreTaskInput struct {
	ID string `json:"id" jsonschema:"ID of the deleted task to restore to the board."`
}

type UpdateTaskInput struct {
	ID               string    `json:"id" jsonschema:"ID of the task to update."`
	ExpectedVersion  *int64    `json:"expected_version,omitempty" jsonschema:"Optional version from a prior read. The edit fails instead of overwriting a newer task when it does not match."`
	Title            *string   `json:"title,omitempty" jsonschema:"Optional complete replacement title. Whitespace is trimmed and the result must not be blank."`
	Description      *string   `json:"description,omitempty" jsonschema:"Optional complete replacement Markdown description. An empty string clears it."`
	AgentSessionURL  *string   `json:"agent_session_url,omitempty" jsonschema:"Optional exact cmux://workspace/<uuid> link to the agent session where the work is happening. An empty string clears it."`
	Dependencies     *[]string `json:"dependencies,omitempty" jsonschema:"Optional complete replacement dependency ID list. An empty list clears all dependencies."`
	WakeAt           *string   `json:"wake_at,omitempty" jsonschema:"Optional date the task should return to the board, as YYYY-MM-DD (midnight UTC) or an RFC 3339 timestamp. An empty string clears it and wakes the task now."`
	WaitingOn        *string   `json:"waiting_on,omitempty" jsonschema:"Optional one-line note naming who or what the task is waiting for. An empty string clears it."`
	OnTimeout        *string   `json:"on_timeout,omitempty" jsonschema:"Optional default action if the wait is unresolved at wake_at. An empty string clears it."`
	Context          *string   `json:"context,omitempty" jsonschema:"Optional lightweight execution-context tag such as home, office, or online. A leading @ is optional and an empty string clears it."`
	ContextCheckedAt *string   `json:"context_checked_at,omitempty" jsonschema:"Optional RFC 3339 timestamp recording when supporting context was last verified. An empty string clears it."`
}

type EditTaskTextInput struct {
	ID              string                 `json:"id" jsonschema:"ID of the task whose text should be edited."`
	ExpectedVersion *int64                 `json:"expected_version,omitempty" jsonschema:"Optional version from a prior read. The edit fails instead of overwriting a newer task when it does not match."`
	Edits           []task.TextReplacement `json:"edits" jsonschema:"One or more exact replacements, applied in order and committed atomically."`
}

// Tools is the transport-neutral implementation behind both MCP tools and the
// CLI-facing HTTP API.
type Tools struct {
	tasks  *task.Service
	reader Reader
}

func NewTools(tasks *task.Service, reader Reader) *Tools {
	return &Tools{tasks: tasks, reader: reader}
}

func (t *Tools) QueryTasksSQL(ctx context.Context, input SQLQueryInput) (SQLQueryOutput, error) {
	result, err := t.reader.Query(ctx, input.SQL)
	if err != nil {
		return SQLQueryOutput{}, err
	}
	return SQLQueryOutput{Columns: result.Columns, Rows: result.Rows, Truncated: result.Truncated}, nil
}

func (t *Tools) CreateTask(ctx context.Context, input CreateTaskInput) (task.Task, error) {
	wakeAt, err := task.ParseWakeAt(input.WakeAt)
	if err != nil {
		return task.Task{}, err
	}
	contextCheckedAt, err := task.ParseContextCheckedAt(input.ContextCheckedAt)
	if err != nil {
		return task.Task{}, err
	}
	return t.tasks.Create(ctx, task.CreateInput{
		Title:            input.Title,
		Description:      input.Description,
		AgentSessionURL:  input.AgentSessionURL,
		Dependencies:     input.Dependencies,
		WakeAt:           wakeAt,
		WaitingOn:        input.WaitingOn,
		OnTimeout:        input.OnTimeout,
		Context:          input.Context,
		ContextCheckedAt: contextCheckedAt,
	})
}

func (t *Tools) EditTaskText(ctx context.Context, input EditTaskTextInput) (task.Task, error) {
	return t.tasks.Edit(ctx, input.ID, task.EditInput{
		Replacements: input.Edits, ExpectedVersion: input.ExpectedVersion,
	})
}

func (t *Tools) UpdateTask(ctx context.Context, input UpdateTaskInput) (task.Task, error) {
	edit := task.EditInput{
		Title:           input.Title,
		Description:     input.Description,
		AgentSessionURL: input.AgentSessionURL,
		Dependencies:    input.Dependencies,
		WaitingOn:       input.WaitingOn,
		OnTimeout:       input.OnTimeout,
		Context:         input.Context,
		ExpectedVersion: input.ExpectedVersion,
	}
	if input.WakeAt != nil {
		// A supplied empty string clears the date, which task.EditInput spells
		// as the zero time.
		wakeAt, err := task.ParseWakeAt(*input.WakeAt)
		if err != nil {
			return task.Task{}, err
		}
		if wakeAt == nil {
			wakeAt = new(time.Time)
		}
		edit.WakeAt = wakeAt
	}
	if input.ContextCheckedAt != nil {
		contextCheckedAt, err := task.ParseContextCheckedAt(*input.ContextCheckedAt)
		if err != nil {
			return task.Task{}, err
		}
		if contextCheckedAt == nil {
			contextCheckedAt = new(time.Time)
		}
		edit.ContextCheckedAt = contextCheckedAt
	}
	return t.tasks.Edit(ctx, input.ID, edit)
}

func (t *Tools) CompleteTask(ctx context.Context, input CompleteTaskInput) (task.Task, error) {
	return t.tasks.Complete(ctx, input.ID)
}

func (t *Tools) DeleteTask(ctx context.Context, input DeleteTaskInput) (task.Task, error) {
	return t.tasks.Delete(ctx, input.ID)
}

func (t *Tools) RestoreTask(ctx context.Context, input RestoreTaskInput) (task.Task, error) {
	return t.tasks.Restore(ctx, input.ID)
}
