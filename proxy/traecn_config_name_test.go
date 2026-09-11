package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// Trae 服务端按 `config_name` 选后端（区分大小写），只发 `model` 时上游会回落默认
// 后端——实测同一个账号只发 model=glm-5.3-flash 会答"我是豆包大语言模型"。
// 回归点：请求必须带上 config_name。
func TestTraeCNRequestBodySendsUpstreamConfigNameForKnownCatalog(t *testing.T) {
	t.Parallel()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	catalog := []string{"auto", "glm-5.3-flash", "Doubao-Seed-Code", "DeepSeek-V4-Pro"}

	body, _, _, _, err := traeCNRequestBodyPlan([]byte(`{"model":"glm-5.3-flash","input":"hi"}`), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "config_name").String(); got != "glm-5.3-flash" {
		t.Fatalf("config_name = %q, want glm-5.3-flash; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "glm-5.3-flash" {
		t.Fatalf("model = %q; body=%s", got, body)
	}
}

// 大小写/分隔符写错的名字先按账号目录校正成 provider 的逐字写法再发，否则上游
// 要么拿到 4001（deepseek-v4-pro），要么认不出 config_name 而回落豆包。
func TestTraeCNRequestBodyCanonicalizesConfigNameFromCatalog(t *testing.T) {
	t.Parallel()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	catalog := []string{"Doubao_1_6", "Doubao-Seed-Code", "DeepSeek-V4-Pro", "kimi-k2.7-code"}

	for requested, want := range map[string]string{
		"doubao-seed-code": "Doubao-Seed-Code",
		"Deepseek-V4-Pro":  "DeepSeek-V4-Pro",
		"deepseek-v4-pro":  "DeepSeek-V4-Pro",
		"doubao-1-6":       "Doubao_1_6",
		"kimi-k2-7-code":   "kimi-k2.7-code",
	} {
		body, _, _, _, err := traeCNRequestBodyPlan([]byte(`{"model":"`+requested+`","input":"hi"}`), catalog)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(body, "config_name").String(); got != want {
			t.Errorf("model %q: config_name = %q, want %q", requested, got, want)
		}
		if got := gjson.GetBytes(body, "model").String(); got != want {
			t.Errorf("model %q: wire model = %q, want %q", requested, got, want)
		}
	}
}

// 目录里没有的名字不写 config_name：宁可让上游回落默认后端，也不要发出去拿 4001。
func TestTraeCNRequestBodyOmitsUnknownConfigName(t *testing.T) {
	t.Parallel()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	body, _, _, _, err := traeCNRequestBodyPlan(
		[]byte(`{"model":"gpt-5.6-sol","input":"hi"}`),
		[]string{"glm-5.3-flash", "Doubao-Seed-Code"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "config_name").Exists() {
		t.Fatalf("unknown model must not be sent as config_name: %s", body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.6-sol" {
		t.Fatalf("model = %q; body=%s", got, body)
	}
}

// auto 走 inline_chat，上游自己挑后端，不指定 config_name。
func TestTraeCNRequestBodyKeepsAutoUnspecified(t *testing.T) {
	t.Parallel()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	body, _, _, _, err := traeCNRequestBodyPlan([]byte(`{"model":"auto","input":"hi"}`), []string{"auto", "glm-5.3-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "config_name").Exists() || gjson.GetBytes(body, "model").Exists() {
		t.Fatalf("auto must not pin a model: %s", body)
	}
	if got := gjson.GetBytes(body, "function").String(); got != "inline_chat" {
		t.Fatalf("function = %q, want inline_chat", got)
	}
}

// 上游目录逐字保留 provider 的 config_name（大小写敏感），占位符仍然隐藏。
func TestTraeCNPublicIDsForConfigKeepsProviderSpelling(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"Doubao-Seed-Code":      "Doubao-Seed-Code",
		"Doubao_1_6":            "Doubao_1_6",
		"DeepSeek-V4-Pro":       "DeepSeek-V4-Pro",
		"Doubao-Seed-2.1-Turbo": "Doubao-Seed-2.1-Turbo",
		"new-provider-model":    "new-provider-model",
	} {
		got := traeCNPublicIDsForConfig(input)
		if len(got) != 1 || got[0] != want {
			t.Errorf("traeCNPublicIDsForConfig(%q) = %v, want [%s]", input, got, want)
		}
	}
	if got := traeCNPublicIDsForConfig("custom_model_gpt-5"); got != nil {
		t.Fatalf("custom model placeholder leaked: %v", got)
	}
}

// 客户端/旧配置里的归一化写法要能通过校验并路由到同一个账号，否则升级目录后
// 所有 lowercase 模型名都会直接 400。
func TestTraeCNChannelAcceptsNormalizedCatalogNames(t *testing.T) {
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, model := range []string{"Doubao-Seed-Code", "doubao-seed-code", "deepseek-v4-pro", "DeepSeek-V4-Pro", "doubao-1-6"} {
		if !handler.traeCNModelNameAccepted(model) {
			t.Errorf("model %q should be accepted on the Trae channel", model)
		}
	}
	for _, model := range []string{"gpt-5.6-sol", "claude-opus-4-7", ""} {
		if handler.traeCNModelNameAccepted(model) {
			t.Errorf("model %q must not be accepted on the Trae channel", model)
		}
	}
}
