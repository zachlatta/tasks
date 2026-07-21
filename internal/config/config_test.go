package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesEnvironmentBeforeDotEnv(t *testing.T) {
	directory := t.TempDir()
	dotenv := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotenv, []byte("TASK_TRACKER_SECRET=from-file\nTASK_TRACKER_ADDR=127.0.0.1:7000\nTASK_TRACKER_DATA_DIR="+filepath.Join(directory, "data")+"\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("TASK_TRACKER_ADDR", "127.0.0.1:9000")

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
		"missing secret":      {PublicURL: "http://127.0.0.1:8080"},
		"insecure remote URL": {Secret: "secret", PublicURL: "http://tasks.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := candidate.ValidateServer(); err == nil {
				t.Fatal("ValidateServer succeeded, want error")
			}
		})
	}
}
