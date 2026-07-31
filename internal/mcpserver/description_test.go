package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/tasktest"
)

// Agents used to burn calls discovering the schema through
// information_schema, and still guessed columns wrong. The SQL tool's own
// description carries the complete schema so a session can write a correct
// query on the first try.
func TestQueryToolDescriptionCarriesTheSchema(t *testing.T) {
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

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var description string
	for _, tool := range tools.Tools {
		if tool.Name == "query_tasks_sql" {
			description = tool.Description
		}
	}
	if description == "" {
		t.Fatal("query_tasks_sql has no description")
	}
	for _, want := range []string{
		// Every readable relation is named with its full column list.
		"task_overview", "tasks", "dependencies", "images", "task_revisions",
		"depends_on", "revision_id", "before_state", "sleeping", "snooze_count",
		"deleted_at", "context_checked_at",
		// The semantics agents need to query the board correctly.
		"live tasks only", "sleeping = 0", "ORDER BY position", "expected_version",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("query_tasks_sql description is missing %q", want)
		}
	}
}
