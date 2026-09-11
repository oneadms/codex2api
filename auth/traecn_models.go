package auth

// TraeCNDefaultModelIDs mirrors the provider's catalog for cold start (before
// the first model sync). It carries no compatibility aliases: the model a
// client asks for is the model Trae receives, unless an administrator set an
// explicit mapping in the Trae CN settings.
func TraeCNDefaultModelIDs() []string {
	// 这些是 provider 的真实 config_name（Trae 用它区分后端，大小写敏感），
	// 与批量目录接口返回的写法逐字一致。对外友好名请用 TRAECN 模型映射。
	return []string{
		"auto",
		"Doubao-Seed-Evolving",
		"Doubao-Seed-2.1-Pro",
		"Doubao-Seed-2.1-Turbo",
		"Doubao-Seed-Code",
		"Doubao_1_6",
		"doubao-for-auto",
		"glm-5.3-flash",
		"glm-5.3",
		"glm-5.2",
		"glm-5.1",
		"glm-5",
		"glm-4.7",
		"glm-4.7-auto",
		"glm-4.6",
		"DeepSeek-V4-Flash-Official",
		"DeepSeek-V4-Flash",
		"DeepSeek-V4-Pro-Official",
		"DeepSeek-V4-Pro",
		"kimi-k3",
		"kimi-k2.7-code",
		"kimi-k2",
		"minimax-m3",
		"minimax-m2.1",
		"minimax-m2",
		"qwen3.8-flash",
		"qwen3.8-max",
		"qwen-3.7-plus",
		"qwen-3.5",
		"qwen3-coder",
	}
}
