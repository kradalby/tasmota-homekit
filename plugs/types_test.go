package plugs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigValidatesPlugs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.hujson")
	if err := os.WriteFile(path, []byte(`{"plugs":[{"id":"a","name":"A","address":"1"}]}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(cfg.Plugs) != 1 {
		t.Fatalf("expected 1 plug, got %d", len(cfg.Plugs))
	}
}

func TestLoadConfigRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.hujson")
	if err := os.WriteFile(path, []byte(`{"plugs":[]}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestLoadConfigRejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dupe.hujson")
	payload := `{"plugs":[{"id":"a","name":"A","address":"1"},{"id":"a","name":"B","address":"2"}]}`
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for duplicate IDs")
	}
}
