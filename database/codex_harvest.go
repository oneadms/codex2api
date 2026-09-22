package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/codex2api/internal/harvest"
)

func (db *DB) ensureCodexHarvestSchema(ctx context.Context) error {
	id := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		id = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS codex_harvest_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_harvest_epoch (id INTEGER PRIMARY KEY, generation BIGINT NOT NULL)`,
		`INSERT INTO codex_harvest_epoch (id, generation) VALUES (1, 0) ON CONFLICT (id) DO NOTHING`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS codex_harvest_nodes (id %s, scope_key TEXT NOT NULL, node_id TEXT NOT NULL, body TEXT NOT NULL, UNIQUE(scope_key, node_id))`, id),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS codex_harvest_events (id %s, account_id BIGINT NOT NULL, job_id TEXT NOT NULL, created_at BIGINT NOT NULL, body TEXT NOT NULL)`, id),
		`CREATE INDEX IF NOT EXISTS codex_harvest_events_account ON codex_harvest_events(account_id, id)`,
		`CREATE INDEX IF NOT EXISTS codex_harvest_events_job ON codex_harvest_events(job_id, id)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// HarvestRepository 适配采票模块的独立接口，避免侵入通用设置及账号表。
type HarvestRepository struct{ db *DB }

func (db *DB) HarvestRepository() *HarvestRepository { return &HarvestRepository{db: db} }

func (r *HarvestRepository) GetValue(ctx context.Context, key string) (string, error) {
	var value string
	err := r.db.conn.QueryRowContext(ctx, `SELECT value FROM codex_harvest_settings WHERE key=$1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", harvest.ErrSettingNotFound
	}
	return value, err
}

func (r *HarvestRepository) Set(ctx context.Context, key, value string) error {
	return r.db.withSQLiteWriteLock(ctx, func() error {
		_, err := r.db.conn.ExecContext(ctx, `INSERT INTO codex_harvest_settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	})
}

func harvestScopeKey(s harvest.CodexHarvestNodeScope) string {
	// 账号身份在 DTO 中隐藏，但必须参与学习记录分区，换账号凭据后不复用旧统计。
	raw, _ := json.Marshal([]any{s.PoolID, s.AccountID, s.Identity, s.Model, s.Blocks})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r *HarvestRepository) Snapshot(ctx context.Context, scope harvest.CodexHarvestNodeScope) (int64, []harvest.CodexHarvestNodeRecord, error) {
	var generation int64
	if err := r.db.conn.QueryRowContext(ctx, `SELECT generation FROM codex_harvest_epoch WHERE id=1`).Scan(&generation); err != nil {
		return 0, nil, err
	}
	rows, err := r.db.conn.QueryContext(ctx, `SELECT id,body FROM codex_harvest_nodes WHERE scope_key=$1 ORDER BY id`, harvestScopeKey(scope))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	items, err := readHarvestNodes(rows)
	return generation, items, err
}

func readHarvestNodes(rows *sql.Rows) ([]harvest.CodexHarvestNodeRecord, error) {
	items := []harvest.CodexHarvestNodeRecord{}
	for rows.Next() {
		var id int64
		var raw string
		var record harvest.CodexHarvestNodeRecord
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		record.ID = id
		items = append(items, record)
	}
	return items, rows.Err()
}

