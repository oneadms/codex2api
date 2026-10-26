package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyTraeCNPayloadRules runs after protocol translation, but before the native
// TRAE envelope and adapter tool hints are built. Each attempt starts with its
// original canonical body, so account-scoped rewrites never leak into a retry.
func applyTraeCNPayloadRules(ctx context.Context, account *auth.Account, inbound GrokProtocol, canonical []byte, headers http.Header) []byte {
	if isCodexDetectorRequest(ctx) || responsesBodyRequestsImageGeneration(canonical) {
		return canonical
	}

	ruleBody := canonical
	var remainingInput []json.RawMessage
	switch auth.NormalizeGrokProtocol(string(inbound)) {
	case GrokProtocolChatCompletions, GrokProtocolMessages:
		// These translators represent system prompts as input messages, not
		// instructions. Expose their text to the existing instructions rules,
		// without altering input while predicates are still being evaluated.
		ruleBody, remainingInput = normalizeTraeCNPayloadRuleInstructions(canonical)
	}
	RecordObservedInstructions(ruleBody, headers)
	// Match the provider's effective config name, while leaving model in the
	// body untouched: changing it here would apply TRAE aliases a second time.
	model := traeCNUsageEffectiveModel(account, gjson.GetBytes(canonical, "model").String())
	rewritten := ApplyPayloadRulesToBody(ruleBody, model, headers, PayloadRuleIdentityFromContext(ctx))
	if bytes.Equal(rewritten, ruleBody) {
		return canonical
	}
	if remainingInput == nil {
		// Native Responses keeps its protected input, including system and
		// developer items; only its explicit instructions field is rewritten.
		return rewritten
	}

	before, after := gjson.GetBytes(ruleBody, "instructions"), gjson.GetBytes(rewritten, "instructions")
	if after.Type == gjson.String && before.String() == after.String() {
		// An unrelated or same-value rule must not merge the original prompt
		// messages or change their positions. Restore the original field shape.
		original := gjson.GetBytes(canonical, "instructions")
		var err error
		if original.Exists() {
			rewritten, err = sjson.SetRawBytes(rewritten, "instructions", []byte(original.Raw))
		} else {
			rewritten, err = sjson.DeleteBytes(rewritten, "instructions")
		}
		if err != nil {
			return canonical
		}
		return rewritten
	}

	// Only changed instructions consume the lifted text. Keep all conversation
	// and tool history, as well as non-text parts of mixed system messages.
	input, err := json.Marshal(remainingInput)
	if err != nil {
		return canonical
	}
	rewritten, err = sjson.SetRawBytes(rewritten, "input", input)
	if err != nil {
		return canonical
	}
	return rewritten
}

// normalizeTraeCNPayloadRuleInstructions creates a temporary instructions view of the
// translated system/developer text. The returned input is installed only if a
// rule changes instructions; nil means there was nothing safe to lift.
func normalizeTraeCNPayloadRuleInstructions(canonical []byte) ([]byte, []json.RawMessage) {
	root := gjson.ParseBytes(canonical)
	instructions, input := root.Get("instructions"), root.Get("input")
	if !input.IsArray() || (instructions.Exists() && instructions.Type != gjson.String && instructions.Type != gjson.Null) {
		return canonical, nil
	}
	var fragments []string
	if instructions.Type == gjson.String && instructions.String() != "" {
		fragments = append(fragments, instructions.String())
	}
	remaining := make([]json.RawMessage, 0)
	lifted := false
	for _, item := range input.Array() {
		text, rest, ok := traeCNPayloadRulePrompt(item)
		if !ok {
			remaining = append(remaining, json.RawMessage(item.Raw))
			continue
		}
		lifted = true
		fragments = append(fragments, text...)
		if rest != "" {
			remaining = append(remaining, json.RawMessage(rest))
		}
	}
	if !lifted {
		return canonical, nil
	}
	body, err := sjson.SetBytes(canonical, "instructions", strings.Join(fragments, "\n\n"))
	if err != nil {
		return canonical, nil
	}
	return body, remaining
}

// traeCNPayloadRulePrompt separates only recognized textual prompt parts. It
// deliberately avoids generic text extraction, which could flatten images,
// files or future content types and silently discard their payloads.
func traeCNPayloadRulePrompt(item gjson.Result) (text []string, remainder string, lifted bool) {
	role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
	typ := strings.TrimSpace(item.Get("type").String())
	if (role != "system" && role != "developer") || (typ != "" && typ != "message") {
		return nil, "", false
	}
	// A malformed/provider-specific system message containing tool history is
	// not a plain prompt. Preserve it rather than consuming its other fields.
	if item.Get("tool_calls").Exists() || item.Get("tool_call_id").Exists() {
		return nil, "", false
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		return []string{content.String()}, "", true
	}
	if !content.IsArray() {
		return nil, "", false
	}
	var remaining []json.RawMessage
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "input_text", "output_text", "text":
			if value := part.Get("text"); value.Type == gjson.String {
				text = append(text, value.String())
				lifted = true
				continue
			}
		}
		remaining = append(remaining, json.RawMessage(part.Raw))
	}
	if !lifted || len(remaining) == 0 {
		return text, "", lifted
	}
	encoded, err := json.Marshal(remaining)
	if err != nil {
		return nil, "", false
	}
	remainder, err = sjson.SetRaw(item.Raw, "content", string(encoded))
	if err != nil {
		return nil, "", false
	}
	return text, remainder, true
}
