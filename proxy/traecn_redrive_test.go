package proxy

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

// traeCNProviderStream 拼一段 Trae 上游 SSE（event: output / event: done）。
func traeCNProviderStream(text string) string {
	return "event: output\ndata: {\"response\":" + jsonString(text) + "}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
}

// traeCNProviderToolStream 拼一段"直接交付工具调用"的上游 SSE。
func traeCNProviderToolStream(name, arguments string) string {
	call := `{"id":"call_1","type":"function","function":{"name":` + jsonString(name) + `,"arguments":` + jsonString(arguments) + `}}`
	return "event: output\ndata: {\"tool_calls\":[" + call + "]}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
}

func jsonString(value string) string {
	encoded, err := jsonMarshalString(value)
	if err != nil {
		return `""`
	}
	return encoded
}

func jsonMarshalString(value string) (string, error) {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String(), nil
}

func traeCNContractFor(name string) traeCNContracts {
	return traeCNContracts{name: traeCNToolContract{Name: name}}
}

// TestTraeCNRedrivesWhenModelStopsWithAPlan 是本次修复的核心回归用例：
// 客户端声明了工具、模型却用"接下来我会…"收尾时，网关不能直接放终态（客户端会安静停止），
// 而要用同一账号把上游再驱动一次，把这一轮真的接下去。
func TestTraeCNRedrivesWhenModelStopsWithAPlan(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	segments := []string{
		traeCNProviderStream("出现 8 个冲突文件。让我先了解冲突的范围和当前状态："),
		traeCNProviderToolStream("exec_command", `{"command":"git status"}`),
	}
	var calls int32
	var seenPrior []string
	redriver := func(priorText string) (io.ReadCloser, error) {
		seenPrior = append(seenPrior, priorText)
		index := int(atomic.AddInt32(&calls, 1)) - 1
		if index >= len(segments)-1 {
			return nil, errors.New("no more segments")
		}
		return io.NopCloser(strings.NewReader(segments[index+1])), nil
	}
	source := io.NopCloser(strings.NewReader(segments[0]))
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(source, "DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("redriver 调用次数 = %d, want 1", calls)
	}
	if len(seenPrior) != 1 || !strings.Contains(seenPrior[0], "让我先了解冲突的范围") {
		t.Fatalf("续跑回灌的上一段正文不对: %#v", seenPrior)
	}
	events := canonicalSSEEvents(t, raw)
	if _, ok := findCanonicalEvent(events, "response.completed"); !ok {
		t.Fatalf("缺少 response.completed；事件=%s", string(raw))
	}
	if _, ok := findCanonicalEvent(events, "response.failed"); ok {
		t.Fatalf("续跑后不应出现 response.failed：%s", string(raw))
	}
	completed, _ := findCanonicalEvent(events, "response.completed")
	outputs := completed.Get("response.output").Array()
	var toolCalls int
	for _, item := range outputs {
		if item.Get("type").String() == "function_call" {
			toolCalls++
		}
	}
	if toolCalls != 1 {
		t.Fatalf("续跑后的工具调用数 = %d, want 1；output=%s", toolCalls, completed.Get("response.output").Raw)
	}
	if !strings.Contains(string(raw), "让我先了解冲突的范围") {
		t.Fatalf("第一段正文应当已经下发给客户端: %s", string(raw))
	}
}

// TestTraeCNRedriveSkippedForRealDeliverables 防止把"真的做完了"的最终答复也判成提前收尾。
func TestTraeCNRedriveSkippedForRealDeliverables(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	cases := map[string]string{
		"长报告":   strings.Repeat("已完成全部阶段。", 60),
		"短确认":   "是的，我已经完成了所有工作。",
		"礼貌收尾":  "All checks pass. Let me know if you want me to continue.",
		"英文长交付": "Implementation complete: 21 tests passed, benchmark 23k ops/s.",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int32
			redriver := func(string) (io.ReadCloser, error) {
				atomic.AddInt32(&calls, 1)
				return nil, errors.New("should not be called")
			}
			raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
				io.NopCloser(strings.NewReader(traeCNProviderStream(text))), "DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
			if err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Fatalf("正常交付被误判为提前收尾，续跑 %d 次: %q", got, text)
			}
			events := canonicalSSEEvents(t, raw)
			if _, ok := findCanonicalEvent(events, "response.completed"); !ok {
				t.Fatalf("应正常完成：%s", string(raw))
			}
		})
	}
}

// TestTraeCNRedriveSkippedWithoutDeclaredTools 纯问答（客户端没给工具）不干预。
func TestTraeCNRedriveSkippedWithoutDeclaredTools(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	var calls int32
	redriver := func(string) (io.ReadCloser, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("should not be called")
	}
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
		io.NopCloser(strings.NewReader(traeCNProviderStream("让我先看看这段代码。"))), "DeepSeek-V4-Pro", nil, nil, false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("无工具请求不应续跑，实际 %d 次", calls)
	}
	if !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("应正常完成：%s", string(raw))
	}
}

// TestTraeCNRedriveSkippedAfterToolCallDelivered 已经交付工具调用的一轮本来就该结束。
func TestTraeCNRedriveSkippedAfterToolCallDelivered(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	var calls int32
	redriver := func(string) (io.ReadCloser, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("should not be called")
	}
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
		io.NopCloser(strings.NewReader(traeCNProviderToolStream("exec_command", `{"command":"ls"}`))),
		"DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("已有工具调用不应续跑，实际 %d 次", calls)
	}
	if !strings.Contains(string(raw), "function_call") {
		t.Fatalf("工具调用应照常交付：%s", string(raw))
	}
}

