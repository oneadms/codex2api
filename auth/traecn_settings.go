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

// 映射只应用一次，目标就是上游的 config_name（provider 的逐字写法，大小写敏感）。
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
		if TraeCNModelsEquivalent(candidate, model) {
			return true
		}
	}
	return false
}

// TraeCNPublicModels 拼出对外可见的 TRAECN 目录：账号目录原样公开，再加上管理员在
// TRAECN 模型映射里配置的对外名称。
//
// 别名表删除后映射目标就是上游模型名，通常与目录里的写法只差大小写或分隔符
// （Doubao_1_6 vs doubao-1-6），按 traeCNModelsEquivalent 匹配即可；目标不在账号
// 目录（含允许清单收窄后的结果）里的映射不对外暴露，避免别名绕过账号限制。
func TraeCNPublicModels(catalog []string) []string {
	snapshot, _ := configuredTraeCNSettings.Load().(traeCNSettingsSnapshot)
	models := make([]string, 0, len(catalog)+len(snapshot.settings.ModelMapping))
	seen := make(map[string]bool, len(catalog)+len(snapshot.settings.ModelMapping))
	add := func(model string) {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			return
		}
		// 覆盖已有名称时同样校验映射目标：映射把某个名字指到账号服务不了的模型时，
		// 这个名字不对外暴露，避免出现「列表里看得到、请求必然失败」的条目。
		if seen[key] || !traeCNModelInCatalog(traeCNRequestModel(snapshot, model), catalog) {
			return
		}
		seen[key] = true
		models = append(models, model)
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
