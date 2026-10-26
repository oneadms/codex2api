package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const (
	traeCNModelSyncPollInterval  = time.Minute
	traeCNModelSyncRetryInterval = 10 * time.Minute
	traeCNModelSyncMaxAttempts   = 3
)

type traeCNModelSyncAttempt struct {
	count int
	at    time.Time
}

// traeCNModelSyncSlot returns today's scheduled instant in the explicitly
// configured timezone. A late tick or a restart after that instant catches up
// today's run, without replaying all the days the service was offline.
func traeCNModelSyncSlot(settings auth.TraeCNSettings, now time.Time) (time.Time, bool) {
	if !settings.ModelSyncEnabled {
		return time.Time{}, false
	}
	clock, err := time.Parse("15:04", settings.ModelSyncTime)
	if err != nil {
		return time.Time{}, false
	}
	location, err := time.LoadLocation(settings.ModelSyncTimezone)
	if err != nil {
		return time.Time{}, false
	}
	local := now.In(location)
	slot := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
	return slot, !now.Before(slot)
}

func (h *Handler) StartTraeCNModelSyncScheduler(ctx context.Context) {
	if h == nil || h.store == nil || h.db == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.traeCNModelSync.startOnce.Do(func() {
		h.traeCNModelSync.wg.Add(1)
		go func() {
			defer h.traeCNModelSync.wg.Done()
			ticker := time.NewTicker(traeCNModelSyncPollInterval)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				h.runTraeCNModelSyncDue(ctx, time.Now())
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (h *Handler) WaitTraeCNModelSyncScheduler() {
	if h != nil {
		h.traeCNModelSync.wg.Wait()
	}
}

func (h *Handler) runTraeCNModelSyncDue(ctx context.Context, now time.Time) {
	if h == nil || h.store == nil || h.db == nil || ctx.Err() != nil {
		return
	}
	// Keep scans sequential, even if a scan takes longer than the poll interval.
	h.traeCNModelSync.scanMu.Lock()
	defer h.traeCNModelSync.scanMu.Unlock()
	scanStarted := time.Now()
	settings := auth.ConfiguredTraeCNSettings()
	slot, due := traeCNModelSyncSlot(settings, now)
	if !due {
		return
	}
	state := &h.traeCNModelSync
	if !state.slot.Equal(slot) {
		state.slot = slot
		state.attempts = make(map[int64]traeCNModelSyncAttempt)
	}

	// Read the persisted projection, not just the hydrated runtime pool. This
	// also makes successful runs survive restarts and includes lazy accounts.
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	rows, err := h.db.ListAccountListProjection(listCtx, database.UpstreamChannelTraeCN)
	cancel()
	if err != nil {
		log.Printf("[traecn-model-sync] 读取账号列表失败: %v", err)
		return
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		syncedAt, _ := time.Parse(time.RFC3339Nano, row.GetCredential(auth.TraeCNModelsSyncedAtCredentialKey))
		if !syncedAt.Before(slot) {
			continue
		}
		attempt := state.attempts[row.ID]
		if attempt.count >= traeCNModelSyncMaxAttempts || !attempt.at.IsZero() && now.Before(attempt.at.Add(traeCNModelSyncRetryInterval)) {
			continue
		}
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return
	}

	success, failed := h.runTraeCNModelSyncTasks(ctx, ids, func(attemptCtx context.Context, id int64) error {
		// Settings and enabled state may change while earlier accounts are being
		// fetched. Stop queued work when the administrator changes the schedule.
		current := auth.ConfiguredTraeCNSettings()
		if !current.ModelSyncEnabled || current.ModelSyncTime != settings.ModelSyncTime || current.ModelSyncTimezone != settings.ModelSyncTimezone {
			return context.Canceled
		}
		account, err := h.traeCNModelSyncAccount(attemptCtx, id, true)
		if err != nil {
			return err
		}
		if !account.TraeCNModelCatalogSyncedAt().Before(slot) {
			return nil
		}
		_, _, _, err = h.syncTraeCNAccountModels(attemptCtx, account)
		if err == nil {
			h.db.InsertAccountEventAsync(id, "updated", "traecn_models_sync_scheduled")
		} else if attemptCtx.Err() == nil {
			h.db.InsertAccountEventAsync(id, "traecn_models_sync_failed", err.Error())
		}
		return err
	}, func(event batchOperationEvent) {
		if ctx.Err() != nil || event.Error == context.Canceled.Error() {
			return
		}
		attempt := state.attempts[event.AccountID]
		attempt.count++
		// Back off from completion, not the beginning of a potentially long
		// batch, so accounts late in the queue cannot be retried immediately.
		attempt.at = now.Add(time.Since(scanStarted))
		state.attempts[event.AccountID] = attempt
		if event.Status == "failed" {
			log.Printf("[traecn-model-sync] 账号 %d 第 %d 次同步失败: %s", event.AccountID, attempt.count, event.Error)
		}
	})
	log.Printf("[traecn-model-sync] 定时同步完成: success=%d failed=%d schedule=%s timezone=%s", success, failed, settings.ModelSyncTime, settings.ModelSyncTimezone)
}
