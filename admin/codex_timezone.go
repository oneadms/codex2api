package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

var codexTimezoneLookupURL = "https://ipwho.is/"

var errCodexTimezoneUnsupported = errors.New("仅 Codex 官方账号支持按出口 IP 绑定时区")

type codexExitTimezone struct {
	IP       string `json:"ip"`
	Timezone string `json:"timezone"`
}

// BindCodexTimezoneFromIP 查询账号当前生效的出口，并立即保存绑定时区。
func (h *Handler) BindCodexTimezoneFromIP(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, err := h.bindCodexTimezoneFromIP(ctx, id, false)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(c, http.StatusNotFound, "账号不存在")
		case errors.Is(err, errCodexTimezoneUnsupported):
			writeError(c, http.StatusBadRequest, err.Error())
		default:
			writeError(c, http.StatusBadGateway, err.Error())
		}
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) bindCodexTimezoneFromIP(ctx context.Context, id int64, onlyIfUnset bool) (*codexExitTimezone, error) {
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if accountRowGroupChannel(row) != database.AccountGroupChannelCodex || strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses) {
		return nil, errCodexTimezoneUnsupported
	}
	if onlyIfUnset && strings.TrimSpace(row.GetCredential(auth.AccountTimezoneCredentialKey)) != "" {
		return nil, nil
	}
	account := h.store.FindByID(id)
	if account == nil {
		// 待审核或禁用账号可能尚未入池，只读取路由信息，不把它加入调度池。
		groups, err := h.db.GetAccountGroupIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		account = &auth.Account{DBID: id, ProxyURL: row.ProxyURL, GroupIDs: groups}
	}
	proxyURL, usable := h.store.ResolveUsableProxyForAccount(account)
	if !usable {
		return nil, errors.New("账号没有可用的出口代理，请检查代理配置")
	}
	result, err := queryCodexExitTimezone(ctx, account, proxyURL)
	if err != nil {
		return nil, err
	}
	if onlyIfUnset {
		// 查询期间可能有人手动绑定时区，后台任务不覆盖这次设置。
		row, err = h.db.GetAccountByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(row.GetCredential(auth.AccountTimezoneCredentialKey)) != "" {
			return nil, nil
		}
	}
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{auth.AccountTimezoneCredentialKey: result.Timezone}); err != nil {
		return nil, fmt.Errorf("保存账号时区失败: %w", err)
	}
	h.store.ApplyAccountTimezone(id, result.Timezone)
	h.invalidateAccountSnapshotCaches()
	h.db.InsertAccountEventAsync(id, "updated", "auto_timezone")
	return result, nil
}

func queryCodexExitTimezone(ctx context.Context, account *auth.Account, proxyURL string) (*codexExitTimezone, error) {
	egress := proxy.ResolveCodexEgressForContext(ctx, account, codexTimezoneLookupURL, proxyURL)
	var client *http.Client
	if egress.ViaResin() {
		client = egress.Client()
	} else {
		// 代理配置无效时直接报错，禁止回落直连后绑定到服务器的时区。
		var err error
		client, err = auth.BuildHTTPClientChecked(egress.DialProxyURL)
		if err != nil {
			return nil, errors.New("账号出口代理配置无效")
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, egress.URL, nil)
	if err != nil {
		return nil, err
	}
	// IP 查询网站只接收出口路由所需的身份头，不携带账号令牌或自定义凭据头。
	egress.ApplyHeaders(request.Header)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("查询出口 IP 失败，请检查账号代理或稍后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IP 查询服务返回 HTTP %d", response.StatusCode)
	}
	var payload struct {
		Success  bool   `json:"success"`
		IP       string `json:"ip"`
		Timezone struct {
			ID string `json:"id"`
		} `json:"timezone"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, errors.New("IP 查询服务返回了无效的数据")
	}
	ip := strings.TrimSpace(payload.IP)
	timezone := strings.TrimSpace(payload.Timezone.ID)
	if !payload.Success || net.ParseIP(ip) == nil || timezone == "" || validateAccountTimezone(timezone) != nil {
		return nil, errors.New("IP 查询服务未返回有效的出口 IP 和时区")
	}
	return &codexExitTimezone{IP: ip, Timezone: timezone}, nil
}

// scheduleCodexTimezoneBindings 仅在首次建号后排队一次，复用导入队列限制并发。
// 调用方须先完成分组绑定，使探测与账号实际出站使用同一条代理链。
func (h *Handler) scheduleCodexTimezoneBindings(ids []int64) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	for _, id := range ids {
		h.runImportProbeTask(func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if _, err := h.bindCodexTimezoneFromIP(ctx, id, true); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errCodexTimezoneUnsupported) {
				log.Printf("账号 %d 首次自动绑定时区失败: %v", id, err)
			}
		})
	}
}
