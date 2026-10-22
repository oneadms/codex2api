package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/codex2api/internal/harvest"
	"reflect"
	"slices"
)

type HarvestSchedulingState struct {
	Mode           string  `json:"mode"`
	Identity       string  `json:"identity"`
	BeforeEnabled  bool    `json:"before_enabled"`
	BeforeModel    any     `json:"before_model"`
	BeforeGroups   []int64 `json:"before_groups"`
	AfterGroups    []int64 `json:"after_groups"`
	SelectedGroups []int64 `json:"selected_groups"`
}
type HarvestSchedulingResult struct {
	Changed      bool
	Enabled      bool
	ModelBlocked bool
	Groups       []int64
	State        HarvestSchedulingState
}

func (r *HarvestRepository) HarvestSchedulingState(ctx context.Context, id int64) (HarvestSchedulingState, error) {
	var s HarvestSchedulingState
	var raw string
	err := r.db.conn.QueryRowContext(ctx, `SELECT body FROM codex_harvest_scheduling WHERE account_id=$1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(raw), &s)
	return s, err
}

// Ownership and account mutations share one transaction. Restoration compares
// only managed fields; unrelated token/ticket refreshes must not cause conflicts.
func (r *HarvestRepository) ApplyHarvestScheduling(ctx context.Context, id int64, identity string, settings harvest.GatewaySettings, ready bool) (out HarvestSchedulingResult, err error) {
	err = r.db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var raw string
		if e := tx.QueryRowContext(ctx, `SELECT value FROM codex_harvest_settings WHERE key=$1`, harvest.GatewaySettingsKey).Scan(&raw); e != nil {
			return e
		}
		var actual harvest.GatewaySettings
		if json.Unmarshal([]byte(raw), &actual) != nil || actual.Revision != settings.Revision || !actual.TicketPoolScheduling {
			return errors.New("configuration_changed")
		}
		batch, e := r.db.selectBatchAccounts(ctx, tx, []int64{id}, true)
		if e != nil {
			return e
		}
		if len(batch.ids) != 1 {
			return errors.New("account_unavailable")
		}
		credentials := batch.credentials[id]
		if e = tx.QueryRowContext(ctx, `SELECT enabled FROM accounts WHERE id=$1`, id).Scan(&out.Enabled); e != nil {
			return e
		}
		groups := []int64{}
		rows, e := tx.QueryContext(ctx, `SELECT group_id FROM account_group_members WHERE account_id=$1 ORDER BY group_id`, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var group int64
			if e = rows.Scan(&group); e != nil {
				rows.Close()
				return e
			}
			groups = append(groups, group)
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()
		out.Groups = groups
		var state HarvestSchedulingState
		e = tx.QueryRowContext(ctx, `SELECT body FROM codex_harvest_scheduling WHERE account_id=$1`, id).Scan(&raw)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil {
			if e = json.Unmarshal([]byte(raw), &state); e != nil {
				return e
			}
		}
		out.ModelBlocked = credentials["codex_harvest_model_blocked"] == true
		switching := state.Mode != "" && (state.Mode != settings.SchedulingMode || state.Mode == "groups" && !reflect.DeepEqual(state.SelectedGroups, settings.SchedulingGroupIDs))
		if state.Mode != "" && (ready || switching) {
			if state.Identity != identity {
				return errors.New("restore_conflict")
			}
			switch state.Mode {
			case "account":
				if out.Enabled {
					return errors.New("restore_conflict")
				}
				if e = r.db.batchUpdateAccountColumns(ctx, tx, []int64{id}, BatchAccountMetadataUpdate{Enabled: OptionalBool{Set: true, Value: state.BeforeEnabled}}); e != nil {
					return e
				}
				out.Enabled = state.BeforeEnabled
			case "model":
				if !out.ModelBlocked {
					return errors.New("restore_conflict")
				}
				if e = r.db.batchUpdateAccountCredentials(ctx, tx, batch.credentials, map[string]interface{}{"codex_harvest_model_blocked": state.BeforeModel}); e != nil {
					return e
				}
				credentials["codex_harvest_model_blocked"] = state.BeforeModel
				out.ModelBlocked = state.BeforeModel == true
			case "groups":
				if !slices.Equal(groups, state.AfterGroups) {
					return errors.New("restore_conflict")
				}
				if e = r.db.batchReplaceAccountGroups(ctx, tx, []int64{id}, state.BeforeGroups); e != nil {
					return e
				}
				groups = slices.Clone(state.BeforeGroups)
				out.Groups = groups
			}
			state = HarvestSchedulingState{}
			out.Changed = true
		}
		if !ready && state.Mode == "" {
			next := HarvestSchedulingState{Mode: settings.SchedulingMode, Identity: identity, BeforeEnabled: out.Enabled, BeforeModel: credentials["codex_harvest_model_blocked"], BeforeGroups: slices.Clone(groups), SelectedGroups: slices.Clone(settings.SchedulingGroupIDs)}
			changed := false
			switch settings.SchedulingMode {
			case "account":
				if out.Enabled {
					e = r.db.batchUpdateAccountColumns(ctx, tx, []int64{id}, BatchAccountMetadataUpdate{Enabled: OptionalBool{Set: true, Value: false}})
					out.Enabled = false
					changed = true
				}
			case "model":
				if !out.ModelBlocked {
					e = r.db.batchUpdateAccountCredentials(ctx, tx, batch.credentials, map[string]interface{}{"codex_harvest_model_blocked": true})
					out.ModelBlocked = true
					changed = true
				}
			case "groups":
				after := []int64{}
				for _, group := range groups {
					if !slices.Contains(settings.SchedulingGroupIDs, group) {
						after = append(after, group)
					}
				}
				if !slices.Equal(groups, after) {
					e = r.db.batchReplaceAccountGroups(ctx, tx, []int64{id}, after)
					next.AfterGroups = after
					out.Groups = after
					changed = true
				}
			}
			if e != nil {
				return e
			}
			if changed {
				state = next
				out.Changed = true
			}
		}
		out.State = state
		if !out.Changed {
			return nil
		}
		if state.Mode == "" {
			_, e = tx.ExecContext(ctx, `DELETE FROM codex_harvest_scheduling WHERE account_id=$1`, id)
		} else {
			b, _ := json.Marshal(state)
			_, e = tx.ExecContext(ctx, `INSERT INTO codex_harvest_scheduling(account_id,body) VALUES($1,$2) ON CONFLICT(account_id) DO UPDATE SET body=excluded.body`, id, string(b))
		}
		if e != nil {
			return e
		}
		return insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, id, "harvest_scheduling")
	})
	return out, err
}
