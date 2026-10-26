package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

const traeCNModelSyncConcurrency = 4
const traeCNBatchModelSyncAction = "traecn_batch_sync_models"

type traeCNModelSyncState struct {
	inflight  singleflight.Group
	startOnce sync.Once
	wg        sync.WaitGroup
	scanMu    sync.Mutex
	slot      time.Time
	attempts  map[int64]traeCNModelSyncAttempt
}

type traeCNModelSyncResult struct {
	models    []string
	effective []string
	syncedAt  time.Time
}

// Imports, single-account refresh, batches and the scheduler all share this
// path. Concurrent requests for the same account share one upstream fetch.
func (h *Handler) syncTraeCNAccountModels(ctx context.Context, account *auth.Account) ([]string, []string, time.Time, error) {
	if h == nil || h.store == nil || h.db == nil || account == nil || !account.IsTraeCNAPI() {
		return nil, nil, time.Time{}, fmt.Errorf("Trae CN 账号或账号服务不可用")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, time.Time{}, err
	}
	result := h.traeCNModelSync.inflight.DoChan(strconv.FormatInt(account.ID(), 10), func() (any, error) {
		models, effective, syncedAt, err := h.fetchAndSaveTraeCNAccountModels(ctx, account)
		return traeCNModelSyncResult{models: models, effective: effective, syncedAt: syncedAt}, err
	})
	select {
	case <-ctx.Done():
		return nil, nil, time.Time{}, ctx.Err()
	case outcome := <-result:
		if outcome.Err != nil {
			return nil, nil, time.Time{}, outcome.Err
		}
		value := outcome.Val.(traeCNModelSyncResult)
		return value.models, value.effective, value.syncedAt, nil
	}
}

func (h *Handler) traeCNModelSyncAccount(ctx context.Context, id int64, enabledOnly bool) (*auth.Account, error) {
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("读取账号失败: %w", err)
	}
	if row.DeletedAt.Valid || row.Status == "deleted" || row.ErrorMessage == "deleted" || !strings.EqualFold(row.GetCredential("upstream_type"), auth.UpstreamTraeCN) {
		return nil, fmt.Errorf("Trae CN 账号不存在")
	}
	if enabledOnly && !row.Enabled {
		return nil, fmt.Errorf("Trae CN 账号已禁用")
	}
	account := h.store.FindByID(id)
	if account == nil {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			return nil, fmt.Errorf("加载账号失败: %w", err)
		}
		account = h.store.FindByID(id)
	}
	if account == nil || !account.IsTraeCNAPI() {
		return nil, fmt.Errorf("Trae CN 账号未加载到运行时池")
	}
	return account, nil
}

// runTraeCNModelSyncTasks uses a fixed worker pool (not one goroutine per ID).
// A single collector updates counts and writes progress so SSE writes never race.
func (h *Handler) runTraeCNModelSyncTasks(ctx context.Context, ids []int64, syncOne func(context.Context, int64) error, onProgress func(batchOperationEvent)) (int64, int64) {
	type result struct {
		id  int64
		err error
	}
	jobs := make(chan int64)
	results := make(chan result, traeCNModelSyncConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < min(traeCNModelSyncConcurrency, len(ids)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				attemptCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				err := syncOne(attemptCtx, id)
				cancel()
				results <- result{id: id, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return
			case jobs <- id:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	var success, failed int64
	for item := range results {
		event := batchOperationEvent{
			Type: "progress", Action: traeCNBatchModelSyncAction,
			Status: "success", AccountID: item.id, Total: len(ids),
			Message: "模型目录同步成功",
		}
		if item.err != nil {
			failed++
			event.Status = "failed"
			event.Error = item.err.Error()
			event.Message = item.err.Error()
		} else {
			success++
		}
		event.Current = int(success + failed)
		event.Success, event.Failed = success, failed
		if onProgress != nil {
			event.AccountName, event.AccountEmail = h.accountOperationIdentity(item.id)
			onProgress(event)
		}
	}
	if success > 0 {
		h.invalidateAccountSnapshotCaches()
	}
	return success, failed
}

// BatchSyncTraeCNModels accepts explicit IDs or the same server-side filter
// selector as other account batch actions, including matches on other pages.
func (h *Handler) BatchSyncTraeCNModels(c *gin.Context) {
	if h == nil || h.store == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "账号服务不可用")
		return
	}
	var req batchAccountIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids 与 selector 不能同时提供")
		return
	}
	var ids []int64
	if req.IDs != nil {
		ids = uniqueAccountIDs(*req.IDs)
	}
	if req.Selector != nil {
		if !strings.EqualFold(strings.TrimSpace(req.Selector.Channel), database.UpstreamChannelTraeCN) {
			writeError(c, http.StatusBadRequest, "模型目录批量同步仅支持 TRAECN 渠道")
			return
		}
		selectorCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		resolved, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		cancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		ids = uniqueAccountIDs(resolved)
	}
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要同步模型目录的账号 ID 列表或筛选条件")
		return
	}
	syncOne := func(ctx context.Context, id int64) error {
		account, err := h.traeCNModelSyncAccount(ctx, id, false)
		if err != nil {
			return err
		}
		_, _, _, err = h.syncTraeCNAccountModels(ctx, account)
		if err == nil {
			h.db.InsertAccountEventAsync(id, "updated", "traecn_models_sync_batch")
		} else {
			h.db.InsertAccountEventAsync(id, "traecn_models_sync_failed", err.Error())
		}
		return err
	}
	var onProgress func(batchOperationEvent)
	stream := strings.EqualFold(c.Query("stream"), "true")
	if stream {
		setupSSE(c)
		sendSSEJSON(c, batchOperationEvent{Type: "start", Action: traeCNBatchModelSyncAction, Total: len(ids)})
		onProgress = func(event batchOperationEvent) { sendSSEJSON(c, event) }
	}
	success, failed := h.runTraeCNModelSyncTasks(c.Request.Context(), ids, syncOne, onProgress)
	if c.Request.Context().Err() != nil {
		return
	}
	if stream {
		sendSSEJSON(c, batchOperationEvent{
			Type: "complete", Action: traeCNBatchModelSyncAction,
			Current: int(success + failed), Total: len(ids), Success: success, Failed: failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已同步 %d 个账号的模型目录，失败 %d 个", success, failed),
		"total":   len(ids), "success": success, "failed": failed,
	})
}
