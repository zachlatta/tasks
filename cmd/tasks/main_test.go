package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zachlatta/tasks/internal/pgtest"
)

func TestCLIAddQueryAndComplete(t *testing.T) {
	t.Setenv("TASKS_DATABASE_URL", pgtest.URL(t))
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"add", "--description", "Old description", "Test the CLI"}, strings.NewReader(""), &output, &errors); code != 0 {
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
		"edit", "--title", "Edited in the CLI", "--description-file", "-", "--expected-version", "1", id,
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
		!strings.Contains(history, `"actor_kind": "local_user"`) ||
		!strings.Contains(history, `"source": "cli"`) {
		t.Fatalf("history query output = %q", history)
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
	t.Setenv("TASKS_DATABASE_URL", pgtest.URL(t))
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
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"add", "a task"}, strings.NewReader(""), &output, &errors); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errors.String(), "TASKS_DATABASE_URL is required") {
		t.Fatalf("stderr = %q", errors.String())
	}
}

func TestCLIEditRejectsConflictingDescriptionInputs(t *testing.T) {
	t.Setenv("TASKS_DATABASE_URL", pgtest.URL(t))
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
