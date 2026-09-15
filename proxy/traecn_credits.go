package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const traeCNCreditsUsagePath = "/trae/api/v2/pay/ide_user_ent_usage"

// Trae CN 的积分按端点分池：权益包 `available_endpoint=0` 是 IDE/Code 侧可用额度，
// `=1` 是 Work 专属额度，两者分开消耗。推理请求用 body 的 access_type 选端点
// （实测 access_type=1 扣 Work 池），pay 接口则用 req_source 选择要看的端点集合：
// IDE 客户端（req_source=1）看不到 Work 专属包，Work 客户端（req_source=2）能看到
// 全部，所以查询默认按 Work 侧取全集，再按 available_endpoint 分池。
const (
	TraeCNCreditsPoolCode = "code"
	TraeCNCreditsPoolWork = "work"

	traeCNCreditsCodeReqSource = 1
	traeCNCreditsWorkReqSource = 2

	// traeCNCreditsCodeEndpoint / WorkEndpoint 是权益包的 available_endpoint 取值。
	traeCNCreditsCodeEndpoint = 0
	traeCNCreditsWorkEndpoint = 1

	// traeCNCreditsWorkPoolDisableEnv 允许只查 IDE 侧权益（TRAECN_CREDITS_WORK_DISABLED=1）。
	traeCNCreditsWorkPoolDisableEnv = "TRAECN_CREDITS_WORK_DISABLED"
)

