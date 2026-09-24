package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Synthetic controlled requests; never claim these are captured Codex App turns.
// Exactly one dispatch per case, no retries or generated continuation messages.
func TestLiveTraeCNToolMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := os.Getenv("TRAECN_TOOL_PROBE_FILE")
	if path == "" {
		t.Skip("TRAECN_TOOL_PROBE_FILE is not configured")
	}
	account := loadTraeCNProbeAccount(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	models, err := FetchTraeCNModels(ctx, account, "")
	cancel()
	if err != nil {
		t.Fatal("could not fetch upstream model catalog")
	}
	target := traeCNResolveConfigName("kimi-k3", models)
	if target == "" {
		t.Fatalf("kimi-k3 not in upstream catalog: %v", models)
	}
	t.Logf("verified config_name=%q catalog_count=%d", target, len(models))
	account.TraeCNUpstreamModelCatalog = models
	// Freeze the pool across the matrix. Default is the IDE/Code pool.
	account.TraeCNCreditsPool = auth.TraeCNCreditsPoolCode
	if os.Getenv("TRAECN_TOOL_PROBE_POOL") == "work" {
		account.TraeCNCreditsPool = auth.TraeCNCreditsPoolWork
	}
	previous := auth.ConfiguredTraeCNSettings()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-6-astra": target}})
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })

	function := func(name string, fields ...string) map[string]any {
		props := map[string]any{}
		for _, field := range fields {
			props[field] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "function", "name": name, "description": "Test fixture tool: " + name, "parameters": map[string]any{"type": "object", "properties": props, "required": append([]string{}, fields...), "additionalProperties": false}}
	}
	read, write := function("read_file", "path"), function("write_file", "path", "content")
	namespace := map[string]any{"type": "namespace", "name": "files", "tools": []any{read, write}}
	codexPrompt := "You are a coding assistant working with the user in a local repository. Use the provided tools to inspect files before editing. Continue until the requested work is complete. Do not invent file contents. A normal final answer may use no tools."
	cases := []struct {
		name, prompt, instructions, effort, kind, namespace, tool, field, want string
		tools                                                                  []any
		input                                                                  []any
	}{
		{name: "minimal", prompt: "Call read_file with path README.md now. Do not answer before the tool result.", kind: "function_call", tool: "read_file", field: "path", want: "README.md", tools: []any{read}},
		{name: "max", prompt: "Call read_file with path README.md now. Do not answer before the tool result.", effort: "max", kind: "function_call", tool: "read_file", field: "path", want: "README.md", tools: []any{read}},
		{name: "codex_prompt", prompt: "Inspect README.md using read_file, then report its title after receiving the tool result.", instructions: codexPrompt, effort: "max", kind: "function_call", tool: "read_file", field: "path", want: "README.md", tools: []any{read}},
		{name: "tool_set", prompt: "Read README.md first before changing any files.", instructions: codexPrompt, effort: "max", kind: "function_call", tool: "read_file", field: "path", want: "README.md", tools: []any{read, write, function("list_files", "directory"), function("run_tests", "pattern"), function("search", "query")}},
		{name: "namespace", prompt: "Use files.read_file to inspect README.md now.", instructions: codexPrompt, effort: "max", kind: "function_call", namespace: "files", tool: "read_file", field: "path", want: "README.md", tools: []any{namespace}},
		{name: "additional_tools", prompt: "Use files.read_file to inspect README.md now.", instructions: codexPrompt, effort: "max", kind: "function_call", namespace: "files", tool: "read_file", field: "path", want: "README.md", input: []any{map[string]any{"type": "additional_tools", "tools": []any{namespace}}}},
		{name: "custom", prompt: "Call apply_patch with the exact raw text: *** Begin Patch\n*** Add File: probe.txt\n+ok\n*** End Patch", effort: "max", kind: "custom_tool_call", tool: "apply_patch", want: "*** Begin Patch", tools: []any{map[string]any{"type": "custom", "name": "apply_patch", "description": "Apply a patch to a fixture workspace. Accepts raw patch text."}}},
		{name: "tool_search", prompt: "Find tools for reading files using tool_search with query read files.", effort: "max", kind: "tool_search_call", field: "query", want: "read files", tools: []any{map[string]any{"type": "tool_search"}}},
		{name: "history", prompt: "Now write the updated contents to config.txt: version=2", instructions: codexPrompt, effort: "max", kind: "function_call", tool: "write_file", field: "path", want: "config.txt", tools: []any{read, write}, input: []any{map[string]any{"type": "function_call", "call_id": "old_read", "name": "read_file", "arguments": "{\"path\":\"config.txt\"}"}, map[string]any{"type": "function_call_output", "call_id": "old_read", "output": "version=1"}}},
		{name: "final_answer", prompt: "The file check is complete. Reply exactly OK. No further tools are needed.", instructions: codexPrompt, effort: "max", kind: "message", want: "OK", tools: []any{read, write}},
	}
	selected := "," + os.Getenv("TRAECN_TOOL_PROBE_CASES") + ","
	for _, tc := range cases {
		if selected != ",," && !strings.Contains(selected, ","+tc.name+",") {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			// The minimal case isolates the protocol from the existing gateway hint.
			if tc.name == "minimal" {
				t.Setenv("TRAECN_CONTINUE_GUARD_DISABLED", "1")
			}
			input := append([]any{}, tc.input...)
			input = append(input, map[string]any{"role": "user", "content": tc.prompt})
			body := map[string]any{"model": "gpt-6-astra", "stream": true, "input": input, "tools": tc.tools, "tool_choice": "auto", "max_output_tokens": 4096}
			if tc.effort != "" {
				body["reasoning"] = map[string]any{"effort": tc.effort}
			}
			if tc.instructions != "" {
				body["instructions"] = tc.instructions
			}
			encoded, _ := json.Marshal(body)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(encoded))).WithContext(ctx)
			captureTraeCNDiagnosticIngress(c, encoded)
			resp, err := ExecuteTraeCNRequest(c.Request.Context(), account, GrokProtocolResponses, encoded, encoded, "", nil)
			if err != nil {
				t.Fatal("upstream dispatch failed; inspect bounded diagnostic artifacts")
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d read_error=%t", resp.StatusCode, err != nil)
			}
			events := canonicalSSEEvents(t, raw)
			completed, ok := findCanonicalEvent(events, "response.completed")
			if !ok {
				failed, _ := findCanonicalEvent(events, "response.failed")
				t.Fatalf("no completed response; code=%s", failed.Get("response.error.code").String())
			}
			found := false
			for _, item := range completed.Get("response.output").Array() {
				if item.Get("type").String() != tc.kind {
					continue
				}
				if tc.kind == "message" {
					found = strings.TrimSpace(item.Get("content.0.text").String()) == tc.want
					continue
				}
				if item.Get("status").String() != "completed" || item.Get("call_id").String() == "" {
					continue
				}
				if tc.tool != "" && (item.Get("name").String() != tc.tool || item.Get("namespace").String() != tc.namespace) {
					continue
				}
				args := item.Get("arguments")
				if args.Type == gjson.String {
					args = gjson.Parse(args.String())
				}
				if tc.kind == "custom_tool_call" {
					found = strings.HasPrefix(item.Get("input").String(), tc.want)
				} else {
					found = args.Get(tc.field).String() == tc.want
				}
				if tc.name == "history" {
					found = found && strings.TrimSpace(args.Get("content").String()) == "version=2"
				}
			}
			if !found {
				t.Fatalf("expected executable %s/%s or final answer not received; inspect diagnostic artifacts", tc.kind, tc.tool)
			}
			t.Logf("verified kind=%s tool=%s namespace=%s input_tokens=%d output_tokens=%d", tc.kind, tc.tool, tc.namespace, completed.Get("response.usage.input_tokens").Int(), completed.Get("response.usage.output_tokens").Int())
		})
	}
}

