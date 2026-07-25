// Package taskapi exposes the task operations shared by the MCP server and
// the CLI-facing HTTP API, plus the HTTP client used by the tasks CLI.
package taskapi

import (
	"context"

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
	Title        string   `json:"title" jsonschema:"Short, required title for the task."`
	Description  string   `json:"description,omitempty" jsonschema:"Optional Markdown task description."`
	Dependencies []string `json:"dependencies,omitempty" jsonschema:"IDs of tasks that must be done first."`
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
	ID              string    `json:"id" jsonschema:"ID of the task to update."`
	ExpectedVersion *int64    `json:"expected_version,omitempty" jsonschema:"Optional version from a prior read. The edit fails instead of overwriting a newer task when it does not match."`
	Title           *string   `json:"title,omitempty" jsonschema:"Optional complete replacement title. Whitespace is trimmed and the result must not be blank."`
	Description     *string   `json:"description,omitempty" jsonschema:"Optional complete replacement Markdown description. An empty string clears it."`
	Dependencies    *[]string `json:"dependencies,omitempty" jsonschema:"Optional complete replacement dependency ID list. An empty list clears all dependencies."`
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
	return t.tasks.Create(ctx, task.CreateInput{
		Title: input.Title, Description: input.Description, Dependencies: input.Dependencies,
	})
}

func (t *Tools) EditTaskText(ctx context.Context, input EditTaskTextInput) (task.Task, error) {
	return t.tasks.Edit(ctx, input.ID, task.EditInput{
		Replacements: input.Edits, ExpectedVersion: input.ExpectedVersion,
	})
}

func (t *Tools) UpdateTask(ctx context.Context, input UpdateTaskInput) (task.Task, error) {
	return t.tasks.Edit(ctx, input.ID, task.EditInput{
		Title:           input.Title,
		Description:     input.Description,
		Dependencies:    input.Dependencies,
		ExpectedVersion: input.ExpectedVersion,
	})
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
