package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/codex2api/internal/diag"
)

func (db *DB) ensureDiagnosticSchema(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS diagnostic_settings (id INTEGER PRIMARY KEY, config TEXT NOT NULL)`)
	return err
}

func (db *DB) LoadDiagnosticConfig(ctx context.Context) (diag.ServerConfig, error) {
	cfg := diag.DefaultServerConfig()
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT config FROM diagnostic_settings WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, errors.New("诊断设置损坏，请重新保存设置")
	}
	cfg.APIKey = decryptCredentialValue("diagnostic_api_key", cfg.APIKey)
	cfg.GitHubToken = decryptCredentialValue("diagnostic_github_token", cfg.GitHubToken)
	if strings.HasPrefix(cfg.APIKey, credEncPrefix) || strings.HasPrefix(cfg.GitHubToken, credEncPrefix) {
		return diag.DefaultServerConfig(), errors.New("无法解密诊断凭据，请检查现有凭据加密密钥")
	}
	return cfg.Normalized(), nil
}

func (db *DB) SaveDiagnosticConfig(ctx context.Context, cfg diag.ServerConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.APIKey = encryptCredentialValue("diagnostic_api_key", cfg.APIKey)
	cfg.GitHubToken = encryptCredentialValue("diagnostic_github_token", cfg.GitHubToken)
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `INSERT INTO diagnostic_settings(id,config) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET config=excluded.config`, string(data))
		if err != nil {
			return fmt.Errorf("保存诊断设置失败: %w", err)
		}
		return nil
	})
}
