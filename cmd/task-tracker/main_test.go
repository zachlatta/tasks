package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIAddListAndComplete(t *testing.T) {
	t.Setenv("TASK_TRACKER_DATA_DIR", t.TempDir())
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run([]string{"add", "Test the CLI"}, &output, &errors); code != 0 {
		t.Fatalf("add exit = %d; stderr: %s", code, errors.String())
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != "created" {
		t.Fatalf("add output = %q", output.String())
	}
	id := fields[1]

	output.Reset()
	errors.Reset()
	if code := run([]string{"list"}, &output, &errors); code != 0 {
		t.Fatalf("list exit = %d; stderr: %s", code, errors.String())
	}
	if !strings.Contains(output.String(), id) || !strings.Contains(output.String(), "todo") || !strings.Contains(output.String(), "Test the CLI") {
		t.Fatalf("list output = %q", output.String())
	}

	output.Reset()
	errors.Reset()
	if code := run([]string{"done", id}, &output, &errors); code != 0 {
		t.Fatalf("done exit = %d; stderr: %s", code, errors.String())
	}
	output.Reset()
	if code := run([]string{"list", "--json"}, &output, &errors); code != 0 {
		t.Fatalf("JSON list exit = %d; stderr: %s", code, errors.String())
	}
	if !strings.Contains(output.String(), `"status": "done"`) {
		t.Fatalf("JSON list output = %q", output.String())
	}
}

func TestCLIRejectsMissingCommand(t *testing.T) {
	var output bytes.Buffer
	var errors bytes.Buffer
	if code := run(nil, &output, &errors); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errors.String(), "Usage:") {
		t.Fatalf("stderr = %q", errors.String())
	}
}
