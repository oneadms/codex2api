package proxy

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestTraeCNPayloadRulesPreserveNonTextAndHistory(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	withPayloadRules(t, `{"override":[{
		"match":{"instructions":"top prompt\n\nfirst prompt\n\nsecond prompt\n\nthird prompt","input.0.content.0.text":"first prompt","input.1.role":"developer"},
		"params":{"instructions":"replacement","reasoning.effort":"high","temperature":0.2,"service_tier":"priority","max_output_tokens":1}
	}]}`)
	body := []byte(`{
		"model":"payload-public-model","instructions":"top prompt",
		"input":[
			{"type":"message","role":"system","id":"mixed-prompt","content":[
				{"type":"input_text","text":"first prompt"},
				{"type":"input_image","image_url":"https://example.invalid/image.png"},
				{"type":"text","text":"second prompt"},
				{"type":"future_block","text":"opaque text","data":{"keep":true}}
			]},
			{"type":"message","role":"developer","content":"third prompt"},
			{"role":"user","content":"question"},
			{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"result"},
			{"role":"assistant","content":"answer"}
		]
	}`)
	original := bytes.Clone(body)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, TraeCNUpstreamModelCatalog: []string{traeCNPayloadRuleConfigName}}
	for _, protocol := range []GrokProtocol{GrokProtocolChatCompletions, GrokProtocolMessages} {
		t.Run(string(protocol), func(t *testing.T) {
			out := applyTraeCNPayloadRules(context.Background(), account, protocol, body, nil)
			if got := gjson.GetBytes(out, "instructions").String(); got != "replacement" {
				t.Fatalf("instructions=%q; predicates must see both normalized instructions and original input", got)
			}
			input := gjson.GetBytes(out, "input").Array()
			wantInput := gjson.GetBytes(body, "input").Array()
			if len(input) != len(wantInput)-1 {
				t.Fatalf("input=%s, want only the fully lifted developer message removed", gjson.GetBytes(out, "input"))
			}
			wantParts := []any{wantInput[0].Get("content.1").Value(), wantInput[0].Get("content.3").Value()}
			if input[0].Get("id").String() != "mixed-prompt" || !reflect.DeepEqual(input[0].Get("content").Value(), wantParts) {
				t.Errorf("mixed message lost its metadata or non-text parts: %s", input[0].Raw)
			}
			for i := 1; i < len(input); i++ {
				if input[i].Raw != wantInput[i+1].Raw {
					t.Errorf("conversation/tool item changed: %s, want %s", input[i].Raw, wantInput[i+1].Raw)
				}
			}
			native, _, _, _, err := traeCNRequestBodyPlan(out, account.TraeCNEffectiveModels())
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(native, "reasoning_effort").String() != "high" || gjson.GetBytes(native, "temperature").Float() != 0.2 {
				t.Errorf("supported rewritten controls did not reach native request: %s", native)
			}
			if gjson.GetBytes(native, "service_tier").Exists() || gjson.GetBytes(native, "max_output_tokens").Exists() {
				t.Errorf("unsupported controls leaked into native request: %s", native)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("rewrites mutated the caller's request body")
			}
		})
	}
}

func TestTraeCNPayloadRulesNoOpPreservesPromptShape(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	for _, field := range []string{"", `"instructions":null,`, `"instructions":"",`, `"instructions":"top",`} {
		for _, rules := range []string{
			`{}`,
			`{"override":[{"api_key_ids":["92001"],"params":{"instructions":"no identity"}}]}`,
			`{"default":[{"params":{"instructions":"already supplied"}}]}`,
			`{"override":[{"params":{"temperature":0.25}}]}`,
		} {
			t.Run(field+"/"+rules, func(t *testing.T) {
				withPayloadRules(t, rules)
				body := []byte(`{` + field + `"model":"auto","input":[{"role":"developer","content":"one"},{"role":"system","content":[{"type":"input_text","text":"two"}]}]}`)
				out := applyTraeCNPayloadRules(context.Background(), account, GrokProtocolChatCompletions, body, nil)
				if gjson.GetBytes(out, "input").Raw != gjson.GetBytes(body, "input").Raw ||
					gjson.GetBytes(out, "instructions").Raw != gjson.GetBytes(body, "instructions").Raw {
					t.Errorf("no-op prompt rule changed layout: %s, original %s", out, body)
				}
				if strings.Contains(rules, "temperature") {
					if gjson.GetBytes(out, "temperature").Float() != 0.25 {
						t.Errorf("unrelated rewrite was lost: %s", out)
					}
				} else if !bytes.Equal(out, body) {
					t.Errorf("unmatched/no-op rules changed body bytes: %s", out)
				}
			})
		}
	}
}

