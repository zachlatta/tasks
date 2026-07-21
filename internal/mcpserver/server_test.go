package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/task-tracker/internal/markdown"
	"github.com/zachlatta/task-tracker/internal/query"
	"github.com/zachlatta/task-tracker/internal/task"
)

func TestToolsCreateQueryAndCompleteTasks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ids := []string{"write-tests", "ship-feature"}
	service := task.NewService(markdown.NewStore(filepath.Join(root, "tasks")), time.Now, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	readModel, err := query.Open(filepath.Join(root, "tasks.db"))
	if err != nil {
		t.Fatalf("open read model: %v", err)
	}
	t.Cleanup(func() { readModel.Close() })
	server := New(service, readModel, "test")
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	wantNames := []string{"complete_task", "create_task", "query_tasks_sql"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tool names = %v, want %v", names, wantNames)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_task", Arguments: map[string]any{"title": "Write tests"}}); err != nil {
		t.Fatalf("create first task: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_task", Arguments: map[string]any{
		"title": "Ship feature", "dependencies": []string{"write-tests"},
	}}); err != nil {
		t.Fatalf("create dependent task: %v", err)
	}
	queryResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "query_tasks_sql", Arguments: map[string]any{
		"sql": "SELECT id, status, blocked FROM task_overview ORDER BY id",
	}})
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	encoded, err := json.Marshal(queryResult.StructuredContent)
	if err != nil {
		t.Fatalf("encode query result: %v", err)
	}
	var output SQLQueryOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("decode query result: %v", err)
	}
	if len(output.Rows) != 2 {
		t.Fatalf("query rows = %#v, want 2", output.Rows)
	}

	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "complete_task", Arguments: map[string]any{"id": "ship-feature"}})
	if err != nil {
		t.Fatalf("call blocked complete tool: %v", err)
	}
	if !blocked.IsError {
		t.Fatal("blocked completion succeeded, want tool error")
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "complete_task", Arguments: map[string]any{"id": "write-tests"}}); err != nil {
		t.Fatalf("complete dependency: %v", err)
	}
	completed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "complete_task", Arguments: map[string]any{"id": "ship-feature"}})
	if err != nil || completed.IsError {
		t.Fatalf("complete dependent task = %#v, %v", completed, err)
	}
}
