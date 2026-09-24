package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// 真实上游探针：默认跳过。两个积分池的余额只在 pay 接口可见，真正扣哪一个由推理
// 请求本身决定，所以这里逐候选身份发一次 tiny 推理请求，再对比余额变化。
//
//	TRAECN_LIVE_CREDITS_FILE=/path/traecn-accounts-YYYY.json \
//	  go test ./proxy/ -run TestLiveTraeCN -v -count=1
//
// 每个候选都会真的消耗账号积分（十几 token）。
func TestLiveTraeCNWorkPoolProbe(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON> to probe the live Work pool")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	ctx := t.Context()

	before, err := traeCNProbeEndpointPools(ctx, account)
	if err != nil {
		t.Fatalf("读取双池失败: %v", err)
	}
	t.Logf("探针前: %s", formatTraeCNProbeEndpointPools(before))

	for _, candidate := range traeCNProbeCandidates() {
		summary, probeErr := traeCNProbeChat(ctx, account, candidate)
		if probeErr != nil {
			t.Logf("[%s] function=%s 请求失败: %v", candidate.name, candidate.function, probeErr)
		} else {
			t.Logf("[%s] function=%s %s", candidate.name, candidate.function, summary)
		}
		time.Sleep(2 * time.Second)
		after, poolErr := traeCNProbeEndpointPools(ctx, account)
		if poolErr != nil {
			t.Logf("[%s] 复读余额失败: %v", candidate.name, poolErr)
			continue
		}
		t.Logf("[%s] 扣费: %s", candidate.name, traeCNProbePoolDiff(before, after))
		before = after
	}
	t.Logf("探针后: %s", formatTraeCNProbeEndpointPools(before))
}

// TestLiveTraeCNGatewayBodyOnWorkPool 用网关真实构造的 llm_utils_chat 请求体验证
// Work 池：先按 IDE 身份发（现在应当 4008），再补上 access_type=1 走 Work 池。
//
//	TRAECN_LIVE_CREDITS_FILE=... go test ./proxy/ -run TestLiveTraeCNGatewayBodyOnWorkPool -v
func TestLiveTraeCNGatewayBodyOnWorkPool(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	ctx := t.Context()

	before, err := traeCNProbeEndpointPools(ctx, account)
	if err != nil {
		t.Fatalf("读取双池失败: %v", err)
	}
	t.Logf("探针前: %s", formatTraeCNProbeEndpointPools(before))

	// 用目录里真实存在的 config_name，走网关真实的请求体构造。
	body := mustTraeCNProbeBody(t, "Doubao_1_6")
	account.TraeCNCreditsPool = auth.TraeCNCreditsPoolAuto
	t.Logf("auto 模式初始判定: 走 Work 池 = %v", account.TraeCNShouldUseWorkPool())

	// 第一跳：余额未知，先按 Code 池发（access_type 不带）。额度耗尽时上游会在
	// SSE 里回 4008，并顺带给出 cn_credits_remain_info。
	rawBody, status, err := traeCNProbeRawBody(ctx, account, applyTraeCNAccessType(body, traeCNAccessTypeForAccount(account)))
	if err != nil {
		t.Fatalf("第一跳失败: %v", err)
	}
	t.Logf("[auto_first_hop] HTTP %d output=%q quota_error=%v remain=%s", status,
		clipTraeCNProbe(collectTraeCNProbeOutput(rawBody), 60), IsTraeCNQuotaError(rawBody),
		clipTraeCNProbe(gjson.Get(strings.Join(strings.Fields(string(rawBody)), " "), "cn_credits_remain_info").Raw, 60))

	// 网关在流上扫描到分池余额后写回账号。
	recordTraeCNCreditsRemainFromPayload(account, rawBody)
	balance := account.TraeCNCreditsBalance()
	t.Logf("[auto_first_hop] 学到的余额: code=%.2f work=%.2f", balance.CodeRemaining, balance.WorkRemaining)
	if !account.TraeCNShouldUseWorkPool() {
		t.Fatalf("余额 code=%.2f work=%.2f 时 auto 应当切到 Work 池", balance.CodeRemaining, balance.WorkRemaining)
	}

	// 第二跳：同一个账号，此时网关会自动带上 access_type=1。
	secondBody := applyTraeCNAccessType(body, traeCNAccessTypeForAccount(account))
	if gjson.GetBytes(secondBody, "access_type").Int() != int64(auth.TraeCNWorkAccessType) {
		t.Fatalf("auto 没有切到 Work 池: %s", secondBody)
	}
	rawBody, status, err = traeCNProbeRawBody(ctx, account, secondBody)
	if err != nil {
		t.Fatalf("第二跳失败: %v", err)
	}
	text := collectTraeCNProbeOutput(rawBody)
	t.Logf("[auto_second_hop] HTTP %d output=%q quota_error=%v", status, clipTraeCNProbe(text, 80), IsTraeCNQuotaError(rawBody))
	if strings.TrimSpace(text) == "" {
		t.Fatalf("Work 池第二跳没有产出正文: %s", clipTraeCNProbe(strings.Join(strings.Fields(string(rawBody)), " "), 300))
	}
	time.Sleep(2 * time.Second)
	after, poolErr := traeCNProbeEndpointPools(ctx, account)
	if poolErr != nil {
		t.Fatalf("复读余额失败: %v", poolErr)
	}
	t.Logf("[auto_second_hop] 扣费: %s", traeCNProbePoolDiff(before, after))
}