// TestTraeCNRedriveIsBounded 连续用计划句收尾时不能无限续跑。
func TestTraeCNRedriveIsBounded(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	var calls int32
	redriver := func(string) (io.ReadCloser, error) {
		atomic.AddInt32(&calls, 1)
		return io.NopCloser(strings.NewReader(traeCNProviderStream("让我先确认一下目录结构。"))), nil
	}
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
		io.NopCloser(strings.NewReader(traeCNProviderStream("让我先确认一下目录结构。"))),
		"DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("续跑次数 = %d, want 2（上限）", got)
	}
	if !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("用尽预算后必须给出终态，不能挂住：%s", string(raw))
	}
}

// TestTraeCNRedriveCanBeDisabled 运维开关必须能一键关掉。
func TestTraeCNRedriveCanBeDisabled(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "0")
	var calls int32
	redriver := func(string) (io.ReadCloser, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("should not be called")
	}
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
		io.NopCloser(strings.NewReader(traeCNProviderStream("让我先确认一下目录结构。"))),
		"DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("开关关闭时仍续跑 %d 次", calls)
	}
	if !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("应正常完成：%s", string(raw))
	}
}

// TestTraeCNRedriveFallsBackWhenUpstreamRefuses 续跑失败不能变成一次新的失败：
// 已经拿到的正文照常交付，客户端拿到 response.completed。
func TestTraeCNRedriveFallsBackWhenUpstreamRefuses(t *testing.T) {
	t.Setenv("TRAECN_AUTO_CONTINUE_MAX", "2")
	redriver := func(string) (io.ReadCloser, error) {
		return nil, errors.New("upstream status 502")
	}
	raw, err := io.ReadAll(traeCNCanonicalStreamWithRedrive(
		io.NopCloser(strings.NewReader(traeCNProviderStream("让我先确认一下目录结构。"))),
		"DeepSeek-V4-Pro", nil, traeCNContractFor("exec_command"), false, redriver))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("续跑失败应退回普通收尾：%s", string(raw))
	}
	if strings.Contains(string(raw), "response.failed") {
		t.Fatalf("续跑失败不应额外制造失败：%s", string(raw))
	}
}

// TestTraeCNContinuationBodyAppendsAssistantAndNudge 续跑请求体必须保留原工具声明，
// 并把模型刚说的话作为 assistant 历史回灌。
func TestTraeCNContinuationBodyAppendsAssistantAndNudge(t *testing.T) {
	original := []byte(`{"function":"chat_v3","model":"DeepSeek-V4-Pro","messages":[{"role":"system","content":[{"type":"text","text":"sys"}]},{"role":"user","content":[{"type":"text","text":"do it"}]}],"tools":[{"type":"function","name":"exec_command"}]}`)
	updated := traeCNContinuationBody(original, "出现 8 个冲突文件。让我先了解冲突的范围和当前状态：")
	messages := gjson.GetBytes(updated, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("messages 条数 = %d, want 4；body=%s", len(messages), updated)
	}
	if messages[2].Get("role").String() != "assistant" || !strings.Contains(messages[2].Get("content.0.text").String(), "让我先了解冲突") {
		t.Fatalf("倒数第二条应当是模型刚才的正文：%s", messages[2].Raw)
	}
	if messages[3].Get("role").String() != "user" || !strings.Contains(messages[3].Get("content.0.text").String(), "继续") {
		t.Fatalf("最后一条应当是继续提醒：%s", messages[3].Raw)
	}
	if got := gjson.GetBytes(updated, "tools.0.name").String(); got != "exec_command" {
		t.Fatalf("工具声明被破坏：%s", updated)
	}
	if got := gjson.GetBytes(updated, "model").String(); got != "DeepSeek-V4-Pro" {
		t.Fatalf("模型名被破坏：%s", updated)
	}
}

// TestTraeCNNextActionAnnouncementDetector 钉住窄判定的边界。
func TestTraeCNNextActionAnnouncementDetector(t *testing.T) {
	positive := []string{
		"出现 8 个冲突文件。让我先了解冲突的范围和当前状态：",
		"完整 `proxy` 包测试还在跑（CI 里要 70 秒左右），我等它跑完。",
		"All 44 samples across 12 cases pass. Now the CLI smoke test, then pytest suite.",
		"现在写像素编辑器（核心页面）、颜色统计、作品管理。",
		"ate the CHANGELOG to reflect the corrected behavior, then run the final full verification.",
		// 长正文里的"等 worker 产出后再决定下一步"同样是没做完就停，不能因为正文长就放过。
		strings.Repeat("三条路线并行推进，目标未达成前保持此布局。", 12) + "等 worker 产出后再做下一步判断。",
	}
	for _, text := range positive {
		if !traeCNNextActionAnnouncement(text) {
			t.Errorf("应当判为提前收尾: %q", text)
		}
	}
	negative := []string{
		"是的，我已经完成了所有工作。",
		"",
		strings.Repeat("已完成全部阶段，21 个测试通过。", 40),
		"All checks pass. Let me know if you want me to continue.",
		"3 个模块已交付，README 已补齐。",
		// 以问号结尾 = 模型在等用户回话，属于合法回合结束。
		"想好新项目做什么了吗？我可以直接在这里初始化 Godot 项目结构。",
		"接下来你想让我先做哪一块？",
	}
	for _, text := range negative {
		if traeCNNextActionAnnouncement(text) {
			t.Errorf("不应判为提前收尾: %q", text)
		}
	}
}
