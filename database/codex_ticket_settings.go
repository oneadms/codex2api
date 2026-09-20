package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// 后台自动打票配置独立读写，通用设置的整行保存不会覆盖它（同 traecn_config 的做法）。
// 打票是后台行为，配置面比业务设置窄得多，因此单独一个 JSON 列最省事：既不必挤进
// SaveSettings 的巨型 UPSERT，也不会被管理端误改。
func (db *DB) LoadCodexTicketConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(codex_ticket_config, '{}') FROM system_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(raw) == "" {
		return "{}", nil
	}
	return raw, err
}

func (db *DB) SaveCodexTicketConfig(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET codex_ticket_config = $1 WHERE id = 1`, raw)
		return err
	})
}