// traeCNProbeRawBody 发一次请求并原样返回 SSE 文本。
func traeCNProbeRawBody(ctx context.Context, account *auth.Account, body []byte) ([]byte, int, error) {
	host, accessToken := account.TraeCNCredentials()
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, host+auth.TraeCNChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	return raw, resp.StatusCode, nil
}

// traeCNProbeChatBody 发送现成请求体并汇总 SSE。
func traeCNProbeChatBody(ctx context.Context, account *auth.Account, body []byte) (string, int, error) {
	host, accessToken := account.TraeCNCredentials()
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, host+auth.TraeCNChatPath, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	text := strings.Join(strings.Fields(string(raw)), " ")
	summary := "text=" + strconv.Quote(clipTraeCNProbe(collectTraeCNProbeOutput(raw), 80))
	if collectTraeCNProbeOutput(raw) == "" {
		summary = "raw=" + clipTraeCNProbe(text, 300)
	}
	return summary, resp.StatusCode, nil
}

// TestLiveTraeCNWorkStreamTail 抓完整的 Work 池（access_type=1）响应，打印事件序列
// 和结尾字节，并把这批原始字节喂给网关的 canonical 转换器，看它最终给出的是
// response.completed 还是 upstream_stream_break。
func TestLiveTraeCNWorkStreamTail(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	body := applyTraeCNAccessType(mustTraeCNProbeBody(t, "Doubao_1_6"), auth.TraeCNWorkAccessType)
	raw, status, err := traeCNProbeRawBody(t.Context(), account, body)
	if err != nil {
		t.Fatalf("Work 流抓取失败: %v", err)
	}
	names := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "event:") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	t.Logf("HTTP %d bytes=%d events=%v", status, len(raw), names)
	tail := string(raw)
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
	}
	t.Logf("原始结尾: %s", strings.ReplaceAll(tail, "\n", "\\n"))

	canonical := traeCNCanonicalStream(io.NopCloser(bytes.NewReader(raw)), "Doubao_1_6")
	converted, err := io.ReadAll(canonical)
	if err != nil {
		t.Fatalf("canonical 转换失败: %v", err)
	}
	types := []string{}
	for _, line := range strings.Split(string(converted), "\n") {
		if strings.HasPrefix(line, "data:") {
			types = append(types, gjson.Get(strings.TrimPrefix(line, "data:"), "type").String())
		}
	}
	t.Logf("canonical bytes=%d types=%v", len(converted), types)
	convTail := string(converted)
	if len(convTail) > 400 {
		convTail = convTail[len(convTail)-400:]
	}
	t.Logf("canonical 结尾: %s", strings.ReplaceAll(convTail, "\n", "\\n"))
}

