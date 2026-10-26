package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	traeCNPayloadRulePublicModel  = "payload-public-model"
	traeCNPayloadRuleConfigName   = "Doubao_1_6"
	traeCNPayloadRuleInstructions = "original system prompt\n\noriginal developer prompt"
	traeCNPayloadRuleReplacement  = "rewritten system prompt"
	traeCNPayloadRuleKeyID        = int64(92001)
	traeCNPayloadRuleKeyName      = "payload-team"
	traeCNPayloadRuleToolSchema   = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`
	traeCNPayloadRuleReply        = "event: output\ndata: {\"type\":\"text\",\"content\":\"payload rules integration ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
)

type traeCNPayloadRuleProtocol struct {
	name, path, body string
	invoke           func(*Handler, *gin.Context)
	originalPrompts  []string
	protectedPrompts []string
}

func traeCNPayloadRuleProtocols() []traeCNPayloadRuleProtocol {
	return []traeCNPayloadRuleProtocol{
		{
			name: "responses", path: "/v1/responses", invoke: (*Handler).Responses,
			body: `{
				"instructions":"original system prompt\n\noriginal developer prompt",
				"input":[
					{"role":"system","content":"protected system input"},
					{"role":"developer","content":[{"type":"input_text","text":"protected developer input"}]},
					{"role":"user","content":"first user input"},
					{"role":"assistant","content":"earlier assistant answer"},
					{"role":"user","content":[{"type":"input_text","text":"latest user input"}]}
				],
				"tools":[{"type":"function","name":"lookup","description":"look up a path","parameters":` + traeCNPayloadRuleToolSchema + `}],
				"service_tier":"priority","max_output_tokens":32
			}`,
			originalPrompts:  []string{traeCNPayloadRuleInstructions, "protected system input", "protected developer input"},
			protectedPrompts: []string{"protected system input", "protected developer input"},
		},
		{
			// Chat system messages become developer items in canonical input.
			// Keep two items so a nonmatching or no-op rule cannot merge them.
			name: "chat", path: "/v1/chat/completions", invoke: (*Handler).ChatCompletions,
			body: `{
				"messages":[
					{"role":"system","content":"original system prompt"},
					{"role":"system","content":[{"type":"text","text":"original developer prompt"}]},
					{"role":"user","content":"first user input"},
					{"role":"assistant","content":"earlier assistant answer"},
					{"role":"user","content":[{"type":"text","text":"latest user input"}]}
				],
				"tools":[{"type":"function","function":{"name":"lookup","description":"look up a path","parameters":` + traeCNPayloadRuleToolSchema + `}}],
				"service_tier":"priority","max_tokens":64,"max_completion_tokens":32
			}`,
			originalPrompts: []string{"original system prompt", "original developer prompt"},
		},
		{
			name: "messages", path: "/v1/messages", invoke: (*Handler).Messages,
			body: `{
				"system":[{"type":"text","text":"original system prompt"},{"type":"text","text":"original developer prompt"}],
				"messages":[
					{"role":"user","content":"first user input"},
					{"role":"assistant","content":"earlier assistant answer"},
					{"role":"user","content":[{"type":"text","text":"latest user input"}]}
				],
				"tools":[{"name":"lookup","description":"look up a path","input_schema":` + traeCNPayloadRuleToolSchema + `}],
				"service_tier":"priority","max_tokens":32
			}`,
			originalPrompts: []string{"original system prompt", "original developer prompt"},
		},
	}
}

func setupTraeCNPayloadRuleIntegration(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("TRAECN_RESUME_ENABLED", "0")
	t.Setenv("TRAECN_CONTINUE_GUARD_DISABLED", "0")
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	// The first mapping needs catalog normalization. Either spelling of its
	// target is also an alias, so a second mapping would select the wrong model.
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{
		traeCNPayloadRulePublicModel: "doubao-1-6",
		"doubao-1-6":                 "DeepSeek-V4-Pro",
		traeCNPayloadRuleConfigName:  "DeepSeek-V4-Pro",
	}})
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	runtime := previousRuntime
	runtime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(runtime)
}

func configureTraeCNPayloadRuleAccount(handler *Handler, groupID int64, groupName string) *auth.Account {
	account := handler.store.Accounts()[0]
	account.Mu().Lock()
	account.GroupIDs = []int64{groupID}
	account.TraeCNUpstreamModelCatalog = []string{traeCNPayloadRuleConfigName, "DeepSeek-V4-Pro"}
	account.Mu().Unlock()
	handler.store.SetGroupName(groupID, groupName)
	// Codex and Messages mappings must not replace the TRAE public alias.
	handler.store.SetCodexModelMapping(`{"payload-public-model":"gpt-5.5"}`)
	handler.store.SetModelMapping(`{"payload-public-model":"gpt-5.5"}`)
	return account
}

func invokeTraeCNPayloadRuleProtocol(t *testing.T, handler *Handler, protocol traeCNPayloadRuleProtocol, stream bool, channel string, allowedGroups []int64) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.TrimSuffix(protocol.body, "}") + fmt.Sprintf(`,"model":%q,"stream":%t}`, traeCNPayloadRulePublicModel, stream)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, protocol.path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Payload-Rule", "client-ide")
	ctx.Set(contextAPIKeyRow, &database.APIKeyRow{
		ID: traeCNPayloadRuleKeyID, Name: traeCNPayloadRuleKeyName,
		AllowedGroupIDs: allowedGroups,
		Limits:          database.APIKeyLimits{UpstreamChannel: channel},
	})
	protocol.invoke(handler, ctx)
	return recorder
}

func assertTraeCNPayloadRuleResponse(t *testing.T, recorder *httptest.ResponseRecorder, protocol traeCNPayloadRuleProtocol, stream bool) {
	t.Helper()
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "payload rules integration ok") {
		t.Fatalf("status=%d, body=%s", recorder.Code, recorder.Body.String())
	}
	var models []string
	if !stream {
		models = append(models, gjson.GetBytes(recorder.Body.Bytes(), "model").String())
	} else {
		completed := false
		for _, line := range strings.Split(recorder.Body.String(), "\n") {
			payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
			payload = strings.TrimSpace(payload)
			if !ok || payload == "[DONE]" {
				continue
			}
			if !gjson.Valid(payload) {
				t.Fatalf("invalid downstream SSE payload %q", payload)
			}
			event := gjson.Parse(payload)
			for _, path := range []string{"model", "response.model", "message.model"} {
				if model := event.Get(path); model.Exists() {
					models = append(models, model.String())
				}
			}
			switch protocol.name {
			case "responses":
				completed = completed || event.Get("type").String() == "response.completed"
			case "chat":
				completed = completed || event.Get("choices.0.finish_reason").String() == "stop"
			case "messages":
				completed = completed || event.Get("type").String() == "message_stop"
			}
		}
		if !completed {
			t.Fatalf("missing successful stream terminal event: %s", recorder.Body.String())
		}
	}
	if len(models) == 0 {
		t.Fatalf("response has no public model: %s", recorder.Body.String())
	}
	for _, model := range models {
		if model != traeCNPayloadRulePublicModel {
			t.Errorf("response model=%q, want public alias %q", model, traeCNPayloadRulePublicModel)
		}
	}
}

func assertTraeCNPayloadRuleNativeBody(t *testing.T, body []byte, prompts []string) {
	t.Helper()
	for _, field := range []string{"model", "config_name"} {
		if got := gjson.GetBytes(body, field).String(); got != traeCNPayloadRuleConfigName {
			t.Errorf("native %s=%q, want normalized config %q (mapping must run once)", field, got, traeCNPayloadRuleConfigName)
		}
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Errorf("TRAE native request must stream regardless of downstream mode: %s", body)
	}
	for _, field := range []string{"service_tier", "max_tokens", "max_output_tokens", "max_completion_tokens"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Errorf("unsupported field %s leaked to TRAE native request: %s", field, body)
		}
	}

	// Compare the actual system messages, not the unused top-level instructions
	// field. The adapter may append its tool helper to the first system message.
	var gotMessages []string
	helperCount := 0
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		role := message.Get("role").String()
		parts := message.Get("content").Array()
		if len(parts) != 1 || parts[0].Get("type").String() != "text" {
			t.Fatalf("message content changed: %s", message.Raw)
		}
		text := parts[0].Get("text").String()
		if role == "system" {
			helperCount += strings.Count(text, traeContinueWorkingMarker)
			if strings.HasPrefix(text, traeContinueWorkingMarker) {
				continue
			}
			text, _, _ = strings.Cut(text, "\n\n"+traeContinueWorkingMarker)
		}
		gotMessages = append(gotMessages, role+":"+text)
	}
	if helperCount != 1 {
		t.Errorf("native tool helper count=%d, want 1", helperCount)
	}
	var wantMessages []string
	for _, prompt := range prompts {
		wantMessages = append(wantMessages, "system:"+prompt)
	}
	wantMessages = append(wantMessages, "user:first user input", "assistant:earlier assistant answer", "user:latest user input")
	if !reflect.DeepEqual(gotMessages, wantMessages) {
		t.Errorf("native messages=%q, want %q", gotMessages, wantMessages)
	}

	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "function" ||
		tools[0].Get("function.name").String() != "lookup" || !strings.HasPrefix(tools[0].Get("function.description").String(), "look up a path\n\nArgument contract:") {
		t.Fatalf("original tools changed: %s", gjson.GetBytes(body, "tools").Raw)
	}
	parameters := tools[0].Get("function.parameters")
	if parameters.Type != gjson.String {
		t.Fatalf("native tool parameters must be a JSON string: %s", parameters.Raw)
	}
	var gotSchema, wantSchema any
	if err := json.Unmarshal([]byte(parameters.String()), &gotSchema); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(traeCNPayloadRuleToolSchema), &wantSchema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSchema, wantSchema) {
		t.Errorf("tool schema changed: got %s, want %s", parameters.String(), traeCNPayloadRuleToolSchema)
	}
}

func TestTraeCNPayloadRulesAllProtocols(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	// No parallel subtests: payload rules and TRAE model mappings are globals.
	for _, tc := range []struct {
		name, rules, instructions string
		changed                   bool
	}{
		{name: "no_rules", rules: `{}`},
		{name: "key_id_hit", rules: `{"override":[{"api_key_ids":["92001"],"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "key_id_miss", rules: `{"override":[{"api_key_ids":["92002"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "key_name_hit", rules: `{"override":[{"api_key_names":["payload-*"],"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "key_name_miss", rules: `{"override":[{"api_key_names":["other-*"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "key_allowed_group_hit", rules: `{"override":[{"group_ids":["73"],"group_names":["allowed-only-*"],"account_group_ids":["71"],"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "key_allowed_group_id_miss", rules: `{"override":[{"group_ids":["74"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "key_allowed_group_name_miss", rules: `{"override":[{"group_names":["other-*"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "normalized_model_hit", rules: `{"override":[{"models":["Doubao_1_6"],"match":{"model":"payload-public-model"},"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "public_alias_model_miss", rules: `{"override":[{"models":["payload-public-model"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "unnormalized_model_miss", rules: `{"override":[{"models":["doubao-1-6"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "header_hit", rules: `{"override":[{"headers":{"X-Payload-Rule":"client-*"},"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "header_miss", rules: `{"override":[{"headers":{"X-Payload-Rule":"other-*"},"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "selected_group_hit", rules: `{"override":[{"account_group_ids":["71"],"account_group_names":["selected-*"],"params":{"instructions":"rewritten system prompt"}}]}`, changed: true, instructions: traeCNPayloadRuleReplacement},
		{name: "selected_group_id_miss", rules: `{"override":[{"account_group_ids":["72"],"account_group_names":["selected-*"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{name: "selected_group_name_miss", rules: `{"override":[{"account_group_ids":["71"],"account_group_names":["other-*"],"params":{"instructions":"rewritten system prompt"}}]}`},
		{
			name: "all_gates_after_translation",
			rules: `{"override":[{
				"models":["Doubao_1_6"],"headers":{"X-Payload-Rule":"client-*"},
				"api_key_ids":["92001"],"api_key_names":["payload-*"],
				"account_group_ids":["71"],"account_group_names":["selected-*"],
				"match":{"model":"payload-public-model","instructions":"original system prompt\n\noriginal developer prompt","tools.0.name":"lookup","tools.0.type":"function"},
				"exist":["input.0"],"not_exist":["messages"],
				"params":{"instructions":"rewritten system prompt","service_tier":"priority","max_output_tokens":1}
			}]}`,
			changed: true, instructions: traeCNPayloadRuleReplacement,
		},
		{
			name:    "append_translated_instructions",
			rules:   `{"append":[{"match":{"instructions":"original system prompt\n\noriginal developer prompt"},"params":{"instructions":"extra guard text"}}]}`,
			changed: true, instructions: traeCNPayloadRuleInstructions + "\n\nextra guard text",
		},
		{name: "filter_instructions", rules: `{"filter":[{"params":["instructions"]}]}`, changed: true},
		{name: "empty_override_instructions", rules: `{"override":[{"params":{"instructions":""}}]}`, changed: true},
		{name: "same_instructions_preserve_messages", rules: `{"override":[{"params":{"instructions":"original system prompt\n\noriginal developer prompt"}}]}`},
		{name: "default_preserves_messages", rules: `{"default":[{"params":{"instructions":"default must not replace existing prompts"}}]}`},
		{name: "unrelated_rule_preserves_messages", rules: `{"override":[{"params":{"reasoning.effort":"low"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPayloadRules(t, tc.rules)
			for _, protocol := range traeCNPayloadRuleProtocols() {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", protocol.name, stream), func(t *testing.T) {
						resetResponseCacheStateForTest(testResponseCacheConfig())
						t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
						var mu sync.Mutex
						var bodies [][]byte
						handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							mu.Lock()
							bodies = append(bodies, body)
							mu.Unlock()
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, traeCNPayloadRuleReply)
						})
						configureTraeCNPayloadRuleAccount(handler, 71, "selected-pool")
						// Key-allowed groups come from the store, not the row below;
						// group 73 is allowed but has no selected account.
						handler.store.SetGroupName(73, "allowed-only-pool")
						handler.store.SetAPIKeyAllowedGroups(traeCNPayloadRuleKeyID, []int64{71, 73})
						recorder := invokeTraeCNPayloadRuleProtocol(t, handler, protocol, stream, database.UpstreamChannelTraeCN, nil)
						assertTraeCNPayloadRuleResponse(t, recorder, protocol, stream)
						mu.Lock()
						seen := append([][]byte(nil), bodies...)
						mu.Unlock()
						if len(seen) != 1 {
							t.Fatalf("upstream calls=%d, want 1", len(seen))
						}
						prompts := protocol.originalPrompts
						if tc.changed {
							prompts = nil
							if tc.instructions != "" {
								prompts = append(prompts, tc.instructions)
							}
							prompts = append(prompts, protocol.protectedPrompts...)
						}
						assertTraeCNPayloadRuleNativeBody(t, seen[0], prompts)
					})
				}
			}
		})
	}
}

func TestTraeCNPayloadRulesRetryUsesSelectedAccount(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	withPayloadRules(t, `{"append":[
		{"api_key_ids":["92001"],"api_key_names":["payload-*"],"account_group_ids":["71"],"account_group_names":["selected-a"],"params":{"instructions":"account A guard"}},
		{"api_key_ids":["92001"],"api_key_names":["payload-*"],"account_group_ids":["72"],"account_group_names":["selected-b"],"params":{"instructions":"account B guard"}}
	]}`)
	for _, protocol := range traeCNPayloadRuleProtocols() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol.name, stream), func(t *testing.T) {
				resetResponseCacheStateForTest(testResponseCacheConfig())
				t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
				type attempt struct {
					token string
					body  []byte
				}
				var mu sync.Mutex
				var attempts []attempt
				handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					mu.Lock()
					attempts = append(attempts, attempt{token: r.Header.Get("X-Ide-Token"), body: body})
					first := len(attempts) == 1
					mu.Unlock()
					if first {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"code":3004,"message":"Your requests have exceeded the rate limit."}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, traeCNPayloadRuleReply)
				})
				firstAccount := configureTraeCNPayloadRuleAccount(handler, 71, "selected-a")
				handler.store.AddAccount(&auth.Account{
					DBID: 92002, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT-B", RefreshToken: "RT-B",
					ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: firstAccount.TraeCNHost,
					GroupIDs: []int64{72}, TraeCNUpstreamModelCatalog: []string{traeCNPayloadRuleConfigName, "DeepSeek-V4-Pro"},
				})
				handler.store.SetGroupName(72, "selected-b")
				handler.store.SetAPIKeyAllowedGroups(traeCNPayloadRuleKeyID, []int64{71, 72})
				handler.store.SetMaxRateLimitRetries(1)
				handler.store.SetRetryIntervalMS(0)
				recorder := invokeTraeCNPayloadRuleProtocol(t, handler, protocol, stream, database.UpstreamChannelTraeCN, []int64{71, 72})
				assertTraeCNPayloadRuleResponse(t, recorder, protocol, stream)
				mu.Lock()
				seen := append([]attempt(nil), attempts...)
				mu.Unlock()
				if len(seen) != 2 {
					t.Fatalf("upstream calls=%d, want rejected attempt plus one retry", len(seen))
				}
				if seen[0].token == seen[1].token {
					t.Fatalf("429 retry reused account token %q", seen[0].token)
				}
				for index, seenAttempt := range seen {
					guard, ok := map[string]string{"AT": "account A guard", "AT-B": "account B guard"}[seenAttempt.token]
					if !ok {
						t.Fatalf("attempt %d has unexpected dummy credential %q", index+1, seenAttempt.token)
					}
					// Exact equality also rejects a stale previous-account append,
					// duplicate original prompts, or append stacking on re-entry.
					prompts := []string{traeCNPayloadRuleInstructions + "\n\n" + guard}
					prompts = append(prompts, protocol.protectedPrompts...)
					assertTraeCNPayloadRuleNativeBody(t, seenAttempt.body, prompts)
				}
			})
		}
	}
}
