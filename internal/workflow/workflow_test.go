package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedReleaseWorkflowsUseRotomBuilder(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"edge-release.yml", "release.yml"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workflow := readWorkflow(t, name)
			if !strings.Contains(
				workflow,
				"runs-on: [self-hosted, linux, x64, rotom-builder]",
			) {
				t.Fatalf("%s must use the isolated Rotom builder", name)
			}
		})
	}
}

func TestPullRequestCIRemainsGitHubHosted(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, "ci.yml")
	if !strings.Contains(workflow, "runs-on: ubuntu-latest") {
		t.Fatal("public pull-request CI must remain GitHub-hosted")
	}
	if strings.Contains(workflow, "rotom-builder") {
		t.Fatal("public pull-request CI must never target the Rotom builder")
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", name)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}