// TestLiveTraeCNWorkStreamWithTools 验证 Work 池交付可执行的天气调用及城市参数。
// HTTP 200 或事件名本身不代表工具调用成功。
func TestLiveTraeCNWorkStreamWithTools(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	tools := `"tools":[{"type":"function","name":"get_weather","description":"look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":"auto",`
	canonical := []byte(`{"model":"Doubao_1_6",` + tools + `"stream":true,"max_output_tokens":400,"input":[{"role":"user","content":[{"type":"input_text","text":"用 get_weather 查一下上海的天气，然后告诉我结果。"}]}]}`)
	body, model, bridges, contracts, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	payload := applyTraeCNAccessType(body, auth.TraeCNWorkAccessType)
	raw, status, err := traeCNProbeRawBody(t.Context(), account, payload)
	if err != nil {
		t.Fatalf("Work 抓取失败: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("Work HTTP %d", status)
	}
	canonicalOut := traeCNCanonicalStreamForTools(io.NopCloser(bytes.NewReader(raw)), model, bridges, contracts)
	converted, err := io.ReadAll(canonicalOut)
	if err != nil {
		t.Fatalf("canonical 转换失败: %v", err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, converted), "response.completed")
	if !ok {
		t.Fatal("Work did not complete with a usable response")
	}
	call := completed.Get(`response.output.#(type=="function_call")`)
	args := call.Get("arguments").String()
	city := gjson.Get(args, "city").String()
	if call.Get("name").String() != "get_weather" || call.Get("call_id").String() == "" || call.Get("status").String() != "completed" || !gjson.Valid(args) || (city != "上海" && city != "上海市" && !strings.EqualFold(city, "Shanghai")) {
		t.Fatal("expected executable get_weather(city=上海) was not received")
	}
	t.Logf("HTTP %d bytes=%d verified=get_weather(city=上海)", status, len(raw))
}

// collectTraeCNProbeOutput 取 SSE 里的正文增量（IDE 事件与 Work 事件都认）。
func collectTraeCNProbeOutput(raw []byte) string {
	out := strings.Builder{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		out.WriteString(gjson.Get(payload, "choices.0.delta.content").String())
		out.WriteString(gjson.Get(payload, "response").String())
	}
	return out.String()
}

// TestLiveTraeCNWorkCatalogAndBody 找 Work 端点可用的模型目录，并验证网关请求体在
// Work 池下的形状（access_type=1 时带 model/config_name 会 4001）。
func TestLiveTraeCNWorkCatalogAndBody(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	ctx := t.Context()

	for _, accessType := range []int{0, auth.TraeCNWorkAccessType} {
		body, _ := json.Marshal(map[string]any{
			"functions":           []string{"chat_v3", "solo_agent", "solo_work_lite", "solo_agent_lite"},
			"agent_type":          "solo_work_lite",
			"current_config_info": map[string]any{"config_name": "", "is_custom_model": false},
			"mode_type":           0,
			"access_type":         accessType,
			"show_custom_model":   true,
		})
		raw, status, err := traeCNProbePost(ctx, account, "/api/ide/v1/batch_get_detail_param", body)
		if err != nil {
			t.Errorf("catalog access_type=%d: %v", accessType, err)
			continue
		}
		names := []string{}
		gjson.GetBytes(raw, "function_configs").ForEach(func(key, value gjson.Result) bool {
			value.Get("config_info_list").ForEach(func(_, item gjson.Result) bool {
				names = append(names, value.Get("function").String()+":"+item.Get("config_name").String())
				return len(names) < 12
			})
			return len(names) < 12
		})
		t.Logf("catalog access_type=%d HTTP %d functions=%v names=%v", accessType, status, gjson.GetBytes(raw, "function_configs.#.function").Raw, names)
	}

	canonical := []byte(`{"model":"deepseek-v3","stream":true,"max_output_tokens":16,"input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	withModel, _, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatal(err)
	}
	bare, _, err := buildTraeCNRequestBody([]byte(`{"stream":true,"max_output_tokens":16,"input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"work_with_model", applyTraeCNAccessType(withModel, auth.TraeCNWorkAccessType)},
		{"work_without_model", applyTraeCNAccessType(bare, auth.TraeCNWorkAccessType)},
		{"work_minimal", []byte(`{"function":"chat_v3","stream":true,"max_tokens":16,"access_type":1,"messages":[{"role":"user","content":[{"type":"text","text":"ping"}]}]}`)},
		// 目录里真实存在的 config_name：网关生产中就是这么发的。
		{"work_valid_model", applyTraeCNAccessType(mustTraeCNProbeBody(t, "Doubao_1_6"), auth.TraeCNWorkAccessType)},
		{"ide_valid_model", mustTraeCNProbeBody(t, "Doubao_1_6")},
	} {
		raw, status, err := traeCNProbeChatBody(ctx, account, tc.body)
		if err != nil {
			t.Errorf("[%s] %v", tc.name, err)
			continue
		}
		t.Logf("[%s] HTTP %d body=%s %s", tc.name, status, clipTraeCNProbe(string(tc.body), 160), raw)
	}
}

// mustTraeCNProbeBody 用目录里真实存在的 config_name 造一个网关形状的请求体。
func mustTraeCNProbeBody(t *testing.T, configName string) []byte {
	t.Helper()
	canonical := []byte(`{"model":"` + configName + `","stream":true,"max_output_tokens":16,"input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	body, _, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	return body
}

// traeCNProbePost 直接向上游某个路径 POST，返回原始响应。
func traeCNProbePost(ctx context.Context, account *auth.Account, path string, body []byte) ([]byte, int, error) {
	host, accessToken := account.TraeCNCredentials()
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, host+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// TestLiveTraeCNCreditsRaw 打印两个 req_source 的权益包（脱敏），用于确认分包规则。
func TestLiveTraeCNCreditsRaw(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	ctx := t.Context()
	for _, reqSource := range []int{traeCNCreditsCodeReqSource, traeCNCreditsWorkReqSource} {
		raw, err := traeCNProbeCreditsRaw(ctx, account, reqSource)
		if err != nil {
			t.Errorf("req_source=%d: %v", reqSource, err)
			continue
		}
		t.Logf("req_source=%d summary=%v", reqSource, gjson.GetBytes(raw, "usage_summary").Raw)
		for index, entry := range gjson.GetBytes(raw, "user_entitlement_pack_list").Array() {
			base := entry.Get("entitlement_base_info")
			t.Logf("req_source=%d pack[%d] endpoint=%v ent_id=%v product_id=%v limit=%v used=%v name=%v",
				reqSource, index, base.Get("available_endpoint").Raw, base.Get("entitlement_id").String(),
				base.Get("product_id").Raw, base.Get("quota.credits_limit").Raw,
				entry.Get("usage.credits_amount").Raw, base.Get("product_extra.package_extra.package_name").String())
		}
	}
}

type traeCNProbeCandidate struct {
	name     string
	function string
	extra    map[string]any
}

// traeCNProbeCandidates 覆盖 IDE 基线身份与 Work 侧的各种候选字段组合。
func traeCNProbeCandidates() []traeCNProbeCandidate {
	return []traeCNProbeCandidate{
		{name: "ide_baseline", function: "chat_v3"},
		{name: "req_source_2", function: "chat_v3", extra: map[string]any{"req_source": 2}},
		{name: "access_type_1", function: "chat_v3", extra: map[string]any{"access_type": 1}},
		{name: "access_type_2", function: "chat_v3", extra: map[string]any{"access_type": 2}},
		{name: "client_type_lite", function: "chat_v3", extra: map[string]any{"client_type": "lite"}},
		{name: "is_remote_req", function: "chat_v3", extra: map[string]any{"is_remote_req": true}},
		{name: "agent_type_work_lite", function: "chat_v3", extra: map[string]any{"agent_type": "solo_work_lite"}},
		{name: "mode_type_1", function: "chat_v3", extra: map[string]any{"mode_type": 1}},
		{name: "solo_coder_access_type_1", function: "solo_coder", extra: map[string]any{"access_type": 1}},
		{name: "solo_coder_remote", function: "solo_coder", extra: map[string]any{"is_remote_req": true, "access_type": 1, "agent_type": "solo_work_lite"}},
	}
}

func loadTraeCNProbeAccount(t *testing.T, path string) *auth.Account {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取账号导出失败: %v", err)
	}
	var payload struct {
		Accounts []struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Host         string `json:"host"`
			ExpiresAt    string `json:"expires_at"`
			MachineID    string `json:"machine_id"`
			DeviceID     string `json:"device_id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Accounts) == 0 {
		t.Fatalf("解析账号导出失败: %v", err)
	}
	source := payload.Accounts[0]
	account := &auth.Account{
		UpstreamType:          auth.UpstreamTraeCN,
		AccessToken:           strings.TrimSpace(source.AccessToken),
		RefreshToken:          strings.TrimSpace(source.RefreshToken),
		TraeCNHost:            strings.TrimSpace(source.Host),
		TraeCNDeviceMachineID: strings.TrimSpace(source.MachineID),
		TraeCNDeviceID:        strings.TrimSpace(source.DeviceID),
	}
	if expires, err := time.Parse(time.RFC3339, strings.TrimSpace(source.ExpiresAt)); err == nil {
		account.ExpiresAt = expires
	}
	if account.AccessToken == "" || account.RefreshToken == "" {
		t.Fatal("导出文件里没有可用的 access_token / refresh_token")
	}
	return account
}

// traeCNProbeEndpointPools 按权益包的 available_endpoint 汇总两个池：
// 0 = IDE/Code 端点，1 = Work 端点。两个 req_source 的包并起来看，
// 避免漏掉 IDE 侧看不到的 Work 专属包。
func traeCNProbeEndpointPools(ctx context.Context, account *auth.Account) (map[int][3]float64, error) {
	const (
		limitIndex = 0
		usedIndex  = 1
		leftIndex  = 2
	)
	type probePack struct {
		endpoint int
		limit    float64
		used     float64
	}
	packs := map[string]probePack{}
	for _, reqSource := range []int{traeCNCreditsCodeReqSource, traeCNCreditsWorkReqSource} {
		raw, err := traeCNProbeCreditsRaw(ctx, account, reqSource)
		if err != nil {
			return nil, err
		}
		for _, entry := range gjson.GetBytes(raw, "user_entitlement_pack_list").Array() {
			base := entry.Get("entitlement_base_info")
			limit := base.Get("quota.credits_limit")
			if !limit.Exists() || limit.Float() <= 0 {
				continue
			}
			id := base.Get("entitlement_id").String()
			if id == "" {
				id = base.Get("product_id").String() + "|" + limit.String()
			}
			packs[id] = probePack{
				endpoint: int(base.Get("available_endpoint").Int()),
				limit:    limit.Float(),
				used:     entry.Get("usage.credits_amount").Float(),
			}
		}
	}
	result := map[int][3]float64{}
	for _, item := range packs {
		row := result[item.endpoint]
		row[limitIndex] += item.limit
		row[usedIndex] += item.used
		row[leftIndex] = row[limitIndex] - row[usedIndex]
		result[item.endpoint] = row
	}
	return result, nil
}

func formatTraeCNProbeEndpointPools(pools map[int][3]float64) string {
	endpoints := make([]int, 0, len(pools))
	for endpoint := range pools {
		endpoints = append(endpoints, endpoint)
	}
	sort.Ints(endpoints)
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		row := pools[endpoint]
		parts = append(parts, fmt.Sprintf("endpoint%d 总量 %.2f 已用 %.2f 剩余 %.2f", endpoint, row[0], row[1], row[2]))
	}
	return strings.Join(parts, " | ")
}

func traeCNProbePoolDiff(before, after map[int][3]float64) string {
	endpoints := make([]int, 0, len(after))
	for endpoint := range after {
		endpoints = append(endpoints, endpoint)
	}
	sort.Ints(endpoints)
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		previous, ok := before[endpoint]
		if !ok {
			continue
		}
		current := after[endpoint]
		parts = append(parts, fmt.Sprintf("endpoint%d %+.4f (已用 %.4f -> %.4f)", endpoint, current[1]-previous[1], previous[1], current[1]))
	}
	return strings.Join(parts, " | ")
}

