package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLIAddQueryAndComplete(t *testing.T) {
	const secret = "test-secret"
	var title string
	var description string
	var agentSessionURL string
	var version int64
	var status string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tools/create_task":
			var input struct {
				Title           string `json:"title"`
				Description     string `json:"description"`
				AgentSessionURL string `json:"agent_session_url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode create input: %v", err)
			}
			title, description, agentSessionURL, version, status = input.Title, input.Description, input.AgentSessionURL, 1, "todo"
			_, _ = io.WriteString(w, `{"data":{"id":"test-task","version":1}}`)
		case "/api/tools/update_task":
			var input struct {
				ID              string  `json:"id"`
				Title           *string `json:"title"`
				Description     *string `json:"description"`
				AgentSessionURL *string `json:"agent_session_url"`
				ExpectedVersion *int64  `json:"expected_version"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode update input: %v", err)
			}
			if input.ID != "test-task" || input.ExpectedVersion == nil || *input.ExpectedVersion != version {
				t.Errorf("update input = %#v; current version = %d", input, version)
			}
			title, description, agentSessionURL, version = *input.Title, *input.Description, *input.AgentSessionURL, version+1
			_, _ = io.WriteString(w, `{"data":{"id":"test-task","version":2}}`)
		case "/api/tools/query_tasks_sql":
			var input struct {
				SQL string `json:"sql"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode query input: %v", err)
			}
			if strings.Contains(input.SQL, "task_revisions") {
				_, _ = io.WriteString(w, `{"data":{"columns":["action","actor_kind","source"],"rows":[`+
					`{"action":"create","actor_kind":"shared_secret","source":"cli"},`+
					`{"action":"edit","actor_kind":"shared_secret","source":"cli"},`+
					`{"action":"complete","actor_kind":"shared_secret","source":"cli"}],"truncated":false}}`)
				return
			}
			row, err := json.Marshal(map[string]any{
				"id": "test-task", "status": status, "title": title, "description": description,
				"agent_session_url": agentSessionURL, "version": version,
			})
			if err != nil {
				t.Errorf("encode query row: %v", err)
			}
			_, _ = io.WriteString(w, `{"data":{"columns":["id","status","title","description","version"],"rows":[`+string(row)+`],"truncated":false}}`)
		case "/api/tools/complete_task":
			status = "done"
			version++
			_, _ = io.WriteString(w, `{"data":{"id":"test-task","status":"done","version":3}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", secret)
	t.Setenv("TASKS_DATABASE_URL", "")

	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{
		"add", "--description", "Old description",
		"--agent-session-url", "cmux://workspace/00000000-0000-4000-8000-000000000001",
		"Test the CLI",
	}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("add exit = %d; stderr: %s", code, errors.String())
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != "created" {
		t.Fatalf("add output = %q", output.String())
	}
	id := fields[1]

	output.Reset()
	errors.Reset()
	if code := run([]string{
		"edit", "--title", "Edited in the CLI", "--description-file", "-",
		"--agent-session-url", "cmux://workspace/00000000-0000-4000-8000-000000000002",
		"--expected-version", "1", id,
	}, strings.NewReader("Uploaded description\n"), &output, &errors); code != 0 {
		t.Fatalf("edit exit = %d; stderr: %s", code, errors.String())
	}
	if got := output.String(); got != "edited "+id+" (version 2)\n" {
		t.Fatalf("edit output = %q", got)
	}

	output.Reset()
	errors.Reset()
	if code := run([]string{"query", "SELECT id, status, title, description, version FROM task_overview"}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("query exit = %d; stderr: %s", code, errors.String())
	}
	if !strings.Contains(output.String(), id) ||
		!strings.Contains(output.String(), "todo") ||
		!strings.Contains(output.String(), "Edited in the CLI") ||
		!strings.Contains(output.String(), "Uploaded description\\n") ||
		!strings.Contains(output.String(), "cmux://workspace/00000000-0000-4000-8000-000000000002") ||
		!strings.Contains(output.String(), `"version": 2`) {
		t.Fatalf("query output = %q", output.String())
	}

	output.Reset()
	errors.Reset()
	if code := run([]string{"done", id}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("done exit = %d; stderr: %s", code, errors.String())
	}
	output.Reset()
	if code := run([]string{"query", "SELECT status FROM tasks WHERE id = '" + id + "'"}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("status query exit = %d; stderr: %s", code, errors.String())
	}
	if !strings.Contains(output.String(), `"status": "done"`) {
		t.Fatalf("status query output = %q", output.String())
	}

	output.Reset()
	if code := run([]string{
		"query",
		"SELECT action, actor_kind, source FROM task_revisions WHERE task_id = '" + id + "' ORDER BY version",
	}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("history query exit = %d; stderr: %s", code, errors.String())
	}
	history := output.String()
	if !strings.Contains(history, `"action": "create"`) ||
		!strings.Contains(history, `"action": "edit"`) ||
		!strings.Contains(history, `"action": "complete"`) ||
		!strings.Contains(history, `"actor_kind": "shared_secret"`) ||
		!strings.Contains(history, `"source": "cli"`) {
		t.Fatalf("history query output = %q", history)
	}
}

func TestCLIDeleteAndRestore(t *testing.T) {
	const secret = "test-secret"
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var input struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode input: %v", err)
		}
		if input.ID != "test-task" {
			t.Errorf("id = %q", input.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tools/delete_task":
			deleted = true
			_, _ = io.WriteString(w, `{"data":{"id":"test-task","version":2,"deleted_at":"2026-07-24T12:00:00Z"}}`)
		case "/api/tools/restore_task":
			deleted = false
			_, _ = io.WriteString(w, `{"data":{"id":"test-task","version":3}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", secret)
	t.Setenv("TASKS_DATABASE_URL", "")

	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"delete", "test-task"}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("delete exit = %d; stderr: %s", code, errors.String())
	}
	if got := output.String(); got != "deleted test-task\n" {
		t.Fatalf("delete output = %q", got)
	}
	if !deleted {
		t.Fatal("delete command did not call delete_task")
	}

	output.Reset()
	errors.Reset()
	if code := run([]string{"restore", "test-task"}, strings.NewReader(""), &output, &errors); code != 0 {
		t.Fatalf("restore exit = %d; stderr: %s", code, errors.String())
	}
	if got := output.String(); got != "restored test-task\n" {
		t.Fatalf("restore output = %q", got)
	}
	if deleted {
		t.Fatal("restore command did not call restore_task")
	}

	output.Reset()
	errors.Reset()
	if code := run([]string{"delete"}, strings.NewReader(""), &output, &errors); code != 2 {
		t.Fatalf("delete without an ID exit = %d, want 2", code)
	}
	if !strings.Contains(errors.String(), "Usage: tasks delete <task-id>") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIHasNoListCommand(t *testing.T) {
	// "list" is rejected as an unknown command before any database connection.
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"list"}, strings.NewReader(""), &output, &errors); code != 2 {
		t.Fatalf("list exit = %d, want 2", code)
	}
	if !strings.Contains(errors.String(), `unknown command "list"`) {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIQueryRejectsWrites(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tools/query_tasks_sql" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"tool_error","message":"only read-only queries are allowed"}}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", "test-secret")
	t.Setenv("TASKS_DATABASE_URL", "")

	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"query", "DELETE FROM tasks"}, strings.NewReader(""), &output, &errors); code != 1 {
		t.Fatalf("query exit = %d, want 1", code)
	}
	if !strings.Contains(errors.String(), "only read-only") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIRejectsMissingCommand(t *testing.T) {
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run(nil, strings.NewReader(""), &output, &errors); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errors.String(), "Usage:\n  tasks add") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIUsesTasksConfigurationName(t *testing.T) {
	t.Setenv("TASKS_API_URL", "")
	t.Setenv("TASKS_SECRET", "")
	t.Setenv("TASKS_DATABASE_URL", "postgres://must-not-be-used.example/tasks")
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"add", "a task"}, strings.NewReader(""), &output, &errors); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errors.String(), "TASKS_API_URL is required") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIRequiresSharedSecretInsteadOfDatabaseCredentials(t *testing.T) {
	t.Setenv("TASKS_API_URL", "http://127.0.0.1:8080")
	t.Setenv("TASKS_SECRET", "")
	t.Setenv("TASKS_DATABASE_URL", "postgres://must-not-be-used.example/tasks")
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"add", "a task"}, strings.NewReader(""), &output, &errors); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errors.String(), "TASKS_SECRET is required") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIEditRejectsConflictingDescriptionInputs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("API must not be called for invalid CLI arguments")
	}))
	t.Cleanup(server.Close)
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", "test-secret")
	t.Setenv("TASKS_DATABASE_URL", "")
	var output bytes.Buffer
	var errors bytes.Buffer
	code := run([]string{
		"edit", "--description", "inline", "--description-file", "-", "task-id",
	}, strings.NewReader("stdin"), &output, &errors)
	if code != 2 {
		t.Fatalf("edit exit = %d, want 2; stderr: %s", code, errors.String())
	}
	if !strings.Contains(errors.String(), "--description and --description-file cannot be used together") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIAddAndEditCarryTheWait(t *testing.T) {
	const secret = "test-secret"
	var created struct {
		WakeAt           string `json:"wake_at"`
		WaitingOn        string `json:"waiting_on"`
		OnTimeout        string `json:"on_timeout"`
		Context          string `json:"context"`
		ContextCheckedAt string `json:"context_checked_at"`
	}
	var edited struct {
		WakeAt           *string `json:"wake_at"`
		WaitingOn        *string `json:"waiting_on"`
		OnTimeout        *string `json:"on_timeout"`
		Context          *string `json:"context"`
		ContextCheckedAt *string `json:"context_checked_at"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tools/create_task":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create input: %v", err)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"waiting-task","version":1}}`)
		case "/api/tools/update_task":
			if err := json.NewDecoder(r.Body).Decode(&edited); err != nil {
				t.Errorf("decode update input: %v", err)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"waiting-task","version":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", secret)

	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"add",
		"--waiting-on", "the reviewers",
		"--wake-at", "2026-07-27",
		"--on-timeout", "Proceed with the reviewers who replied",
		"--context", "office",
		"--context-checked-at", "2026-07-24T13:30:00Z",
		"Chase the sign-off",
	}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("add exit code = %d; stderr: %s", code, stderr.String())
	}
	if created.WakeAt != "2026-07-27" ||
		created.WaitingOn != "the reviewers" ||
		created.OnTimeout != "Proceed with the reviewers who replied" ||
		created.Context != "office" ||
		created.ContextCheckedAt != "2026-07-24T13:30:00Z" {
		t.Fatalf("add sent %#v", created)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{
		"edit",
		"--wake-at", "",
		"--waiting-on", "",
		"--on-timeout", "",
		"--context", "",
		"--context-checked-at", "",
		"waiting-task",
	}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("edit exit code = %d; stderr: %s", code, stderr.String())
	}
	if edited.WakeAt == nil || *edited.WakeAt != "" ||
		edited.WaitingOn == nil || *edited.WaitingOn != "" ||
		edited.OnTimeout == nil || *edited.OnTimeout != "" ||
		edited.Context == nil || *edited.Context != "" ||
		edited.ContextCheckedAt == nil || *edited.ContextCheckedAt != "" {
		t.Fatalf("edit sent %#v", edited)
	}
}

func TestCLIEditAcceptsTheWaitAlone(t *testing.T) {
	const secret = "test-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":"waiting-task","version":2}}`)
	}))
	defer server.Close()
	t.Setenv("TASKS_API_URL", server.URL)
	t.Setenv("TASKS_SECRET", secret)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"edit", "--wake-at", "2026-08-03", "waiting-task"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("edit exit code = %d; stderr: %s", code, stderr.String())
	}
}
