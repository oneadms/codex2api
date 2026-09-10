package auth

import (
	"slices"
	"testing"
)

func TestTraeCNModelMappingHonorsCatalogAndAllowlist(t *testing.T) {
	previous := ConfiguredTraeCNSettings()
	t.Cleanup(func() { SetConfiguredTraeCNSettings(previous) })
	settings := TraeCNSettings{ModelMapping: map[string]string{
		"my-code":         "doubao-seed-code",
		"claude-opus-4-6": "doubao-seed-code",
		"not-available":   "glm-5.1",
	}}
	SetConfiguredTraeCNSettings(settings)
	account := &Account{UpstreamType: UpstreamTraeCN, AccessToken: "AT", TraeCNUpstreamModelCatalog: []string{"doubao-seed-code", "glm-5.1"}, TraeCNModelAllowlist: []string{"doubao-seed-code"}, TraeCNModelAllowlistSet: true}
	for _, model := range []string{"my-code", "MY-CODE", "claude-opus-4-6", "doubao-seed-code"} {
		if !account.TraeCNSupportsModel(model) {
			t.Errorf("mapped model %q is not routable", model)
		}
	}
	for _, model := range []string{"not-available", "glm-5.1"} {
		if account.TraeCNSupportsModel(model) {
			t.Errorf("mapping bypassed allowlist: %s", model)
		}
	}
	public := TraeCNPublicModels(account.TraeCNEffectiveModels())
	if !slices.Contains(public, "my-code") || slices.Contains(public, "not-available") {
		t.Fatalf("public models = %v", public)
	}
	// 发布后修改调用方持有的 map，不得改变当前配置。
	settings.ModelMapping["my-code"] = "glm-5.1"
	if got := TraeCNRequestModel("my-code"); got != "doubao-seed-code" {
		t.Fatalf("snapshot changed: %s", got)
	}
	SetConfiguredTraeCNSettings(TraeCNSettings{ModelMapping: map[string]string{"doubao-seed-code": "glm-5.1"}})
	if account.TraeCNSupportsModel("doubao-seed-code") || len(TraeCNPublicModels(account.TraeCNEffectiveModels())) != 0 {
		t.Fatal("overriding an existing model bypassed the target allowlist")
	}
	SetConfiguredTraeCNSettings(TraeCNSettings{})
	if account.TraeCNSupportsModel("my-code") || !account.TraeCNSupportsModel("doubao-seed-code") {
		t.Fatal("clearing mappings did not restore defaults")
	}
}

func TestTraeCNSettingsValidation(t *testing.T) {
	for _, raw := range []string{
		`{"model_mapping":{"":"doubao-seed-code"}}`,
		`{"model_mapping":{"my-code":""}}`,
		`{"model_mapping":{"my code":"doubao-seed-code"}}`,
		`{"model_mapping":{"MY-CODE":"glm-5.1","my-code":"doubao-seed-code"}}`,
		`{"model_mapping":{"model-*":"doubao-seed-code"}}`,
		`{"model_mapping":[]}`,
	} {
		if _, err := ParseTraeCNSettings(raw); err == nil {
			t.Errorf("invalid settings accepted: %s", raw)
		}
	}
	settings, err := ParseTraeCNSettings(`{"model_mapping":{" My-Code ":" doubao-seed-code "}}`)
	if err != nil || settings.ModelMapping["My-Code"] != "doubao-seed-code" {
		t.Fatalf("normalized=%v err=%v", settings, err)
	}
}
