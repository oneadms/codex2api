package proxy

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const traeCNCreditsUsagePath = "/trae/api/v2/pay/ide_user_ent_usage"

// TraeCNCreditsSnapshot 保存上游当前权益汇总，不累加历史、过期或隐藏的积分包。
type TraeCNCreditsSnapshot struct {
	Total       float64   `json:"total"`
	Used        float64   `json:"used"`
	Remaining   float64   `json:"remaining"`
	UsedPercent float64   `json:"used_percent"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// QueryTraeCNCredits 只查询积分，令牌与出口沿用账号配置。
func QueryTraeCNCredits(ctx context.Context, store *auth.Store, account *auth.Account) (TraeCNCreditsSnapshot, error) {
	return queryTraeCNCredits(ctx, store, account, TraeCNCheckinHost)
}

func queryTraeCNCredits(ctx context.Context, store *auth.Store, account *auth.Account, host string) (TraeCNCreditsSnapshot, error) {
	var empty TraeCNCreditsSnapshot
	if account == nil || !account.IsTraeCNAPI() {
		return empty, fmt.Errorf("Trae CN 账号不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	account.Mu().RLock()
	proxyURL := strings.TrimSpace(account.ProxyURL)
	account.Mu().RUnlock()
	var err error
	if store != nil {
		err = store.EnsureTraeCNAccountWithProxy(ctx, account, proxyURL)
	} else {
		err = account.EnsureTraeCNAccessToken(ctx, proxyURL, false)
	}
	if err != nil {
		return empty, fmt.Errorf("刷新 Trae CN 令牌失败")
	}
	_, accessToken := account.TraeCNCredentials()
	if accessToken == "" {
		return empty, fmt.Errorf("Trae CN 访问令牌为空")
	}
	endpoint := strings.TrimRight(host, "/") + traeCNCreditsUsagePath
	viaResin := IsResinEnabledForContext(ctx) && account.ID() > 0
	if viaResin {
		platform := ResinPlatformFromContext(ctx)
		if platform == "" {
			platform = ResinPlatformForSessionFromContext(ctx, "")
		}
		endpoint = BuildReverseProxyURLForContext(ctx, endpoint, platform)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(`{"require_usage":true,"req_source":1}`))
	if err != nil {
		return empty, fmt.Errorf("创建积分查询请求失败")
	}
	// pay 和签到接口均使用市场客户端身份，不能套用推理接口的 x-ide-token 头。
	req.Header = auth.TraeCNCheckinHeaders(account, accessToken, uuid.NewString())
	var resp *http.Response
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
		resp, err = getResinHTTPClient(account).Do(req)
	} else {
		resp, err = getPooledClient(account, proxyURL).Do(req)
	}
	if err != nil {
		return empty, fmt.Errorf("积分查询连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return empty, fmt.Errorf("积分查询返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return empty, fmt.Errorf("积分查询响应读取失败或超过大小限制")
	}
	return parseTraeCNCredits(raw, time.Now())
}

func parseTraeCNCredits(raw []byte, now time.Time) (TraeCNCreditsSnapshot, error) {
	var result TraeCNCreditsSnapshot
	if !gjson.ValidBytes(raw) {
		return result, fmt.Errorf("积分查询响应不是有效 JSON")
	}
	root := gjson.ParseBytes(raw)
	if code := root.Get("code"); code.Exists() && code.String() != "0" {
		return result, fmt.Errorf("积分查询返回业务错误")
	}
	// 非积分计费账号不能把美元额度误显示成积分。
	if root.Get("is_credits_billing").Type != gjson.True || root.Get("is_dollar_usage_billing").Bool() {
		return result, fmt.Errorf("该账号未返回积分计费数据")
	}
	amount := func(path string) (float64, bool) {
		value := root.Get(path)
		if value.Type != gjson.Number && value.Type != gjson.String {
			return 0, false
		}
		number, err := strconv.ParseFloat(value.String(), 64)
		return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0
	}
	var totalOK, usedOK bool
	result.Total, totalOK = amount("usage_summary.total_amount")
	result.Used, usedOK = amount("usage_summary.consumed_amount")
	if !totalOK || !usedOK {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("积分汇总缺少有效的总量或已消耗字段")
	}
	result.Remaining = math.Max(0, result.Total-result.Used)
	if result.Total > 0 {
		result.UsedPercent = math.Min(100, result.Used/result.Total*100)
	} else if result.Used > 0 {
		result.UsedPercent = 100
	}
	result.UpdatedAt = now.UTC()
	return result, nil
}
