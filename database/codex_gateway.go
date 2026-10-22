package database

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/internal/harvest"
	"time"
)

func (db *DB) ensureCodexGatewaySchema(ctx context.Context) error {
	id := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		id = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	for _, q := range []string{fmt.Sprintf(`CREATE TABLE IF NOT EXISTS codex_gateway_history (id %s, created_at BIGINT NOT NULL, body TEXT NOT NULL)`, id), `CREATE TABLE IF NOT EXISTS codex_harvest_scheduling (account_id BIGINT PRIMARY KEY, body TEXT NOT NULL)`, `CREATE INDEX IF NOT EXISTS codex_gateway_history_time ON codex_gateway_history(created_at)`} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func (r *HarvestRepository) AppendGatewayHistory(ctx context.Context, v harvest.GatewayHistoryRecord) error {
	if v.CheckedAt.IsZero() {
		v.CheckedAt = time.Now()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.db.withSQLiteWriteLock(ctx, func() error {
		_, err := r.db.conn.ExecContext(ctx, `INSERT INTO codex_gateway_history(created_at,body) VALUES($1,$2)`, v.CheckedAt.Unix(), string(b))
		return err
	})
}
func (r *HarvestRepository) GatewayHistory(ctx context.Context, offset, limit int) (harvest.GatewayHistoryPage, error) {
	out := harvest.GatewayHistoryPage{Items: []harvest.GatewayHistoryRecord{}}
	if err := r.db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_gateway_history`).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := r.db.conn.QueryContext(ctx, `SELECT id,body FROM codex_gateway_history ORDER BY id DESC LIMIT $1 OFFSET $2`, min(max(limit, 1), 200), max(offset, 0))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var v harvest.GatewayHistoryRecord
		var id int64
		if err = rows.Scan(&id, &raw); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return out, err
		}
		v.ID = id
		out.Items = append(out.Items, v)
	}
	return out, rows.Err()
}
func (r *HarvestRepository) PruneGatewayHistory(ctx context.Context) error {
	return r.db.withSQLiteWriteLock(ctx, func() error {
		_, err := r.db.conn.ExecContext(ctx, `DELETE FROM codex_gateway_history WHERE created_at<$1 OR id<(SELECT COALESCE(MAX(id),0)-10000 FROM codex_gateway_history)`, time.Now().Add(-7*24*time.Hour).Unix())
		return err
	})
}
