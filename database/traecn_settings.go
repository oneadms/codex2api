package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// TRAECN 配置独立读写，通用设置的整行保存不会覆盖模型映射。
func (db *DB) LoadTraeCNConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(traecn_config, '{}') FROM system_settings WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(raw) == "" {
		return "{}", nil
	}
	return raw, err
}

func (db *DB) SaveTraeCNConfig(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET traecn_config = $1 WHERE id = 1`, raw)
		return err
	})
}
