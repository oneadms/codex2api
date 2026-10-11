package proxy

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func traeCNLoopTestSSE(event string, value any) string {
	raw, _ := json.Marshal(value)
	return "event: " + event + "\ndata: " + string(raw) + "\n\n"
}

// 同名工具可以来自不同命名空间，声明、历史和回程必须使用同一套身份映射。
func TestTraeCNNamespaceToolRoundTrip(t *testing.T) {
	t.Parallel()
	request := []byte(`{"model":"doubao-seed-code","tools":[
  {"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},
  {"type":"namespace","name":"device","children":[{"type":"function","name":"read","parameters":{"type":"object"}}]},
  {"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}
],"input":[
  {"type":"function_call","namespace":"files","name":"read","call_id":"old_read","arguments":"{}"},
  {"type":"function_call_output","call_id":"old_read","output":"file contents"},
  {"type":"custom_tool_call","namespace":"functions","name":"exec","call_id":"old_exec","input":"await tools.read()"},
  {"type":"custom_tool_call_output","call_id":"old_exec","output":"done"}
],"tool_choice":{"type":"function","namespace":"device","name":"read"}}`)
	body, model, bridges, contracts, err := traeCNRequestBodyPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"files__read", "device__read", "functions__exec"} {
		found := false
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			found = found || tool.Get("function.name").String() == name
		}
		if !found {
			t.Fatalf("missing callable tool %q: %s", name, body)
		}
	}
	// Trae 的 tool_choice 是字符串字段：带 namespace 的强制选择同样不能发对象上去，
	// 只能降级成 required（详见 traeCNToolChoiceToChat 与实测的 4001）。
	if got := gjson.GetBytes(body, "tool_choice"); got.Type != gjson.String || got.String() != "required" {
		t.Fatalf("tool choice must be downgraded to the string required, got %s", got.Raw)
	}
	var history []string
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		for _, call := range message.Get("tool_calls").Array() {
			history = append(history, call.Get("function_call.name").String())
		}
	}
	if strings.Join(history, ",") != "files__read,functions__exec" {
		t.Fatalf("history identity mismatch: %v", history)
	}
	provider := traeCNLoopTestSSE("output", map[string]any{"response": "我还会继续检查设备。", "tool_calls": []any{
		map[string]any{"id": "call_read", "function_call": map[string]any{"name": "device__read", "arguments": "{}"}},
		map[string]any{"id": "call_exec", "function_call": map[string]any{"name": "functions__exec", "arguments": `{"input":"await tools.read()"}`}},
	}}) + traeCNLoopTestSSE("done", map[string]any{"finish_reason": "stop"})
	raw, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(provider)), model, bridges, contracts))
	if err != nil {
		t.Fatal(err)
	}
	events := canonicalSSEEvents(t, raw)
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok {
		t.Fatalf("missing completed event: %s", raw)
	}
	for _, tc := range []struct{ typ, namespace, name string }{{"function_call", "device", "read"}, {"custom_tool_call", "functions", "exec"}} {
		item := completed.Get(`response.output.#(type=="` + tc.typ + `")`)
		if item.Get("name").String() != tc.name || item.Get("namespace").String() != tc.namespace {
			t.Fatalf("Codex cannot resolve the returned tool: %s", item.Raw)
		}
		for _, event := range events {
			if event.Get("item.type").String() == tc.typ && (event.Get("item.name").String() != tc.name || event.Get("item.namespace").String() != tc.namespace) {
				t.Fatalf("stream identity differs from terminal output: %s", event.Raw)
			}
		}
	}
}

func TestTraeCNNamespaceNameCollisionIsRejected(t *testing.T) {
	t.Parallel()
	_, _, err := buildTraeCNRequestBody([]byte(`{"model":"doubao-seed-code","input":"read","tools":[
  {"type":"function","name":"files__read","parameters":{"type":"object"}},
  {"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}
]}`))
	if err == nil {
		t.Fatal("colliding tool identities must not be silently merged")
	}
}

func TestTraeCNParallelToolHistoryStaysInOneTurn(t *testing.T) {
	t.Parallel()
	body, _, err := buildTraeCNRequestBody([]byte(`{"model":"doubao-seed-code","input":[
  {"type":"function_call","call_id":"read_a","name":"read","arguments":"{}"},
  {"type":"function_call","call_id":"read_b","name":"read","arguments":"{}"},
  {"type":"function_call_output","call_id":"read_a","output":"A"},
  {"type":"function_call_output","call_id":"read_b","output":"B"}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "messages.#").Int() != 3 || gjson.GetBytes(body, "messages.0.tool_calls.#").Int() != 2 ||
		gjson.GetBytes(body, "messages.1.tool_call_id").String() != "read_a" || gjson.GetBytes(body, "messages.2.tool_call_id").String() != "read_b" {
		t.Fatalf("parallel calls were split into invalid assistant turns: %s", body)
	}
}

// TestTraeCNToolChoiceObjectIsDowngradedToString 覆盖线上实测到的上游 400：
// Trae 的 LLMUtilsChatRequest.tool_choice 是字符串字段，发对象会被上游拒绝
// （code 4001 "cannot unmarshal object into Go struct field ... tool_choice"），
// 整轮请求当场失败。对象形态必须降级成字符串，而不是原样发出去。
func TestTraeCNToolChoiceObjectIsDowngradedToString(t *testing.T) {
	cases := []struct {
		choice string
		want   string
	}{
		{`"auto"`, "auto"},
		{`"none"`, "none"},
		{`"required"`, "required"},
		{`{"type":"function","name":"exec_command","namespace":""}`, "required"},
		{`{"type":"function","function":{"name":"read_file"}}`, "required"},
		{`{"type":"custom","name":"apply_patch"}`, "required"},
		{`{"type":"tool_search"}`, "required"},
		{`{"type":"allowed_tools","tools":[{"type":"function","name":"x"}]}`, "required"},
		{`{"type":"auto"}`, "auto"},
		{`{"type":"none"}`, "none"},
		{`{"type":"something_new"}`, "required"},
	}
	for _, tc := range cases {
		converted, err := traeCNToolChoiceToChat(gjson.Parse(tc.choice))
		if err != nil {
			t.Fatalf("tool_choice %s 转换失败: %v", tc.choice, err)
		}
		got, ok := converted.(string)
		if !ok {
			t.Fatalf("tool_choice %s 转换结果必须是字符串（Trae 字段是 string），实际 %#v", tc.choice, converted)
		}
		if got != tc.want {
			t.Fatalf("tool_choice %s → %q, want %q", tc.choice, got, tc.want)
		}
	}

	// 出站请求体里也必须真的是字符串，否则上游仍然会 400。
	body, _, _, _, err := traeCNRequestBodyPlan([]byte(`{"model":"deepseek-v3","input":"x","tool_choice":{"type":"function","name":"exec_command"},"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "tool_choice"); got.Type != gjson.String || got.String() != "required" {
		t.Fatalf("出站 tool_choice = %s (type %v), want 字符串 required", got.Raw, got.Type)
	}
}