// TraeCNCreditsPool 保存单个积分池的汇总。查询失败的池只带 Kind 与 Error，
// 不能用零值冒充真实余额。
type TraeCNCreditsPool struct {
	Kind        string    `json:"kind"`
	Total       float64   `json:"total"`
	Used        float64   `json:"used"`
	Remaining   float64   `json:"remaining"`
	UsedPercent float64   `json:"used_percent"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// TraeCNCreditsSnapshot 保存上游当前权益汇总，不累加历史、过期或隐藏的积分包。
type TraeCNCreditsSnapshot struct {
	Pools []TraeCNCreditsPool `json:"pools"`
}

// Pool 返回指定积分池；池被关闭或不在快照里时返回 false。
func (s TraeCNCreditsSnapshot) Pool(kind string) (TraeCNCreditsPool, bool) {
	kind = NormalizeTraeCNCreditsPoolKind(kind)
	for _, pool := range s.Pools {
		if NormalizeTraeCNCreditsPoolKind(pool.Kind) == kind {
			return pool, true
		}
	}
	return TraeCNCreditsPool{}, false
}

// NormalizeTraeCNCreditsPoolKind 把未知取值收敛到 Code 池。
func NormalizeTraeCNCreditsPoolKind(kind string) string {
	if strings.EqualFold(strings.TrimSpace(kind), TraeCNCreditsPoolWork) {
		return TraeCNCreditsPoolWork
	}
	return TraeCNCreditsPoolCode
}

// TraeCNCreditsWorkPoolDisabled 允许运维只查 IDE 侧权益。
func TraeCNCreditsWorkPoolDisabled() bool {
	return strings.TrimSpace(os.Getenv(traeCNCreditsWorkPoolDisableEnv)) == "1"
}

func traeCNCreditsPoolLabel(kind string) string {
	if NormalizeTraeCNCreditsPoolKind(kind) == TraeCNCreditsPoolWork {
		return "Work"
	}
	return "Code"
}

// QueryTraeCNCredits 查询积分，令牌与出口沿用账号配置。查询成功后会把两个端点的
// 剩余量写回账号，供推理请求决定走哪个积分池。
func QueryTraeCNCredits(ctx context.Context, store *auth.Store, account *auth.Account) (TraeCNCreditsSnapshot, error) {
	return queryTraeCNCredits(ctx, store, account, TraeCNCheckinHost)
}

func queryTraeCNCredits(ctx context.Context, store *auth.Store, account *auth.Account, host string) (TraeCNCreditsSnapshot, error) {
	if account == nil || !account.IsTraeCNAPI() {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("Trae CN 账号不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := newTraeCNCreditsRequest(ctx, store, account, host)
	if err != nil {
		return TraeCNCreditsSnapshot{}, err
	}
	// 默认按 Work 侧（req_source=2）取全集：它包含 IDE 侧看得见的通用包和 IDE 侧
	// 看不到的 Work 专属包。上游拒绝该视角时退回 IDE 侧，至少拿到 Code 池。
	raw, err := request.fetch(ctx, traeCNCreditsWorkReqSource)
	workScope := err == nil
	if err != nil {
		raw, err = request.fetch(ctx, traeCNCreditsCodeReqSource)
		if err != nil {
			return TraeCNCreditsSnapshot{}, err
		}
	}
	snapshot, err := parseTraeCNCredits(raw, time.Now())
	if err != nil {
		return TraeCNCreditsSnapshot{}, err
	}
	if !workScope && !TraeCNCreditsWorkPoolDisabled() {
		// IDE 视角拿不到 Work 专属包，明确标注而不是给出 0 余额。
		snapshot.Pools = append(snapshot.Pools, TraeCNCreditsPool{
			Kind:  TraeCNCreditsPoolWork,
			Error: "上游未返回 Work 侧权益",
		})
	}
	recordTraeCNCreditsBalance(account, snapshot)
	liftTraeCNCreditsCooldown(store, account)
	return snapshot, nil
}

// liftTraeCNCreditsCooldown 在额度恢复后立刻解冻账号：额度不足的冷却默认一小时，
// 如果管理台的余额查询已经看到积分回来了，就不该再等满一小时。
// 只解冻长冷却（额度不足），短限流冷却让它自然到期，避免提前解冻后又被上游拒。
func liftTraeCNCreditsCooldown(store *auth.Store, account *auth.Account) {
	if store == nil || account == nil {
		return
	}
	switch account.TraeCNCreditsState() {
	case auth.TraeCNCreditsStateOK, auth.TraeCNCreditsStateWorkOnly:
	default:
		return
	}
	account.Mu().RLock()
	cooling := account.Status == auth.StatusCooldown
	remaining := time.Until(account.CooldownUtil)
	account.Mu().RUnlock()
	if !cooling || remaining < 5*time.Minute {
		return
	}
	store.ClearCooldown(account)
	log.Printf("[TRAECN] stage=credits_recovered account=%d remaining_seconds=%d action=clear_cooldown", account.ID(), int(remaining/time.Second))
}

// recordTraeCNCreditsBalance 把查到的两个池剩余量写进账号，推理请求据此选端点。
func recordTraeCNCreditsBalance(account *auth.Account, snapshot TraeCNCreditsSnapshot) {
	if account == nil {
		return
	}
	codePool, codeOK := snapshot.Pool(TraeCNCreditsPoolCode)
	workPool, workOK := snapshot.Pool(TraeCNCreditsPoolWork)
	// 两个池都要有真实结果才写回：只拿到 IDE 侧时把 Work 记成 0，会让 auto 在账号
	// 其实有 Work 额度时永远不切池。
	if !codeOK || codePool.Error != "" || !workOK || workPool.Error != "" {
		return
	}
	account.SetTraeCNCreditsBalanceFromPools(codePool.Remaining, workPool.Remaining, codePool.UpdatedAt)
}

// traeCNCreditsRequest 固化一次查询的令牌、出口与端点。
type traeCNCreditsRequest struct {
	account     *auth.Account
	accessToken string
	endpoint    string
	proxyURL    string
	viaResin    bool
}

func newTraeCNCreditsRequest(ctx context.Context, store *auth.Store, account *auth.Account, host string) (traeCNCreditsRequest, error) {
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
		return traeCNCreditsRequest{}, fmt.Errorf("刷新 Trae CN 令牌失败")
	}
	_, accessToken := account.TraeCNCredentials()
	if accessToken == "" {
		return traeCNCreditsRequest{}, fmt.Errorf("Trae CN 访问令牌为空")
	}
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		host = TraeCNCheckinHost
	}
	endpoint := host + traeCNCreditsUsagePath
	viaResin := IsResinEnabledForContext(ctx) && account.ID() > 0
	if viaResin {
		platform := ResinPlatformFromContext(ctx)
		if platform == "" {
			platform = ResinPlatformForSessionFromContext(ctx, "")
		}
		endpoint = BuildReverseProxyURLForContext(ctx, endpoint, platform)
	}
	return traeCNCreditsRequest{account: account, accessToken: accessToken, endpoint: endpoint, proxyURL: proxyURL, viaResin: viaResin}, nil
}

func (r traeCNCreditsRequest) fetch(ctx context.Context, reqSource int) ([]byte, error) {
	body := fmt.Sprintf(`{"require_usage":true,"req_source":%d}`, reqSource)
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, r.endpoint, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建积分查询请求失败")
	}
	// pay 和签到接口均使用市场客户端身份，不能套用推理接口的 x-ide-token 头。
	req.Header = auth.TraeCNCheckinHeaders(r.account, r.accessToken, uuid.NewString())
	var resp *http.Response
	if r.viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(r.account))
		resp, err = getResinHTTPClient(r.account).Do(req)
	} else {
		resp, err = getPooledClient(r.account, r.proxyURL).Do(req)
	}
	if err != nil {
		return nil, fmt.Errorf("积分查询连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分查询返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("积分查询响应读取失败或超过大小限制")
	}
	return raw, nil
}

// parseTraeCNCredits 按权益包的 available_endpoint 把积分分成 Code / Work 两个池。
// 上游的 usage_summary 是两个池的合计，不能直接当单池余额用。
func parseTraeCNCredits(raw []byte, now time.Time) (TraeCNCreditsSnapshot, error) {
	if !gjson.ValidBytes(raw) {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("积分查询响应不是有效 JSON")
	}
	root := gjson.ParseBytes(raw)
	if code := root.Get("code"); code.Exists() && code.String() != "0" {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("积分查询返回业务错误")
	}
	// 非积分计费账号不能把美元额度误显示成积分。
	if root.Get("is_credits_billing").Type != gjson.True || root.Get("is_dollar_usage_billing").Bool() {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("该账号未返回积分计费数据")
	}
	packs := root.Get("user_entitlement_pack_list")
	if !packs.IsArray() {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("积分汇总缺少权益包列表")
	}
	totals := map[string]struct{ total, used float64 }{}
	seen := map[string]struct{}{}
	for _, entry := range packs.Array() {
		base := entry.Get("entitlement_base_info")
		limit := base.Get("quota.credits_limit")
		// 只统计真正的积分包：没有 credits_limit 的是功能权益（enable_solo_* 之类）。
		if limit.Type != gjson.Number && limit.Type != gjson.String {
			continue
		}
		limitValue, err := strconv.ParseFloat(limit.String(), 64)
		if err != nil || math.IsNaN(limitValue) || math.IsInf(limitValue, 0) || limitValue <= 0 {
			continue
		}
		usedValue := entry.Get("usage.credits_amount").Float()
		if math.IsNaN(usedValue) || math.IsInf(usedValue, 0) || usedValue < 0 {
			usedValue = 0
		}
		// 同一份权益包在两次 req_source 查询里会重复出现，按 entitlement_id 去重。
		id := base.Get("entitlement_id").String()
		if id == "" {
			id = strings.Join([]string{
				base.Get("product_id").String(),
				limit.String(),
				base.Get("start_time").String(),
			}, "|")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		kind := TraeCNCreditsPoolCode
		if int(base.Get("available_endpoint").Int()) == traeCNCreditsWorkEndpoint {
			kind = TraeCNCreditsPoolWork
		}
		bucket := totals[kind]
		bucket.total += limitValue
		bucket.used += math.Min(usedValue, limitValue)
		totals[kind] = bucket
	}
	if len(totals) == 0 {
		return TraeCNCreditsSnapshot{}, fmt.Errorf("积分汇总缺少有效的权益包余额")
	}
	snapshot := TraeCNCreditsSnapshot{}
	for _, kind := range []string{TraeCNCreditsPoolCode, TraeCNCreditsPoolWork} {
		bucket, ok := totals[kind]
		if !ok {
			continue
		}
		pool := TraeCNCreditsPool{
			Kind:      kind,
			Total:     bucket.total,
			Used:      bucket.used,
			Remaining: math.Max(0, bucket.total-bucket.used),
			UpdatedAt: now.UTC(),
		}
		pool.UsedPercent = math.Min(100, bucket.used/bucket.total*100)
		snapshot.Pools = append(snapshot.Pools, pool)
	}
	return snapshot, nil
}
