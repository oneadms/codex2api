package auth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/codex2api/security"
)

// TraeCNSettings 保存 TRAECN 独立的渠道配置，不参与其他渠道的模型解析。
type TraeCNSettings struct {
	ModelMapping map[string]string `json:"model_mapping"`
}

const TraeCNModelMappingMaxEntries = 200

type traeCNSettingsSnapshot struct {
	settings TraeCNSettings
	targets  map[string]string
}

var configuredTraeCNSettings atomic.Value // traeCNSettingsSnapshot

func cloneTraeCNSettings(settings TraeCNSettings) TraeCNSettings {
	out := TraeCNSettings{ModelMapping: make(map[string]string, len(settings.ModelMapping))}
	for from, to := range settings.ModelMapping {
		out.ModelMapping[from] = to
	}
	return out
}

// SetConfiguredTraeCNSettings 在持久化成功后发布不可变快照。
func SetConfiguredTraeCNSettings(settings TraeCNSettings) {
	snapshot := traeCNSettingsSnapshot{settings: cloneTraeCNSettings(settings), targets: make(map[string]string, len(settings.ModelMapping))}
	for from, to := range settings.ModelMapping {
		snapshot.targets[strings.ToLower(from)] = to
	}
	configuredTraeCNSettings.Store(snapshot)
}

func ConfiguredTraeCNSettings() TraeCNSettings {
	snapshot, _ := configuredTraeCNSettings.Load().(traeCNSettingsSnapshot)
	return cloneTraeCNSettings(snapshot.settings)
}

func NormalizeTraeCNSettings(settings TraeCNSettings) (TraeCNSettings, error) {
	out := TraeCNSettings{ModelMapping: map[string]string{}}
	if len(settings.ModelMapping) > TraeCNModelMappingMaxEntries {
		return out, fmt.Errorf("TRAE 模型映射最多允许 %d 条", TraeCNModelMappingMaxEntries)
	}
	seen := make(map[string]bool, len(settings.ModelMapping))
	for from, to := range settings.ModelMapping {
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if from == "" || to == "" {
			return out, fmt.Errorf("模型名称和目标模型不能为空")
		}
		if err := security.ValidateModelName(from); err != nil {
			return out, fmt.Errorf("对外模型名 %q 无效: %w", from, err)
		}
		if err := security.ValidateModelName(to); err != nil {
			return out, fmt.Errorf("目标模型 %q 无效: %w", to, err)
		}
		key := strings.ToLower(from)
		if seen[key] {
			return out, fmt.Errorf("对外模型名 %q 重复（不区分大小写）", from)
		}
		seen[key] = true
		out.ModelMapping[from] = to
	}
	return out, nil
}

func ParseTraeCNSettings(raw string) (TraeCNSettings, error) {
	var settings TraeCNSettings
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
			return settings, err
		}
	}
	return NormalizeTraeCNSettings(settings)
}

// 映射只应用一次，目标是 TRAE 的模型目录名称，再由内置表转换成上游名称。
// 这样允许覆盖已有名称，也不会因两条规则互相引用而循环解析。
func traeCNRequestModel(snapshot traeCNSettingsSnapshot, model string) string {
	model = strings.TrimSpace(model)
	if target, ok := snapshot.targets[strings.ToLower(model)]; ok {
		return target
	}
	return model
}

func TraeCNRequestModel(model string) string {
	snapshot, _ := configuredTraeCNSettings.Load().(traeCNSettingsSnapshot)
	return traeCNRequestModel(snapshot, model)
}

func traeCNModelInCatalog(model string, catalog []string) bool {
	for _, candidate := range catalog {
		if traeCNModelsEquivalent(candidate, model) {
			return true
		}
	}
	return false
}

// TraeCNPublicModels 仅公开目标仍在账号目录和白名单中的别名。
// 覆盖已有名称时同样校验映射目标，避免别名绕过账号的模型限制。
func TraeCNPublicModels(catalog []string) []string {
	snapshot, _ := configuredTraeCNSettings.Load().(traeCNSettingsSnapshot)
	models := make([]string, 0, len(catalog)+len(snapshot.targets))
	available := make(map[string]bool, len(catalog))
	for _, model := range catalog {
		available[strings.ToLower(model)] = true
	}
	seen := make(map[string]bool)
	add := func(model string) {
		key := strings.ToLower(model)
		target := traeCNRequestModel(snapshot, model)
		if !seen[key] && (available[strings.ToLower(target)] || traeCNModelInCatalog(target, catalog)) {
			seen[key] = true
			models = append(models, model)
		}
	}
	for _, model := range catalog {
		add(model)
	}
	for from := range snapshot.settings.ModelMapping {
		add(from)
	}
	sort.Strings(models)
	return models
}
