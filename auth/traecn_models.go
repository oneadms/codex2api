package auth

import (
	"sort"
	"strings"
)

// traeCNWireModels mirrors the current Trae desktop model catalog. Public
// compatibility aliases stay visible to clients, while llm_utils_chat receives
// the concrete provider model in its `model` field (never `config_name`).
var traeCNWireModels = map[string]string{
	"claude-opus-4-7":            "glm-5.2",
	"claude-opus-4-6":            "glm-5.2",
	"claude-opus-4-5":            "glm-5.2",
	"claude-opus-4-5-20251101":   "glm-5.2",
	"claude-sonnet-4-6":          "glm-5.2",
	"claude-sonnet-4-5":          "glm-5.2",
	"claude-sonnet-4-5-20250929": "glm-5.2",
	"claude-sonnet-4":            "glm-5.2",
	"claude-3.7-sonnet":          "glm-5.2",
	"claude-3.5-sonnet":          "glm-5.2",
	"claude-haiku-4-5":           "glm-5.1",
	"claude-haiku-4-5-20251001":  "glm-5.1",
	"deepseek-v3":                "DeepSeek-V4-Pro",
	"deepseek-v4-pro":            "DeepSeek-V4-Pro",
	"deepseek-v4-flash":          "DeepSeek-V4-Flash",
	"deepseek-r1":                "custom_model_deepseek_reasoner",
	"doubao-seed-code":           "Doubao_1_6",
	"doubao-seed-2.0-code":       "Doubao-Seed-2.0-Code",
	"doubao-1.8":                 "doubao_1_8",
	"doubao-1-6":                 "Doubao_1_6",
	"qwen3.7-plus":               "qwen-3.7-plus",
	"qwen3.6-plus":               "qwen-3.6-plus",
	"qwen3-coder":                "qwen3-coder",
	"gpt-4o":                     "custom_model_gpt-5",
	"gpt-4o-mini":                "custom_model_gpt-5",
	"gemini-2.0-flash":           "custom_model_gemini",
	"gemini-2.5-pro":             "custom_model_vercel_gemini",
	"kimi-k2-5":                  "kimi-k2.5",
	"qwen-3-5":                   "qwen-3.5",
	"doubao-seed-2-1-pro":        "Doubao-Seed-2.1-Pro",
	"doubao-seed-2-1-turbo":      "Doubao-Seed-2.1-Turbo",
	"kimi-k2-7-code":             "kimi-k2.7-code",
	"deepseek-v3-1":              "deepseek-V3.1",
}

// TraeCNWireModel resolves a public model ID to the concrete identifier sent
// to Trae. Unknown account-defined models pass through unchanged.
func TraeCNWireModel(model string) string {
	model = strings.TrimSpace(model)
	if mapped := traeCNWireModels[strings.ToLower(model)]; mapped != "" {
		return mapped
	}
	return model
}

// TraeCNPublicModelIDsForWire returns the public compatibility IDs that map to
// a concrete Trae config/model name.  The upstream detail endpoint exposes
// config_name values, while clients use the IDs from the local OpenAI surface;
// keeping this reverse lookup next to TraeCNWireModel prevents those two
// catalogs from drifting apart.
func TraeCNPublicModelIDsForWire(wire string) []string {
	wire = strings.TrimSpace(wire)
	if wire == "" {
		return nil
	}
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for publicID, mappedWire := range traeCNWireModels {
		if !strings.EqualFold(strings.TrimSpace(mappedWire), wire) {
			continue
		}
		key := strings.ToLower(publicID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ids = append(ids, publicID)
	}
	sort.Strings(ids)
	return ids
}

// TraeCNDefaultModelIDs is the built-in logical model catalog exposed by the
// Trae CN wrapper. The provider may add models over time; account-level models
// can narrow this list without changing the global catalog.
func TraeCNDefaultModelIDs() []string {
	return []string{
		"claude-3.5-sonnet",
		"claude-3.7-sonnet",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
		"claude-opus-4-5",
		"claude-opus-4-5-20251101",
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-sonnet-4",
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-6",
		"deepseek-r1",
		"deepseek-v3",
		"deepseek-v3-1",
		"deepseek-v4-flash",
		"deepseek-v4-pro",
		"doubao-1-6",
		"doubao-1.8",
		"doubao-seed-2-1-pro",
		"doubao-seed-2-1-turbo",
		"doubao-seed-2.0-code",
		"doubao-seed-code",
		"gemini-2.0-flash",
		"gemini-2.5-pro",
		"glm-4.6",
		"glm-4.7",
		"glm-5",
		"glm-5.1",
		"glm-5.2",
		"glm-5v-turbo",
		"gpt-4o",
		"gpt-4o-mini",
		"kimi-k2",
		"kimi-k2-5",
		"kimi-k2-7-code",
		"kimi-k2.6",
		"minimax-m2.7",
		"minimax-m3",
		"qwen-3-5",
		"qwen3-coder",
		"qwen3.6-plus",
		"qwen3.7-plus",
		"auto",
	}
}
