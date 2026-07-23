package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsToHostedPublicURL(t *testing.T) {
	previous, existed := os.LookupEnv("TASKS_PUBLIC_URL")
	if err := os.Unsetenv("TASKS_PUBLIC_URL"); err != nil {
		t.Fatalf("unset TASKS_PUBLIC_URL: %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("TASKS_PUBLIC_URL", previous)
		} else {
			_ = os.Unsetenv("TASKS_PUBLIC_URL")
		}
	})

	loaded, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.PublicURL != "https://tasks.hackclub.com" {
		t.Fatalf("PublicURL = %q, want hosted URL", loaded.PublicURL)
	}
	if filepath.Base(loaded.DataDir) != "tasks" {
		t.Fatalf("DataDir = %q, want tasks config directory", loaded.DataDir)
	}
}

func TestLoadUsesEnvironmentBeforeDotEnv(t *testing.T) {
	directory := t.TempDir()
	dotenv := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotenv, []byte("TASKS_SECRET=from-file\nTASKS_ADDR=127.0.0.1:7000\nTASKS_DATABASE_URL=postgres://localhost:5432/tasks\nTASKS_DATA_DIR="+filepath.Join(directory, "data")+"\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("TASKS_ADDR", "127.0.0.1:9000")

	loaded, err := Load(dotenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Secret != "from-file" || loaded.Address != "127.0.0.1:9000" {
		t.Fatalf("config = %#v", loaded)
	}
	if err := loaded.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
}

func TestValidateServerRequiresSecretAndSafePublicURL(t *testing.T) {
	for name, candidate := range map[string]Config{
		"missing secret":      {DatabaseURL: "postgres://localhost/tasks", PublicURL: "http://127.0.0.1:8080"},
		"missing database":    {Secret: "secret", PublicURL: "http://127.0.0.1:8080"},
		"insecure remote URL": {Secret: "secret", DatabaseURL: "postgres://localhost/tasks", PublicURL: "http://tasks.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := candidate.ValidateServer(); err == nil {
				t.Fatal("ValidateServer succeeded, want error")
			}
		})
	}
}
