package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/taskapi"
)

// Reader runs trusted, read-only SQL against the task tables for agents.
type Reader = taskapi.Reader

type SQLQueryInput = taskapi.SQLQueryInput
type SQLQueryOutput = taskapi.SQLQueryOutput
type CreateTaskInput = taskapi.CreateTaskInput
type CompleteTaskInput = taskapi.CompleteTaskInput
type DeleteTaskInput = taskapi.DeleteTaskInput
type RestoreTaskInput = taskapi.RestoreTaskInput
type UpdateTaskInput = taskapi.UpdateTaskInput
type EditTaskTextInput = taskapi.EditTaskTextInput

func New(tasks *task.Service, reader Reader, version string) *mcp.Server {
	return NewWithTools(taskapi.NewTools(tasks, reader), version)
}

// NewWithTools exposes the same transport-neutral operations used by the
// CLI-facing HTTP API through MCP.
func NewWithTools(tools *taskapi.Tools, version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tasks",
		Title:   "Tasks",
		Version: version,
	}, nil)
	closedWorld := false
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.QueryTasksSQLTool,
		Title: "Query tasks with read-only SQL",
		Description: "Runs trusted, read-only PostgreSQL queries (SELECT, WITH, or EXPLAIN; results capped at 500 rows; 5-second statement timeout).\n" +
			taskapi.SchemaReference(),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input SQLQueryInput) (*mcp.CallToolResult, SQLQueryOutput, error) {
		output, err := tools.QueryTasksSQL(ctx, input)
		return nil, output, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.CreateTaskTool,
		Title: "Create a task",
		Description: "Creates a todo task in the shared Postgres backend. Dependencies must name existing task IDs. " +
			"Set waiting_on with a wake_at (or dependency), and optionally on_timeout, to capture a complete wait. " +
			"Context is a lightweight tag such as home, office, or online; context_checked_at records an explicit verification. " +
			"agent_session_url may link the task to an exact cmux://workspace/<uuid> agent workspace.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input CreateTaskInput) (*mcp.CallToolResult, task.Task, error) {
		created, err := tools.CreateTask(mutationContext(ctx), input)
		return nil, created, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.EditTaskTextTool,
		Title: "Edit task text with guarded replacements",
		Description: "Atomically edits title or description text with exact old_text/new_text replacements. " +
			"Each old_text must occur exactly once unless replace_all is true; missing or ambiguous context fails without changing the task. " +
			"Use query_tasks_sql to read the current text and version first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true), OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input EditTaskTextInput) (*mcp.CallToolResult, task.Task, error) {
		edited, err := tools.EditTaskText(mutationContext(ctx), input)
		return nil, edited, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.UpdateTaskTool,
		Title: "Replace task fields",
		Description: "Atomically replaces any supplied mutable fields: title, description, the complete dependency list, " +
			"an exact Cmux agent-session URL, the complete wait (wake_at, waiting_on, and on_timeout), and/or execution-context metadata. " +
			"Omitted fields stay unchanged; supplied empty values clear their fields. " +
			"A named wait must retain a wake date or dependency review trigger. " +
			"A task with a future wake_at or an unfinished dependency is held off the board until it wakes. " +
			"Status and attachments are preserved. Use query_tasks_sql to read the current task and version first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true), OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input UpdateTaskInput) (*mcp.CallToolResult, task.Task, error) {
		edited, err := tools.UpdateTask(mutationContext(ctx), input)
		return nil, edited, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        taskapi.CompleteTaskTool,
		Title:       "Complete a task",
		Description: "Marks a task done after all of its dependencies are done.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input CompleteTaskInput) (*mcp.CallToolResult, task.Task, error) {
		completed, err := tools.CompleteTask(mutationContext(ctx), input)
		return nil, completed, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.DeleteTaskTool,
		Title: "Delete a task",
		Description: "Soft-deletes a task: it leaves the board and every read, while its row and complete revision history stay in the database. " +
			"restore_task brings it back with its status, order, text, dependencies, and files intact. " +
			"A task other live tasks still depend on cannot be deleted; delete or re-point those first. " +
			"Deleted tasks are absent from task_overview; find them with SELECT id, title, deleted_at FROM tasks WHERE deleted_at IS NOT NULL.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(true), IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input DeleteTaskInput) (*mcp.CallToolResult, task.Task, error) {
		deleted, err := tools.DeleteTask(mutationContext(ctx), input)
		return nil, deleted, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:  taskapi.RestoreTaskTool,
		Title: "Restore a deleted task",
		Description: "Returns a soft-deleted task to the board unchanged. " +
			"A task whose own dependencies are still deleted cannot be restored until they are.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input RestoreTaskInput) (*mcp.CallToolResult, task.Task, error) {
		restored, err := tools.RestoreTask(mutationContext(ctx), input)
		return nil, restored, err
	})
	return server
}

func mutationContext(ctx context.Context) context.Context {
	clientID, _ := auth.ClientIDFromContext(ctx)
	return task.WithAuditMetadata(ctx, task.AuditMetadata{
		ActorKind: "oauth_client",
		ActorID:   clientID,
		Source:    "mcp",
	})
}

func boolPointer(value bool) *bool {
	return &value
}
