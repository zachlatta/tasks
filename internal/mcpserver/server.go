package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/task"
)

// Reader runs trusted, read-only SQL against the task tables for agents.
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

func New(tasks *task.Service, reader Reader, version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "tasks",
		Title:   "Tasks",
		Version: version,
	}, nil)
	closedWorld := false
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query_tasks_sql",
		Title:       "Query tasks with read-only SQL",
		Description: "Runs trusted, read-only PostgreSQL queries. Tables: tasks, dependencies, images, task_revisions. View: task_overview. Results are capped at 500 rows. Inspect the schema via information_schema.columns.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input SQLQueryInput) (*mcp.CallToolResult, SQLQueryOutput, error) {
		result, err := reader.Query(ctx, input.SQL)
		if err != nil {
			return nil, SQLQueryOutput{}, err
		}
		return nil, SQLQueryOutput{Columns: result.Columns, Rows: result.Rows, Truncated: result.Truncated}, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_task",
		Title:       "Create a task",
		Description: "Creates a todo task in the shared Postgres backend. Dependencies must name existing task IDs.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input CreateTaskInput) (*mcp.CallToolResult, task.Task, error) {
		created, err := tasks.Create(mutationContext(ctx), task.CreateInput{
			Title: input.Title, Description: input.Description, Dependencies: input.Dependencies,
		})
		return nil, created, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "complete_task",
		Title:       "Complete a task",
		Description: "Marks a task done after all of its dependencies are done.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input CompleteTaskInput) (*mcp.CallToolResult, task.Task, error) {
		completed, err := tasks.Complete(mutationContext(ctx), input.ID)
		return nil, completed, err
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
