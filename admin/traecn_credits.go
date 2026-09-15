package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

const traeCNCreditsCacheTTL = time.Minute
const traeCNCreditsCacheMaxEntries = 1024

// 限制积分查询总并发，打开大号池列表时不挤占推理连接。
var traeCNCreditsQuerySlots = make(chan struct{}, 4)

type traeCNCreditsResponse struct {
	Credits *proxy.TraeCNCreditsSnapshot `json:"credits"`
	Stale   bool                         `json:"stale"`
	Error   string                       `json:"error,omitempty"`
}

type traeCNCreditsCacheEntry struct {
	Response  traeCNCreditsResponse
	ExpiresAt time.Time
}

type traeCNCreditsCache struct {
	mu      sync.Mutex
	entries map[*auth.Account]traeCNCreditsCacheEntry
	group   singleflight.Group
}

func (cache *traeCNCreditsCache) fresh(account *auth.Account) (traeCNCreditsResponse, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[account]
	return entry.Response, ok && time.Now().Before(entry.ExpiresAt)
}

func (cache *traeCNCreditsCache) get(ctx context.Context, account *auth.Account, force bool, query func(context.Context) (proxy.TraeCNCreditsSnapshot, error)) (traeCNCreditsResponse, error) {
	if !force {
		if cached, ok := cache.fresh(account); ok {
			return cached, nil
		}
	}
	// 同账号并发查询只向上游发一次；页面关闭不取消其他管理员共用的查询。
	result := cache.group.DoChan(fmt.Sprintf("%p", account), func() (any, error) {
		if !force {
			if cached, ok := cache.fresh(account); ok {
				return cached, nil
			}
		}
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		select {
		case traeCNCreditsQuerySlots <- struct{}{}:
			defer func() { <-traeCNCreditsQuerySlots }()
		case <-queryCtx.Done():
			return nil, queryCtx.Err()
		}
		snapshot, err := query(queryCtx)
		cache.mu.Lock()
		defer cache.mu.Unlock()
		response := traeCNCreditsResponse{Credits: &snapshot}
		if err != nil {
			// 查询失败时保留最后成功值并标明过期，不能用零覆盖旧余额。
			response = cache.entries[account].Response
			response.Stale = response.Credits != nil
			response.Error = err.Error()
		}
		if cache.entries == nil {
			cache.entries = make(map[*auth.Account]traeCNCreditsCacheEntry)
		}
		if _, exists := cache.entries[account]; !exists && len(cache.entries) >= traeCNCreditsCacheMaxEntries {
			var oldest *auth.Account
			var oldestAt time.Time
			for key, entry := range cache.entries {
				if oldest == nil || entry.ExpiresAt.Before(oldestAt) {
					oldest, oldestAt = key, entry.ExpiresAt
				}
			}
			delete(cache.entries, oldest)
		}
		// 失败结果也短暂缓存，避免无效账号在翻页时被反复查询。
		cache.entries[account] = traeCNCreditsCacheEntry{Response: response, ExpiresAt: time.Now().Add(traeCNCreditsCacheTTL)}
		return response, nil
	})
	select {
	case <-ctx.Done():
		return traeCNCreditsResponse{}, ctx.Err()
	case completed := <-result:
		if completed.Err != nil {
			return traeCNCreditsResponse{}, completed.Err
		}
		return completed.Val.(traeCNCreditsResponse), nil
	}
}

// GetTraeCNCredits 返回积分汇总，认证信息始终留在服务端。
func (h *Handler) GetTraeCNCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号服务不可用")
		return
	}
	account := h.store.FindByID(id)
	if account == nil || !account.IsTraeCNAPI() {
		writeError(c, http.StatusNotFound, "Trae CN 账号不存在")
		return
	}
	response, err := h.traeCNCredits.get(c.Request.Context(), account, c.Query("refresh") == "1", func(ctx context.Context) (proxy.TraeCNCreditsSnapshot, error) {
		return proxy.QueryTraeCNCredits(ctx, h.store, account)
	})
	if err != nil {
		writeError(c, http.StatusGatewayTimeout, "积分查询超时或已取消")
		return
	}
	if response.Credits == nil {
		writeError(c, http.StatusBadGateway, response.Error)
		return
	}
	c.JSON(http.StatusOK, response)
}
