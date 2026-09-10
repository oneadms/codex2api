package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Trae CN 的积分签到走账号体系域名（不是推理用的 mchost host），认证头沿用同一枚
// Cloud-IDE-JWT 访问令牌，但要求额外的市场客户端标识。
const (
	TraeCNCheckinHost       = "https://api.trae.cn"
	traeCNCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCNCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
)

// traeCNCheckinMarketClientID 是桌面端上报的市场客户端标识。
const traeCNCheckinMarketClientID = "VSCode 1.107.1"

type traeCNCheckinStatus struct {
	Code      int64
	Enabled   bool
	CheckedIn bool
	Credits   int64
	Extra     int64
	Message   string
}

// traeCNCheckinOutcome 是单次签到尝试的结果，供调度器与管理台共用。
type traeCNCheckinOutcome struct {
	CheckedIn bool
	Claimed   bool
	// Skipped 非空表示没有发起领取：disabled（上游关闭签到）、already（今天已签）、
	// not_enabled（本账号无签到资格）。
	Skipped string
	Status  traeCNCheckinStatus
}

// Message 生成一句可读结果，写入账号事件。
func (o traeCNCheckinOutcome) Message() string {
	switch {
	case o.Skipped == "disabled":
		return "上游未开启签到"
	case o.Skipped == "not_enabled":
		return "账号没有签到资格"
	case o.Skipped == "already":
		return fmt.Sprintf("今日已签到（积分 %d）", o.Status.Credits)
	case o.Claimed:
		return fmt.Sprintf("签到成功（积分 %d，额外 %d）", o.Status.Credits, o.Status.Extra)
	default:
		return o.Status.Message
	}
}

// RunTraeCNCheckin 查询签到状态并在需要时领取，然后重新读取状态以拿到最新积分。
func RunTraeCNCheckin(ctx context.Context, store *auth.Store, account *auth.Account, proxyOverride string) (traeCNCheckinOutcome, error) {
	return runTraeCNCheckin(ctx, store, account, proxyOverride, TraeCNCheckinHost)
}

func runTraeCNCheckin(ctx context.Context, store *auth.Store, account *auth.Account, proxyOverride, host string) (traeCNCheckinOutcome, error) {
	var outcome traeCNCheckinOutcome
	if account == nil || !account.IsTraeCNAPI() {
		return outcome, fmt.Errorf("traecn account is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(proxyOverride) == "" {
		account.Mu().RLock()
		proxyOverride = strings.TrimSpace(account.ProxyURL)
		account.Mu().RUnlock()
	}
	// 签到与推理共用同一枚会轮换的 RT/AT：先确保令牌有效再发请求。
	var err error
	if store != nil {
		err = store.EnsureTraeCNAccountWithProxy(ctx, account, proxyOverride)
	} else {
		err = account.EnsureTraeCNAccessToken(ctx, proxyOverride, false)
	}
	if err != nil {
		return outcome, fmt.Errorf("refresh Trae CN token: %w", err)
	}
	_, accessToken := account.TraeCNCredentials()
	if strings.TrimSpace(accessToken) == "" {
		return outcome, fmt.Errorf("traecn credentials are incomplete")
	}
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		host = TraeCNCheckinHost
	}
	client := getPooledClient(account, proxyOverride)
	post := func(path string) (gjson.Result, error) {
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		body, _ := json.Marshal(map[string]any{"req_source": 1})
		req, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, host+path, bytes.NewReader(body))
		if requestErr != nil {
			return gjson.Result{}, requestErr
		}
		req.Header = auth.TraeCNCheckinHeaders(account, accessToken, uuid.NewString())
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			return gjson.Result{}, requestErr
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return gjson.Result{}, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message := strings.TrimSpace(string(raw))
			if len(message) > 200 {
				message = message[:200]
			}
			return gjson.Result{}, fmt.Errorf("HTTP %d %s", resp.StatusCode, message)
		}
		return gjson.ParseBytes(raw), nil
	}

	statusOf := func(root gjson.Result) traeCNCheckinStatus {
		return traeCNCheckinStatus{
			Code:      root.Get("code").Int(),
			Enabled:   !root.Get("enable").Exists() || root.Get("enable").Bool(),
			CheckedIn: root.Get("checked_in").Bool(),
			Credits:   root.Get("credits").Int(),
			Extra:     root.Get("extra_credits").Int(),
			Message:   strings.TrimSpace(root.Get("message").String()),
		}
	}

	root, err := post(traeCNCheckinStatusPath)
	if err != nil {
		return outcome, fmt.Errorf("traecn checkin status: %w", err)
	}
	outcome.Status = statusOf(root)
	if !outcome.Status.Enabled {
		outcome.Skipped = "disabled"
		return outcome, nil
	}
	if outcome.Status.CheckedIn {
		outcome.Skipped = "already"
		outcome.CheckedIn = true
		return outcome, nil
	}
	claim, err := post(traeCNCheckinClaimPath)
	if err != nil {
		return outcome, fmt.Errorf("traecn checkin claim: %w", err)
	}
	if code := claim.Get("code").Int(); code != 0 {
		message := strings.TrimSpace(claim.Get("message").String())
		if message == "" {
			message = "上游拒绝签到"
		}
		return outcome, fmt.Errorf("traecn checkin claim: %s (code %d)", message, code)
	}
	outcome.Claimed = true
	// 领取后重新读一次状态：积分与额外积分只有状态接口会回。
	if latest, latestErr := post(traeCNCheckinStatusPath); latestErr == nil {
		outcome.Status = statusOf(latest)
		outcome.CheckedIn = outcome.Status.CheckedIn
	} else {
		outcome.CheckedIn = true
	}
	return outcome, nil
}
