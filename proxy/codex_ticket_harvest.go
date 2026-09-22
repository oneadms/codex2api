package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 后台自动打票器：用合成 ping 请求向 Codex 上游铸造 X-Codex-Turn-State 门票，
// 按 (账号, 模型) 分别持有，业务请求上由 proxy/codex_turn_state_inject.go 注入。
//
// 设计要点（与 sub2api 的 openai_codex_ticket.go 同源，但接进 codex2api 既有设施）：
//
//  1. 采票使用专用代理和全新会话，业务请求沿用保存的出口与会话快照。
//     轮换代理地址不代表固定出口 IP；定向节点只有通过独立 Selector 才能锁定。
//  2. 只补缺与临期：已有可用门票且未进入重打窗口的 (账号, 模型) 直接跳过。
//  3. 失败冷却：按账号+模型记 NextProbeAt，避免一个坏号被反复打。
//  4. 必须读到成功终态才落库：HTTP 200 不等于铸造成功，response.failed / 无终态
//     都不算，避免把一张假票塞进注入路径换来一次必然失败的上游请求。
//  5. 落库 + 即时发布：db.UpdateCredentials 持久化（SQLite 自动走事务合并），
//     store.PublishCodexTicket 立即更新内存；全量 reconcile 兜底 5 分钟收敛。
//
// 打票是尽力而为的后台行为：任何一轮失败都只记日志、绝不回灌下游。

// codexTicketProbeURLForTest 供测试替换上游端点；生产为空。
var codexTicketProbeURLForTest string

// codexTicketProbeTimeoutForTest 供测试缩短探测超时；生产为零值。
var codexTicketProbeTimeoutForTest time.Duration

func codexTicketUpstreamEndpoint() string {
	if codexTicketProbeURLForTest != "" {
		return codexTicketProbeURLForTest
	}
	return CodexBaseURL + "/responses"
}

