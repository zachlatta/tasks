package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryTrustedJobUsesDistinctRotomRunner(t *testing.T) {
	t.Parallel()

	for name, runner := range map[string]string{
		"edge-release.yml": "runs-on: rotom-builder-tasks-edge-release",
		"release.yml":      "runs-on: rotom-builder-tasks-stable-release",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workflow := readWorkflow(t, name)
			if !strings.Contains(workflow, runner) {
				t.Fatalf("%s must use its distinct Rotom runner", name)
			}
		})
	}

	ci := readWorkflow(t, "ci.yml")
	trustedCIRunner := `runs-on: >-
      ${{ github.event_name == 'pull_request' &&
          github.event.pull_request.head.repo.full_name != github.repository &&
          'ubuntu-latest' || 'rotom-builder-tasks-ci' }}`
	if !strings.Contains(ci, trustedCIRunner) {
		t.Fatal("trusted CI must use its distinct Rotom runner")
	}
}

func TestForkPullRequestCIRemainsGitHubHosted(t *testing.T) {
	t.Parallel()

	workflow := readWorkflow(t, "ci.yml")
	if !strings.Contains(workflow, "'ubuntu-latest' ||") {
		t.Fatal("public pull-request CI must remain GitHub-hosted")
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
