package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_NativePassthrough(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
native-passthrough: true
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if !cfg.NativePassthrough {
		t.Fatal("NativePassthrough = false, want true")
	}
}

func TestLoadConfigOptional_NativePassthroughDefaultFalse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.NativePassthrough {
		t.Fatal("NativePassthrough = true, want false")
	}
}
