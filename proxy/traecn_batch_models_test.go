package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// 桌面客户端用批量接口取模型目录，它的目录比单数接口新（多出 glm-5.3-flash /
// kimi-k3 / qwen3.8-* 等）。同步必须优先调它，否则新模型永远同步不到。
func TestExtractTraeCNModelIDsFromBatchFunctionConfigs(t *testing.T) {
	t.Parallel()
	body := []byte(`{
  "allow_tenant_user_add_model": true,
  "function_configs": [
    {"function":"chat_v3","config_info_list":[
      {"config_name":"glm-5.3-flash","config_switch":true},
      {"config_name":"glm-5.3","config_switch":true},
      {"config_name":"kimi-k3","config_switch":true},
      {"config_name":"qwen3.8-max","config_switch":true},
      {"config_name":"Doubao-Seed-2.1-Turbo","config_switch":true},
      {"config_name":"summary","config_switch":true},
      {"config_name":"custom_model_gpt-5","config_switch":true},
      {"config_name":"disabled-model","config_switch":false}
    ]},
    {"function":"solo_agent","config_info_list":[
      {"config_name":"glm-5.3-flash","config_switch":true},
      {"config_name":"Doubao-Seed-2.1-Pro","config_switch":true}
    ]},
    {"function":"inline_chat","config_info_list":[{"config_name":"inline_chat","config_switch":true}]}
  ]
}`)
	got := extractTraeCNModelIDs(body)
	joined := strings.Join(got, ", ")
	// Doubao 系 config 会按网关目录的拼写归一（Doubao-Seed-2.1-Pro -> doubao-seed-2-1-pro）。
	for _, want := range []string{"glm-5.3-flash", "glm-5.3", "kimi-k3", "qwen3.8-max", "doubao-seed-2-1-turbo", "doubao-seed-2-1-pro", "auto"} {
		if !containsFold(got, want) {
			t.Errorf("批量目录缺少 %q: %s", want, joined)
		}
	}
	for _, unwanted := range []string{"summary", "custom_model_gpt-5", "disabled-model", "inline_chat"} {
		if containsFold(got, unwanted) {
			t.Errorf("批量目录混入了 %q: %s", unwanted, joined)
		}
	}
	if count := len(got) - 1; count != 6 { // 6 个去重模型 + auto
		t.Errorf("去重后模型数 = %d, 期望 6（+auto）: %s", count, joined)
	}
}

// 同步请求顺序：批量接口优先，成功即返回，不再回落到落后的兼容面。
func TestFetchTraeCNModelsPrefersBatchDetailEndpoint(t *testing.T) {
	var batchCalls, legacyCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case traeCNModelsBatchDetailPath:
			batchCalls++
			raw, _ := io.ReadAll(r.Body)
			request := gjson.ParseBytes(raw)
			if request.Get("agent_type").String() != "solo_agent" || !request.Get("functions").IsArray() {
				t.Errorf("unexpected batch body: %s", raw)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"glm-5.3-flash","config_switch":true},{"config_name":"glm-5.2","config_switch":true}]}]}`)
		case traeCNModelsPath, traeCNModelsDetailCompatPath, traeCNModelsDetailPath:
			legacyCalls++
			io.WriteString(w, `{"config_info_list":[{"config_name":"glm-5.2","config_switch":true}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNHost: server.URL}
	models, err := FetchTraeCNModels(t.Context(), account, "")
	if err != nil {
		t.Fatalf("FetchTraeCNModels() error = %v", err)
	}
	if batchCalls != 1 || legacyCalls != 0 {
		t.Fatalf("batch=%d legacy=%d, want batch first and no fallback", batchCalls, legacyCalls)
	}
	if !containsFold(models, "glm-5.3-flash") || !containsFold(models, "glm-5.2") {
		t.Fatalf("models = %#v", models)
	}
}