// Execute only fixture operations. Subsequent requests contain actual tool
// results, never a synthetic "continue" message. An early prose-only answer
// fails immediately; eight turns is a test budget, not a retry policy.
func TestLiveTraeCNRepositoryTasks(t *testing.T) {
	path := os.Getenv("TRAECN_TOOL_PROBE_FILE")
	if path == "" {
		t.Skip("TRAECN_TOOL_PROBE_FILE is not configured")
	}
	gin.SetMode(gin.TestMode)
	account := loadTraeCNProbeAccount(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	catalog, err := FetchTraeCNModels(ctx, account, "")
	cancel()
	if err != nil {
		t.Fatal("model catalog fetch failed")
	}
	target := traeCNResolveConfigName("kimi-k3", catalog)
	if target == "" {
		t.Fatal("kimi-k3 is not in model catalog")
	}
	account.TraeCNUpstreamModelCatalog = catalog
	account.TraeCNCreditsPool = auth.TraeCNCreditsPoolCode
	if os.Getenv("TRAECN_TOOL_PROBE_POOL") == "work" {
		account.TraeCNCreditsPool = auth.TraeCNCreditsPoolWork
	}
	previous := auth.ConfiguredTraeCNSettings()
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-6-astra": target}})
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	t.Setenv("TRAECN_RESUME_ENABLED", "0")
	t.Setenv("TRAECN_RESUME_UPSTREAM_ENABLED", "0")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account.DBID = 91239
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	makeTool := func(name, description string, fields ...string) map[string]any {
		props := map[string]any{}
		for _, field := range fields {
			props[field] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "function", "name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": props, "required": append([]string{}, fields...), "additionalProperties": false}}
	}
	tools := []any{
		makeTool("read_file", "Read a file from the fixture repository.", "path"),
		makeTool("write_file", "Replace the entire file with content.", "path", "content"),
		makeTool("run_tests", "Check fixture expectations; call after writing files."),
	}
	cases := []struct{ name, prompt, path, before, want string }{
		{"repair_go", "修复 add.go 中 Add 的错误。先读文件，修改后运行测试，再汇报结果。", "add.go", "package calc\nfunc Add(a, b int) int { return a - b }\n", "return a + b"},
		{"update_config", "检查 config.json，把端口改成 9000，保留其他配置并运行测试。完成后汇报。", "config.json", "{\"port\":8080,\"host\":\"localhost\"}", "9000"},
		{"read_modify_verify", "README.md 中的安装命令已经过期。检查内容，把 npm install 替换成 npm ci，保留其他内容，运行测试验证后交付。", "README.md", "# Demo\n\nInstall: npm install\n\nRun: npm start\n", "npm ci"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.before
			reads, writes, tests := 0, 0, 0
			verified := false
			history := []any{map[string]any{"role": "user", "content": tc.prompt}}
			// Longer context stresses conversion without pretending to be an original
			// Codex App capture. The many tools are deliberately plausible distractors.
			declarations := append([]any{}, tools...)
			for i := 0; i < 32; i++ {
				declarations = append(declarations, makeTool(fmt.Sprintf("inspect_component_%d", i), "Optional repository component metadata lookup.", "component"))
			}
			instructions := "You are a coding assistant. Work autonomously until the task is complete. Inspect files before editing and verify changes. Only use the provided tools; the tool results are authoritative. Do not claim success until verification passes.\n"
			instructions += strings.Repeat("Repository note: generated files are excluded; preserve unrelated configuration and existing user changes.\n", 80)
			for turn := 0; turn < 8; turn++ {
				encoded, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": true, "input": history, "instructions": instructions, "tools": declarations, "tool_choice": "auto", "reasoning": map[string]any{"effort": "max"}, "max_output_tokens": 4096})
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				requestCtx, requestCancel := context.WithTimeout(t.Context(), 120*time.Second)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(encoded))).WithContext(requestCtx)
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91240, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
				handler.Responses(c)
				requestCancel()
				if recorder.Code != http.StatusOK {
					t.Fatalf("turn %d HTTP %d", turn, recorder.Code)
				}
				completed, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed")
				if !ok {
					t.Fatalf("turn %d has no completed response", turn)
				}
				calls := 0
				outputs := []any{}
				for _, item := range completed.Get("response.output").Array() {
					history = append(history, item.Value())
					if item.Get("type").String() != "function_call" {
						continue
					}
					calls++
					if item.Get("call_id").String() == "" || item.Get("status").String() != "completed" {
						t.Fatal("unexecutable call")
					}
					args := gjson.Parse(item.Get("arguments").String())
					result := ""
					switch item.Get("name").String() {
					case "read_file":
						if args.Get("path").String() != tc.path {
							t.Fatal("unexpected path")
						}
						reads++
						result = content
					case "write_file":
						if reads == 0 || args.Get("path").String() != tc.path || args.Get("content").String() == "" {
							t.Fatal("unsafe or empty fixture write")
						}
						writes++
						content = args.Get("content").String()
						result = "File updated. Run tests to verify."
						verified = false
					case "run_tests":
						tests++
						verified = strings.Contains(content, tc.want) && writes > 0
						if tc.name == "update_config" {
							verified = verified && gjson.Get(content, "port").Int() == 9000 && gjson.Get(content, "host").String() == "localhost"
						}
						if tc.name == "read_modify_verify" {
							verified = verified && strings.Contains(content, "npm start") && !strings.Contains(content, "npm install")
						}
						if verified {
							result = "PASS: all fixture checks passed"
						} else {
							result = "FAIL: expected change missing"
						}
					default:
						t.Fatalf("unexpected tool %s", item.Get("name").String())
					}
					outputs = append(outputs, map[string]any{"type": "function_call_output", "call_id": item.Get("call_id").String(), "output": result})
				}
				t.Logf("turn=%d executable_calls=%d reads=%d writes=%d checks=%d verified=%t", turn+1, calls, reads, writes, tests, verified)
				if calls == 0 {
					if !verified {
						t.Fatal("early text-only completion before verified task result")
					}
					return
				}
				history = append(history, outputs...)
			}
			t.Fatal("task did not finish within the eight-turn test budget")
		})
	}
}