func TestTraeCNPayloadRulesAppendIdempotent(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	withPayloadRules(t, `{"append":[{"params":{"instructions":"guard"}}]}`)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	body := []byte(`{"model":"auto","input":[{"role":"developer","content":"original"},{"role":"user","content":"question"}]}`)
	once := applyTraeCNPayloadRules(context.Background(), account, GrokProtocolChatCompletions, body, nil)
	twice := applyTraeCNPayloadRules(context.Background(), account, GrokProtocolChatCompletions, once, nil)
	if gjson.GetBytes(twice, "instructions").String() != "original\n\nguard" || !bytes.Equal(once, twice) {
		t.Fatalf("append stacked on reentry: first=%s, second=%s", once, twice)
	}
}

func TestTraeCNPayloadRulesMissingInstructions(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	for _, rules := range []string{
		`{"default":[{"params":{"instructions":"default prompt"}}]}`,
		`{"default_raw":[{"params":{"instructions":"\"default prompt\""}}]}`,
	} {
		t.Run(rules, func(t *testing.T) {
			withPayloadRules(t, rules)
			for _, protocol := range []GrokProtocol{GrokProtocolResponses, GrokProtocolChatCompletions, GrokProtocolMessages} {
				body := []byte(`{"model":"auto","input":[{"role":"user","content":"question"}]}`)
				out := applyTraeCNPayloadRules(context.Background(), account, protocol, body, nil)
				if gjson.GetBytes(out, "instructions").String() != "default prompt" || gjson.GetBytes(out, "input").Raw != gjson.GetBytes(body, "input").Raw {
					t.Errorf("%s default prompt failed: %s", protocol, out)
				}
			}
		})
	}
}

func TestTraeCNPayloadRulesIdentityFailsClosed(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	for _, gate := range []string{
		`"api_key_ids":["*"]`, `"api_key_names":["*"]`,
		`"group_ids":["*"]`, `"group_names":["*"]`,
		`"account_group_ids":["*"]`, `"account_group_names":["*"]`, `"account_plans":["*"]`,
	} {
		t.Run(gate, func(t *testing.T) {
			withPayloadRules(t, `{"override":[{`+gate+`,"params":{"instructions":"must not apply"}}]}`)
			body := []byte(`{"model":"auto","input":[{"role":"developer","content":"original"},{"role":"user","content":"question"}]}`)
			for _, ctx := range []context.Context{context.Background(), WithPayloadRuleIdentity(context.Background(), &PayloadRuleIdentity{})} {
				// API key ID 0 still matches "*" when explicitly supplied; only
				// test unresolved accounts with that partially populated identity.
				if PayloadRuleIdentityFromContext(ctx) != nil && !strings.HasPrefix(gate, `"account_`) {
					continue
				}
				out := applyTraeCNPayloadRules(ctx, account, GrokProtocolChatCompletions, body, http.Header{"Authorization": {"Bearer fake-key"}})
				if !bytes.Equal(out, body) {
					t.Errorf("unresolved identity matched %s: %s", gate, out)
				}
			}
		})
	}
}

func TestTraeCNPayloadRulesExcludedRequests(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	withPayloadRules(t, `{"override":[{"params":{"instructions":"must not apply"}}]}`)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		body string
	}{
		{"detector", WithCodexDetectorRequest(context.Background()), `{"model":"auto","instructions":"probe prompt","input":"question"}`},
		{"image", context.Background(), `{"model":"auto","instructions":"image prompt","input":"question","tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := ObservedInstructions()
			out := applyTraeCNPayloadRules(tc.ctx, account, GrokProtocolResponses, []byte(tc.body), nil)
			if string(out) != tc.body {
				t.Errorf("excluded request was rewritten: %s", out)
			}
			if !reflect.DeepEqual(ObservedInstructions(), before) {
				t.Error("excluded request was captured as an instructions sample")
			}
		})
	}
}

func TestTraeCNPayloadRulesObserveOriginalPrompt(t *testing.T) {
	setupTraeCNPayloadRuleIntegration(t)
	withPayloadRules(t, `{"override":[{"params":{"instructions":"replacement"}}]}`)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN}
	body := []byte(`{"model":"auto","input":[{"role":"developer","content":"observed original one"},{"role":"system","content":"observed original two"}]}`)
	out := applyTraeCNPayloadRules(context.Background(), account, GrokProtocolMessages, body, http.Header{"Originator": {t.Name()}})
	if gjson.GetBytes(out, "instructions").String() != "replacement" || gjson.GetBytes(out, "input.#").Int() != 0 {
		t.Fatalf("prompt-only input was not replaced: %s", out)
	}
	for _, sample := range ObservedInstructions() {
		if sample.Originator == t.Name() && sample.Instructions == "observed original one\n\nobserved original two" {
			return
		}
	}
	t.Fatal("pre-rewrite translated instructions were not observed")
}
