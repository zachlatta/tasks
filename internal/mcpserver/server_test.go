package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/pgtest"
	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/tasktest"
)

type emptyReader struct{}

func (emptyReader) Query(context.Context, string) (postgres.Result, error) {
	return postgres.Result{}, nil
}

func TestServerIdentity(t *testing.T) {
	t.Parallel()

	repository := tasktest.NewRepository()
	server := New(task.NewService(repository, time.Now, func() string { return "test-id" }), emptyReader{}, "test")
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	serverInfo := session.InitializeResult().ServerInfo
	if serverInfo.Name != "tasks" || serverInfo.Title != "Tasks" {
		t.Fatalf("server info = %#v, want Tasks identity", serverInfo)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	wantNames := []string{"complete_task", "create_task", "edit_task_text", "query_tasks_sql", "update_task"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("tool names = %v, want %v", names, wantNames)
	}
}

func TestToolsUpdateAndEditTaskTextWithoutPostgres(t *testing.T) {
	t.Parallel()

	repository := tasktest.NewRepository()
	service := task.NewService(repository, time.Now, func() string { return "editable" })
	server := New(service, emptyReader{}, "test")
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_task", Arguments: map[string]any{
		"title": "Research", "description": "source and source",
	}}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	updated, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_task", Arguments: map[string]any{
		"id": "editable", "expected_version": 1, "title": "Research sources",
	}})
	if err != nil || updated.IsError {
		t.Fatalf("update task = %#v, %v", updated, err)
	}
	edited, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "edit_task_text", Arguments: map[string]any{
		"id": "editable", "expected_version": 2,
		"edits": []map[string]any{{
			"field": "description", "old_text": "source", "new_text": "primary source", "replace_all": true,
		}},
	}})
	if err != nil || edited.IsError {
		t.Fatalf("edit task text = %#v, %v", edited, err)
	}
	item, err := service.Get(ctx, "editable")
	if err != nil {
		t.Fatalf("get edited task: %v", err)
	}
	if item.Title != "Research sources" || item.Description != "primary source and primary source" || item.Version != 3 {
		t.Fatalf("edited task = %#v", item)
	}

	ambiguous, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "edit_task_text", Arguments: map[string]any{
		"id": "editable",
		"edits": []map[string]any{{
			"field": "description", "old_text": "primary source", "new_text": "document",
		}},
	}})
	if err != nil {
		t.Fatalf("call ambiguous edit: %v", err)
	}
	if !ambiguous.IsError {
		t.Fatal("ambiguous edit succeeded, want tool error")
	}
	unchanged, err := service.Get(ctx, "editable")
	if err != nil {
		t.Fatalf("get task after ambiguous edit: %v", err)
	}
	if unchanged.Description != item.Description || unchanged.Version != item.Version {
		t.Fatalf("ambiguous edit changed task from %#v to %#v", item, unchanged)
	}

	cleared, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_task", Arguments: map[string]any{
		"id": "editable", "expected_version": 3, "description": "", "dependencies": []string{},
	}})
	if err != nil || cleared.IsError {
		t.Fatalf("clear task fields = %#v, %v", cleared, err)
	}
	item, err = service.Get(ctx, "editable")
	if err != nil {
		t.Fatalf("get cleared task: %v", err)
	}
	if item.Description != "" || len(item.Dependencies) != 0 || item.Version != 4 {
		t.Fatalf("cleared task = %#v", item)
	}
}

func TestToolsCreateQueryAndCompleteTasks(t *testing.T) {
	t.Parallel()

	store, err := postgres.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)
	ids := []string{"write-tests", "ship-feature"}
	service := task.NewService(store, time.Now, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	server := New(service, store, "test")
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
	wantNames := []string{"complete_task", "create_task", "edit_task_text", "query_tasks_sql", "update_task"}
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

	updated, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_task", Arguments: map[string]any{
		"id": "ship-feature", "expected_version": 1, "title": "Ship the feature",
		"description": "Write release notes. Publish release notes.",
	}})
	if err != nil || updated.IsError {
		t.Fatalf("update task = %#v, %v", updated, err)
	}
	edited, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "edit_task_text", Arguments: map[string]any{
		"id": "ship-feature", "expected_version": 2,
		"edits": []map[string]any{{
			"field": "description", "old_text": "release notes", "new_text": "launch notes", "replace_all": true,
		}},
	}})
	if err != nil || edited.IsError {
		t.Fatalf("edit task text = %#v, %v", edited, err)
	}
	queryResult, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "query_tasks_sql", Arguments: map[string]any{
		"sql": "SELECT title, description, version FROM tasks WHERE id = 'ship-feature'",
	}})
	if err != nil {
		t.Fatalf("query edited task: %v", err)
	}
	encoded, err = json.Marshal(queryResult.StructuredContent)
	if err != nil {
		t.Fatalf("encode edited query result: %v", err)
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("decode edited query result: %v", err)
	}
	if len(output.Rows) != 1 ||
		output.Rows[0]["title"] != "Ship the feature" ||
		output.Rows[0]["description"] != "Write launch notes. Publish launch notes." ||
		output.Rows[0]["version"] != float64(3) {
		t.Fatalf("edited task query rows = %#v", output.Rows)
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
	historyResult, err := store.Query(ctx, `
		SELECT action, actor_kind, source
		FROM task_revisions
		WHERE task_id = 'ship-feature'
		ORDER BY version
	`)
	if err != nil {
		t.Fatalf("query task history: %v", err)
	}
	if len(historyResult.Rows) != 4 {
		t.Fatalf("history rows = %#v, want create, two edits, and complete", historyResult.Rows)
	}
	for _, row := range historyResult.Rows {
		if row["actor_kind"] != "oauth_client" || row["source"] != "mcp" {
			t.Fatalf("MCP revision attribution = %#v", row)
		}
	}
	wantActions := []string{"create", "edit", "edit", "complete"}
	for index, want := range wantActions {
		if historyResult.Rows[index]["action"] != want {
			t.Fatalf("history actions = %#v, want %v", historyResult.Rows, wantActions)
		}
	}
}