// traeCNProbeCreditsRaw 原始调用 pay 接口，只用于探针观察。
func traeCNProbeCreditsRaw(ctx context.Context, account *auth.Account, reqSource int) ([]byte, error) {
	_, accessToken := account.TraeCNCredentials()
	endpoint := TraeCNCheckinHost + traeCNCreditsUsagePath
	payload := fmt.Sprintf(`{"require_usage":true,"req_source":%d}`, reqSource)
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header = auth.TraeCNCheckinHeaders(account, accessToken, uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// traeCNProbeChat 用给定身份跑完一次最小对话，返回 SSE 摘要。
func traeCNProbeChat(ctx context.Context, account *auth.Account, candidate traeCNProbeCandidate) (string, error) {
	host, accessToken := account.TraeCNCredentials()
	if host == "" {
		return "", fmt.Errorf("账号缺少 host")
	}
	body := map[string]any{
		"function": candidate.function,
		"stream":   true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "ping"}},
		}},
		"max_tokens": 24,
	}
	for key, value := range candidate.extra {
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	endpoint := host + auth.TraeCNChatPath
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	events := map[string]int{}
	text := strings.Builder{}
	raw := bytes.Buffer{}
	scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(resp.Body, 1<<18), &raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			events[strings.TrimSpace(strings.TrimPrefix(line, "event:"))]++
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		delta := gjson.Get(data, "choices.0.delta.content")
		if delta.Type == gjson.String {
			text.WriteString(delta.String())
		}
	}
	plain := strings.Join(strings.Fields(raw.String()), " ")
	summary := fmt.Sprintf("HTTP %d events=%v remain=%s text=%q",
		resp.StatusCode, events, clipTraeCNProbe(gjson.Get(plain, "cn_credits_remain_info").Raw, 80), clipTraeCNProbe(text.String(), 80))
	if code := gjson.Get(plain, "@this").String(); code != "" {
		_ = code
	}
	for _, chunk := range strings.Split(plain, "id:") {
		if strings.Contains(chunk, "event:error") {
			summary += " error=" + clipTraeCNProbe(strings.TrimSpace(chunk[strings.Index(chunk, "data:")+5:]), 160)
			break
		}
	}
	return summary, nil
}

