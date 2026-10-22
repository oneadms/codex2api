package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/codex2api/internal/harvest"
)

// CompareAndSwapGatewaySettings protects against stale clients and other server
// processes. RouteRevision is independent of this per-save concurrency token.
func (r *HarvestRepository) CompareAndSwapGatewaySettings(ctx context.Context, revision string, next harvest.GatewaySettings) (saved bool, err error) {
	body, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	err = r.db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw string
		e := tx.QueryRowContext(ctx, `SELECT value FROM codex_harvest_settings WHERE key=$1`, harvest.GatewaySettingsKey).Scan(&raw)
		var result sql.Result
		if errors.Is(e, sql.ErrNoRows) {
			if revision != "" {
				return nil
			}
			result, e = tx.ExecContext(ctx, `INSERT INTO codex_harvest_settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, harvest.GatewaySettingsKey, string(body))
		} else if e == nil {
			var previous harvest.GatewaySettings
			if e = json.Unmarshal([]byte(raw), &previous); e != nil {
				return e
			}
			if previous.Revision != revision {
				return nil
			}
			result, e = tx.ExecContext(ctx, `UPDATE codex_harvest_settings SET value=$1 WHERE key=$2 AND value=$3`, string(body), harvest.GatewaySettingsKey, raw)
		}
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		saved = count == 1
		return e
	})
	return saved, err
}
