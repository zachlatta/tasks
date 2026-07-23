package main

import (
	"strings"
	"testing"
)

func TestRenderFormulaIncludesEachHomebrewPlatform(t *testing.T) {
	checksums := strings.NewReader(`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  tasks_edge-SNAPSHOT-abc1234_darwin_amd64.tar.gz
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  tasks_edge-SNAPSHOT-abc1234_darwin_arm64.tar.gz
cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  tasks_edge-SNAPSHOT-abc1234_linux_amd64.tar.gz
dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd  tasks_edge-SNAPSHOT-abc1234_linux_arm64.tar.gz
eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee  tasks_edge-SNAPSHOT-abc1234_windows_amd64.zip
`)

	formula, err := renderFormula("zachlatta/tasks", "0.0.0.42", "edge-SNAPSHOT-abc1234", checksums)
	if err != nil {
		t.Fatalf("renderFormula: %v", err)
	}

	for _, expected := range []string{
		`class Tasks < Formula`,
		`version "0.0.0.42"`,
		`homepage "https://github.com/zachlatta/tasks"`,
		`url "https://github.com/zachlatta/tasks/releases/download/edge/tasks_edge-SNAPSHOT-abc1234_darwin_amd64.tar.gz"`,
		`sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
		`url "https://github.com/zachlatta/tasks/releases/download/edge/tasks_edge-SNAPSHOT-abc1234_darwin_arm64.tar.gz"`,
		`sha256 "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`,
		`url "https://github.com/zachlatta/tasks/releases/download/edge/tasks_edge-SNAPSHOT-abc1234_linux_amd64.tar.gz"`,
		`sha256 "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"`,
		`url "https://github.com/zachlatta/tasks/releases/download/edge/tasks_edge-SNAPSHOT-abc1234_linux_arm64.tar.gz"`,
		`sha256 "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"`,
		`bin.install "tasks"`,
		`assert_equal "edge-SNAPSHOT-abc1234\n", shell_output("#{bin}/tasks version")`,
	} {
		if !strings.Contains(formula, expected) {
			t.Errorf("formula missing %q:\n%s", expected, formula)
		}
	}
	if strings.Contains(formula, "windows") {
		t.Fatalf("formula unexpectedly includes Windows archive:\n%s", formula)
	}
}

func TestRenderFormulaRequiresEveryHomebrewPlatform(t *testing.T) {
	checksums := strings.NewReader(`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  tasks_edge-SNAPSHOT-abc1234_darwin_amd64.tar.gz
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  tasks_edge-SNAPSHOT-abc1234_darwin_arm64.tar.gz
cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  tasks_edge-SNAPSHOT-abc1234_linux_amd64.tar.gz
`)

	_, err := renderFormula("zachlatta/tasks", "0.0.0.42", "edge-SNAPSHOT-abc1234", checksums)
	if err == nil || !strings.Contains(err.Error(), "linux_arm64") {
		t.Fatalf("renderFormula error = %v, want missing linux_arm64 archive", err)
	}
}
