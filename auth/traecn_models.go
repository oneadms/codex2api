package auth

// TraeCNDefaultModelIDs mirrors the provider's catalog for cold start (before
// the first model sync). It carries no compatibility aliases: the model a
// client asks for is the model Trae receives, unless an administrator set an
// explicit mapping in the Trae CN settings.
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