// codexTicketHarvestProbeBody 构造一条最小的铸造 ping 回合。store:false 避免把探测
// 写进用户历史；stream:true 与业务同构，便于读到 response.completed 终态再落库。
func codexTicketHarvestProbeBody(model string) []byte {
	body := []byte(`{"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	updated, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return updated
}

// ==================== 探测 ====================

// codexTicketHarvestResult 是一次打票的结果。Err 非空表示本轮未取到可用门票。
type codexTicketHarvestResult struct {
	State          string
	Cookies        codexTicketCookies
	Binding        *auth.CodexTicket
	HTTPStatus     int
	ObservedLength int
	// Result 是探测结论，取值与 auth.CodexTicketProbeSummary.Result 一致：
	// success / token_error / error / invalid_state / response_incomplete_or_error。
	Result string
	Err    error
}

// fireCodexTicketHarvest 用专用打票代理向 Codex 上游铸造一张门票。出站头复用
// applyCodexRequestHeaders 的伪装逻辑，保证探测请求与业务请求形状一致。
//
// 门票只有在读到 response.completed / response.incomplete 成功终态后才可信。
func fireCodexTicketHarvest(ctx context.Context, account *auth.Account, model, proxyURL string, attemptTimeout time.Duration) (result codexTicketHarvestResult) {
	fail := func(result string, err error) codexTicketHarvestResult {
		return codexTicketHarvestResult{Result: result, Err: err}
	}
	if account == nil {
		return fail("error", fmt.Errorf("账号为空"))
	}
	if !account.CanHarvestCodexTicket(time.Now()) {
		return fail("error", fmt.Errorf("账号不支持 Codex 自动打票或当前不可用"))
	}
	accessToken := account.GetAccessToken()
	if strings.TrimSpace(accessToken) == "" {
		return fail("token_error", fmt.Errorf("账号无 access token"))
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return fail("error", fmt.Errorf("模型为空"))
	}
	if strings.TrimSpace(proxyURL) == "" {
		// 绝不回退到业务代理：那会污染账号的住宅出口。
		return fail("error", fmt.Errorf("未配置打票代理"))
	}
	client, err := auth.BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return fail("error", fmt.Errorf("打票代理不可用: %w", err))
	}
	if attemptTimeout <= 0 {
		attemptTimeout = time.Duration(auth.CodexTicketDefaultProbeTimeoutSecs) * time.Second
	}
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	release, err := mihomo.Lease(attemptCtx, proxyURL)
	if err != nil {
		return fail("error", fmt.Errorf("采票节点不可用"))
	}
	defer func() { release(result.Result == "success") }()

	body := codexTicketHarvestProbeBody(model)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, codexTicketUpstreamEndpoint(), bytes.NewReader(body))
	if err != nil {
		return fail("error", err)
	}
	sessionID := NewUpstreamSessionUUID()
	applyCodexRequestHeaders(req, account, accessToken, sessionID, "", nil, nil)
	applyCodexTicketSession(req.Header, sessionID)
	// 与 sub2api 一致：只复用本账号、本模型的有效 Cookie，不继承自定义 Cookie 或旧票据。
	req.Header.Del("Cookie")
	if cookie := account.CodexHarvestCookie(model, time.Now()); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	req.Header.Del(codexTurnStateHeader)
	req.Header.Set("Accept", "text/event-stream")

	probe, err := performCodexHarvestTransport(req, body, client)
	if err != nil {
		return fail("error", fmt.Errorf("打票请求失败: %w", err))
	}
	status := probe.Status
	if status != http.StatusOK {
		return codexTicketHarvestResult{
			HTTPStatus: status,
			Result:     harvestResultForHTTPStatus(status),
			Err:        fmt.Errorf("上游状态 %d", status),
		}
	}
	state, cookies := probe.State, probe.Cookies
	if !probe.Completed {
		return codexTicketHarvestResult{ObservedLength: len(state), HTTPStatus: status, Result: harvestResultForTerminal(probe.Terminal), Err: fmt.Errorf("打票响应未到成功终态")}
	}
	settings := auth.ConfiguredCodexTicketSettings()
	planType := account.GetPlanType()
	targetLen := auth.CodexTicketTargetLength(planType, settings.TargetLength)
	// Explain rejections without logging the opaque ticket, access token or proxy.
	// Capture the plan/config once so the diagnostic matches the actual check.
	invalidState := func(validation string, err error) codexTicketHarvestResult {
		return codexTicketHarvestResult{
			ObservedLength: len(state),
			HTTPStatus:     status,
			Result:         "invalid_state",
			Err: fmt.Errorf("%w (validation=%s plan_type=%q actual_length=%d expected_length=%d configured_length=%d)",
				err, validation, planType, len(state), targetLen, settings.TargetLength),
		}
	}
	if state == "" {
		return invalidState("missing", fmt.Errorf("响应未回带 turn state"))
	}
	shape, err := auth.ParseCodexTicketShape(state)
	if err != nil {
		return invalidState("shape", fmt.Errorf("响应门票信封解析失败: %w", err))
	}
	if !strings.HasPrefix(state, auth.CodexTicketStatePrefix) {
		return invalidState("prefix", fmt.Errorf("响应门票前缀不符合要求"))
	}
	if len(state) != targetLen {
		return invalidState("length", fmt.Errorf("响应门票长度与账号套餐或目标长度配置不匹配"))
	}
	if cookies.Header == "" {
		return invalidState("cookie", fmt.Errorf("响应门票缺少配套的有效 Cookie"))
	}
	now := time.Now()
	ticket := &auth.CodexTicket{
		State: state, Cookie: cookies.Header, Length: len(state), IssuedAt: shape.IssuedAt,
		ExpiresAt: auth.CodexTicketExpiry(now, shape.IssuedAt, time.Duration(settings.TTLSeconds)*time.Second),
	}
	if !cookies.ExpiresAt.IsZero() && cookies.ExpiresAt.Before(ticket.ExpiresAt) {
		ticket.ExpiresAt = cookies.ExpiresAt
	}
	if !ticket.Valid(now, targetLen) {
		return invalidState("time", fmt.Errorf("响应门票时间校验失败: issued_at=%s expires_at=%s now=%s age_seconds=%d",
			shape.IssuedAt.UTC().Format(time.RFC3339), ticket.ExpiresAt.UTC().Format(time.RFC3339),
			now.UTC().Format(time.RFC3339), int64(now.Sub(shape.IssuedAt)/time.Second)))
	}
	return codexTicketHarvestResult{HTTPStatus: status, State: state, Cookies: cookies,
		Binding: &auth.CodexTicket{HarvestProxyURL: proxyURL, HarvestSessionID: sessionID}, Result: "success"}
}

func harvestResultForHTTPStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "token_error"
	default:
		return "error"
	}
}

func harvestResultForTerminal(gotTerminal bool) string {
	if !gotTerminal {
		return "response_incomplete_or_error"
	}
	return "error"
}

// validateCodexHarvestStream 读流式响应直到终态。返回值 gotTerminal 表示是否读到
// response.completed / response.incomplete；ok 表示整轮成功。
func validateCodexHarvestStream(r io.Reader) (gotTerminal, ok bool) {
	if r == nil {
		return false, false
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		switch gjson.Get(data, "type").String() {
		case "response.completed", "response.incomplete":
			return true, true
		case "response.failed", "error":
			return gotTerminal, false
		}
	}
	// 读到 EOF 都没等到终态（连接被掐断、上游只发了一半）——不可信。
	return gotTerminal, false
}

// ==================== 调度 ====================

// codexTicketHarvestKick 是"立刻打一轮"的信号。启用门控的那一刻账号上还没有票，
// 而 FailClosed 会把门控模型上的请求直接拒掉——等到下一个探测周期才补票，等于让
// 运维在保存设置后先看几分钟 503。保存设置时踢一脚，把这段空窗压到一个请求往返。
var codexTicketHarvestKick = make(chan struct{}, 1)

// KickCodexTicketHarvest 请求立即打一轮票（非阻塞，已有待处理信号时直接返回）。
func KickCodexTicketHarvest() {
	select {
	case codexTicketHarvestKick <- struct{}{}:
	default:
	}
}

// StartCodexTicketHarvestLoop 启动后台打票循环。配置（开关/代理/名单/周期）每个
// 周期重新读取，新配置从下一轮生效；ctx 取消即退出。任何错误都不影响服务启动。
func StartCodexTicketHarvestLoop(ctx context.Context, db *database.DB, store *auth.Store) {
	if db == nil || store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 注册反馈回写目标：业务请求顺手采纳上游新票、撤销被拒的票。
	registerCodexTicketFeedback(db, store)
	db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, taskCancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(ctx, taskCancel)
		defer func() {
			stopParent()
			taskCancel()
		}()
		runCodexTicketHarvestOnce(taskCtx, db, store)
		for {
			interval := auth.CodexTicketProbeInterval()
			var wake <-chan struct{}
			if manager := codexHarvester.Load(); manager != nil {
				controls, _, _ := manager.Controls.Controls(taskCtx)
				interval = time.Duration(controls.Speed.RoundIntervalSeconds) * time.Second
				wake = manager.Controls.Wake()
				next := time.Now().Add(interval)
				manager.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) { r.NextRoundAt = &next })
			}
			timer := time.NewTimer(interval)
			select {
			case <-taskCtx.Done():
				timer.Stop()
				return
			case <-codexTicketHarvestKick:
			case <-wake:
			case <-timer.C:
			}
			timer.Stop()
			runCodexTicketHarvestOnce(taskCtx, db, store)
		}
	})
}

// runCodexTicketHarvestOnce 打一轮票：门控齐备时逐个账号、逐个门控模型判断。
func runCodexTicketHarvestOnce(ctx context.Context, db *database.DB, store *auth.Store) {
	if manager := codexHarvester.Load(); manager != nil && manager.DB == db && manager.Store == store {
		manager.RunRound(ctx)
		return
	}
	if !auth.CodexTicketGateEnabled() {
		return
	}
	settings := auth.ConfiguredCodexTicketSettings()
	refreshCodexTickets(ctx, db, store, settings)
}

// refreshCodexTickets 逐个账号、逐个门控模型打票。已有可用的账号本地门票且未进入重打窗口的
// 直接跳过，并将其登记到同套餐共享池；本地票不可用时仍尝试为账号自己打票。
func refreshCodexTickets(ctx context.Context, db *database.DB, store *auth.Store, settings auth.CodexTicketSettings) {
	proxyURL := strings.TrimSpace(settings.HarvestProxyURL)
	if proxyURL == "" || len(settings.Models) == 0 {
		return
	}
	// 每轮有请求数上限，避免名单很长或账号很多时把打票代理打爆。
	budget := settings.MaxProbesPerRound
	if budget <= 0 {
		budget = auth.CodexTicketDefaultMaxProbesPerRound
	}
	timeout := time.Duration(settings.ProbeTimeoutSeconds) * time.Second
	if codexTicketProbeTimeoutForTest > 0 {
		timeout = codexTicketProbeTimeoutForTest
	}
	for _, acc := range store.Accounts() {
		if ctx.Err() != nil || budget <= 0 {
			return
		}
		if !acc.CanHarvestCodexTicket(time.Now()) {
			continue
		}
		for _, model := range settings.Models {
			if ctx.Err() != nil || budget <= 0 {
				return
			}
			now := time.Now()
			targetLen := auth.CodexTicketTargetLengthFor(acc.GetPlanType())
			if ticket := acc.CodexTicketForModel(model, now, targetLen); ticket != nil {
				auth.PublishCodexTicketToSharedPool(acc, ticket)
				if !ticket.NeedsRefresh(now, auth.CodexTicketRefreshBefore()) {
					continue
				}
			}
			if acc.CodexTicketProbeCoolingDown(model, now) {
				continue
			}
			harvestOneTicket(ctx, db, store, acc, model, proxyURL, timeout, &budget)
		}
	}
}

// probeCodexTicketWithRefresh prepares credentials before probing and permits
// one forced refresh after a 401. Both HTTP attempts count toward the round's
// budget. A 403 may be a proxy/WAF rejection and must not rotate OAuth tokens.
func probeCodexTicketWithRefresh(ctx context.Context, store *auth.Store, acc *auth.Account, model, proxyURL string, timeout time.Duration, budget *int) codexTicketHarvestResult {
	if budget == nil || *budget <= 0 {
		return codexTicketHarvestResult{Result: "error", Err: fmt.Errorf("本轮打票次数已用完")}
	}
	if timeout <= 0 {
		timeout = time.Duration(auth.CodexTicketDefaultProbeTimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := store.EnsureCodexTicketAccessToken(ctx, acc, false); err != nil {
		return codexTicketHarvestResult{Result: "token_error", Err: err}
	}
	if err := waitCodexHarvestRequest(ctx); err != nil {
		return codexTicketHarvestResult{Result: "error", Err: err}
	}
	*budget--
	result := fireCodexTicketHarvest(ctx, acc, model, proxyURL, timeout)
	if result.HTTPStatus != http.StatusUnauthorized || ctx.Err() != nil {
		return result
	}
	if err := store.EnsureCodexTicketAccessToken(ctx, acc, true); err != nil {
		result.Err = fmt.Errorf("%v；%w", result.Err, err)
		return result
	}
	if *budget <= 0 {
		result.Err = fmt.Errorf("%v；凭据已刷新，本轮打票次数已用完，冷却后重试", result.Err)
		return result
	}
	if err := waitCodexHarvestRequest(ctx); err != nil {
		return codexTicketHarvestResult{Result: "error", Err: err}
	}
	*budget--
	return fireCodexTicketHarvest(ctx, acc, model, proxyURL, timeout)
}

// harvestOneTicket 为 (账号, 模型) 铸造并发布一张门票。
func harvestOneTicket(ctx context.Context, db *database.DB, store *auth.Store, acc *auth.Account, model, proxyURL string, timeout time.Duration, budget *int) bool {
	if !acc.CanHarvestCodexTicket(time.Now()) {
		return false
	}
	checkedAt := time.Now()
	res := probeCodexTicketWithRefresh(ctx, store, acc, model, proxyURL, timeout, budget)
	// Persist before declaring success: a failed write must not leave a success
	// summary or log while the fail-closed gate still has no usable ticket.
	if res.Result == "success" && !publishCodexTicket(db, store, acc, model, res.State, codexTicketIssuedAt(res.State), res.Cookies, res.Binding) {
		res.Result = "error"
		res.Err = fmt.Errorf("门票落库失败")
	}
	summary := &auth.CodexTicketProbeSummary{
		Result:     res.Result,
		HTTPStatus: res.HTTPStatus,
		CheckedAt:  checkedAt,
	}
	if res.Result != "success" {
		next := checkedAt.Add(auth.CodexTicketCooldown())
		summary.NextProbeAt = &next
		store.PublishCodexTicketProbe(acc.ID(), model, summary)
		log.Printf("[codex-ticket] 账号 %d %s 打票未成功: result=%s status=%d err=%v", acc.ID(), model, res.Result, res.HTTPStatus, res.Err)
		return false
	}
	store.PublishCodexTicketProbe(acc.ID(), model, summary)
	log.Printf("[codex-ticket] 账号 %d %s 打票成功", acc.ID(), model)
	return true
}

// codexTicketIssuedAt 从门票信封里取上游签发时刻；解析不出返回零值，发布前仍须通过信封校验。
func codexTicketIssuedAt(state string) time.Time {
	shape, err := auth.ParseCodexTicketShape(state)
	if err != nil {
		return time.Time{}
	}
	return shape.IssuedAt
}
