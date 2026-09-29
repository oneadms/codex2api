package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/internal/diag"
)

func TestDiagnosticSettingsEncryptedPersistence(t *testing.T) {
	setCredEncryptionKeyForTest("diagnostic-test-key")
	t.Cleanup(func() { setCredEncryptionKeyForTest("") })
	path := filepath.Join(t.TempDir(), "diagnostics.sqlite")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg, err := db.LoadDiagnosticConfig(t.Context())
	if err != nil || cfg != diag.DefaultServerConfig() {
		t.Fatalf("defaults: %v", err)
	}
	cfg.APIKey, cfg.GitHubToken = "model-secret-value", "github-secret-value"
	if err = db.SaveDiagnosticConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = db.conn.QueryRowContext(t.Context(), "SELECT config FROM diagnostic_settings WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "secret-value") || strings.Count(raw, credEncPrefix) != 2 {
		t.Fatal("credentials not encrypted at rest")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadDiagnosticConfig(t.Context())
	if err != nil || loaded != cfg {
		t.Fatalf("database restart lost settings: %v", err)
	}
	setCredEncryptionKeyForTest("wrong-key")
	if _, err = db.LoadDiagnosticConfig(t.Context()); err == nil {
		t.Fatal("undecryptable credentials accepted")
	}
}
