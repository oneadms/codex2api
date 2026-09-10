package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestTraeCNToolParametersUseJSONString(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","properties":{"path":{"type":"string","description":"路径包含引号 \" 和反斜杠 \\"}},"required":["path"],"additionalProperties":false}`
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tool string
	}{
		{"object", `{"type":"function","name":"read_file","parameters":` + schema + `}`},
		{"already_encoded", `{"type":"function","name":"read_file","parameters":` + string(encoded) + `}`},
		{"nested_function", `{"type":"function","function":{"name":"read_file","parameters":` + schema + `}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := buildTraeCNRequestBody([]byte(`{"model":"doubao-seed-code","input":"read file","tools":[` + tc.tool + `]}`))
			if err != nil {
				t.Fatal(err)
			}
			parameters := gjson.GetBytes(body, "tools.0.function.parameters")
			if parameters.Type != gjson.String {
				t.Fatalf("TRAE tool parameters must be a JSON string, got %s: %s", parameters.Type, body)
			}
			var got, want any
			if err := json.Unmarshal([]byte(parameters.String()), &got); err != nil {
				t.Fatalf("parameters is not a JSON schema: %v", err)
			}
			if err := json.Unmarshal([]byte(schema), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("schema changed or was encoded twice: got %s, want %s", parameters.String(), schema)
			}
		})
	}
}

// 用上游同样的 string 字段解码请求，覆盖三个入口实际发出的工具定义。
func TestTraeCNHandlersSendStringToolParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			path   string
			body   string
			invoke func(*Handler, *gin.Context)
		}{
			{"/v1/responses", `{"model":"doubao-seed-code","input":"read file","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`, (*Handler).Responses},
			{"/v1/chat/completions", `{"model":"doubao-seed-code","messages":[{"role":"user","content":"read file"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}`, (*Handler).ChatCompletions},
			{"/v1/messages", `{"model":"doubao-seed-code","max_tokens":64,"messages":[{"role":"user","content":"read file"}],"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`, (*Handler).Messages},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.path, stream), func(t *testing.T) {
				resetResponseCacheStateForTest(testResponseCacheConfig())
				t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
				handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Tools []struct {
							Function struct {
								Name       string `json:"name"`
								Parameters string `json:"parameters"`
							} `json:"function"`
						} `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						w.WriteHeader(http.StatusBadRequest)
						json.NewEncoder(w).Encode(map[string]any{"code": 4001, "message": "bad request: " + err.Error()})
						return
					}
					if len(request.Tools) != 1 || request.Tools[0].Function.Name != "read_file" ||
						gjson.Get(request.Tools[0].Function.Parameters, "properties.path.type").String() != "string" {
						http.Error(w, "missing tool schema", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
				})
				body := strings.TrimSuffix(tc.body, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				ctx.Request.Header.Set("Content-Type", "application/json")
				ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91088, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
				tc.invoke(handler, ctx)
				if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ok") {
					t.Fatalf("status=%d, want successful TRAE request: %s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
}