// TestLiveTraeCNWorkStreamRaw 打印 access_type=1（Work 池）请求的原始 SSE，
// 用来比对它与 IDE 侧事件的 schema 差别。
func TestLiveTraeCNWorkStreamRaw(t *testing.T) {
	exportPath := strings.TrimSpace(os.Getenv("TRAECN_LIVE_CREDITS_FILE"))
	if exportPath == "" {
		t.Skip("set TRAECN_LIVE_CREDITS_FILE=<exported traecn-accounts JSON>")
	}
	account := loadTraeCNProbeAccount(t, exportPath)
	for _, candidate := range []traeCNProbeCandidate{
		{name: "work_access_type_1", function: "chat_v3", extra: map[string]any{"access_type": 1}},
		{name: "work_access_type_2", function: "chat_v3", extra: map[string]any{"access_type": 2}},
	} {
		raw, status, err := traeCNProbeRawStream(t.Context(), account, candidate)
		if err != nil {
			t.Errorf("[%s] %v", candidate.name, err)
			continue
		}
		t.Logf("[%s] HTTP %d raw=%s", candidate.name, status, clipTraeCNProbe(strings.Join(strings.Fields(raw), " "), 2600))
	}
}

// traeCNProbeRawStream 原样返回一次最小请求的 SSE 文本。
func traeCNProbeRawStream(ctx context.Context, account *auth.Account, candidate traeCNProbeCandidate) (string, int, error) {
	host, accessToken := account.TraeCNCredentials()
	body := map[string]any{
		"function": candidate.function,
		"stream":   true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "say hi"}},
		}},
		"max_tokens": 24,
	}
	for key, value := range candidate.extra {
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, host+auth.TraeCNChatPath, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(raw), resp.StatusCode, nil
}

func clipTraeCNProbe(value string, limit int) string {
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}