func (r *HarvestRepository) Record(ctx context.Context, feedback harvest.CodexHarvestNodeFeedback) (bool, error) {
	stored := false
	err := r.db.withSQLiteWriteLock(ctx, func() error {
		tx, err := r.db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var generation int64
		// 与重置共用数据库锁；重置前开始的请求不能重新污染清空后的记录。
		if err = tx.QueryRowContext(ctx, `UPDATE codex_harvest_epoch SET generation=generation WHERE id=1 RETURNING generation`).Scan(&generation); err != nil {
			return err
		}
		if generation != feedback.Generation {
			return nil
		}
		key := harvestScopeKey(feedback.Scope)
		var raw string
		record := harvest.CodexHarvestNodeRecord{CodexHarvestNodeScope: feedback.Scope, NodeID: feedback.Node.ID, NodeName: feedback.Node.Name, Provider: feedback.Node.Provider}
		err = tx.QueryRowContext(ctx, `SELECT body FROM codex_harvest_nodes WHERE scope_key=$1 AND node_id=$2`, key, feedback.Node.ID).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &record); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		record.NodeName, record.Provider = feedback.Node.Name, feedback.Node.Provider
		record.LastResult, record.LatencyMS, record.UpdatedAt = feedback.Result, feedback.LatencyMS, now
		switch feedback.Result {
		case "success":
			record.Successes++
			record.ConsecutiveFailures = 0
			record.LastSuccess = &now
			record.CooldownUntil = nil
		case "account_error":
			record.AccountErrors++
		default:
			if feedback.Result == "network_error" {
				record.NetworkErrors++
			} else {
				record.Misses++
			}
			record.ConsecutiveFailures++
			until := now.Add(time.Duration(feedback.CooldownSeconds) * time.Second)
			record.CooldownUntil = &until
		}
		body, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_harvest_nodes(scope_key,node_id,body) VALUES($1,$2,$3) ON CONFLICT(scope_key,node_id) DO UPDATE SET body=excluded.body`, key, feedback.Node.ID, string(body))
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		stored = true
		return nil
	})
	return stored, err
}

func (r *HarvestRepository) List(ctx context.Context, offset, limit int) (harvest.CodexHarvestNodePage, error) {
	out := harvest.CodexHarvestNodePage{Items: []harvest.CodexHarvestNodeRecord{}}
	if err := r.db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_harvest_nodes`).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := r.db.conn.QueryContext(ctx, `SELECT id,body FROM codex_harvest_nodes ORDER BY id DESC LIMIT $1 OFFSET $2`, min(max(limit, 1), 200), max(offset, 0))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Items, err = readHarvestNodes(rows)
	return out, err
}

func (r *HarvestRepository) Reset(ctx context.Context, id int64) error {
	return r.db.withSQLiteWriteLock(ctx, func() error {
		tx, err := r.db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `UPDATE codex_harvest_epoch SET generation=generation+1 WHERE id=1`); err != nil {
			return err
		}
		if id == 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM codex_harvest_nodes`)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM codex_harvest_nodes WHERE id=$1`, id)
		}
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (r *HarvestRepository) AppendEvent(ctx context.Context, event harvest.Event) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return r.db.withSQLiteWriteLock(ctx, func() error {
		_, err := r.db.conn.ExecContext(ctx, `INSERT INTO codex_harvest_events(account_id,job_id,created_at,body) VALUES($1,$2,$3,$4)`, event.AccountID, event.JobID, event.CreatedAt.Unix(), string(body))
		return err
	})
}

func (r *HarvestRepository) Events(ctx context.Context, accountID int64, jobID string, offset, limit int) (harvest.EventPage, error) {
	out := harvest.EventPage{Items: []harvest.Event{}}
	where := ` WHERE ($1=0 OR account_id=$1) AND ($2='' OR job_id=$2)`
	if err := r.db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_harvest_events`+where, accountID, jobID).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := r.db.conn.QueryContext(ctx, `SELECT id,body FROM codex_harvest_events`+where+` ORDER BY id DESC LIMIT $3 OFFSET $4`, accountID, jobID, min(max(limit, 1), 200), max(offset, 0))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		var event harvest.Event
		if err = rows.Scan(&id, &raw); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(raw), &event); err != nil {
			return out, err
		}
		event.ID = id
		out.Items = append(out.Items, event)
	}
	return out, rows.Err()
}

func (r *HarvestRepository) PruneEvents(ctx context.Context) error {
	return r.db.withSQLiteWriteLock(ctx, func() error {
		_, err := r.db.conn.ExecContext(ctx, `DELETE FROM codex_harvest_events WHERE created_at<$1 OR id<(SELECT COALESCE(MAX(id),0)-10000 FROM codex_harvest_events)`, time.Now().Add(-7*24*time.Hour).Unix())
		return err
	})
}
