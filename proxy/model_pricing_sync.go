package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// DefaultModelPricingSyncURL uses models.dev reference token prices.
const DefaultModelPricingSyncURL = ModelsDevPricingSyncURL

// ModelsDevPricingSyncURL 是 models.dev 公开定价 API，格式为 provider→models→cost
// （USD / 1M tokens，含 272K 长上下文分档），同步时自动识别并转换。
const ModelsDevPricingSyncURL = "https://models.dev/api.json"

// modelPricingSyncURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var modelPricingSyncURLForTest = ""

// ModelPricingSyncResult 是一次定价同步的结果投影。
type ModelPricingSyncResult struct {
	SourceURL string `json:"source_url"`
	Fetched   int    `json:"fetched"` // 从 URL 解析到的模型数
	Applied   int    `json:"applied"` // 实际写入的 synced 条目数
	Skipped   int    `json:"skipped"` // 因已有 custom 覆盖而跳过的模型数
}

// SyncModelPricingFromURL 从 JSON URL 拉取定价并写入 synced 覆盖。
// custom 覆盖优先级更高，同步不覆盖已有 custom 条目（只跳过、不清除）。
// 成功后持久化到系统设置并刷新运行时覆盖表。
func SyncModelPricingFromURL(ctx context.Context, db *database.DB, syncURL, proxyURL string) (*ModelPricingSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步模型定价")
	}
	// 字段即准：传入的 syncURL 就是设置页里那个 URL 字段的当前值。
	// 非空 → 用它并存为来源；空 → 用内置默认并清空存储的来源（回退默认）。
	fieldURL := strings.TrimSpace(syncURL)
	fetchURL := fieldURL
	if modelPricingSyncURLForTest != "" {
		fetchURL = modelPricingSyncURLForTest
	} else if fetchURL == "" {
		fetchURL = DefaultModelPricingSyncURL
	}
	result := &ModelPricingSyncResult{SourceURL: fetchURL}

	fetched, err := fetchModelPricingJSON(ctx, fetchURL, proxyURL)
	if err != nil {
		return result, err
	}
	result.Fetched = len(fetched)
	if len(fetched) == 0 {
		return result, fmt.Errorf("定价源未解析到任何模型")
	}

	_, err = db.MutateModelPricingSettings(ctx, &fieldURL, func(current map[string]database.ModelPricingOverride) error {
		for model, ov := range fetched {
			key := strings.ToLower(strings.TrimSpace(model))
			if key == "" || ov.IsEmpty() {
				continue
			}
			// 已有 custom 覆盖优先，跳过不动。
			if existing, ok := current[key]; ok && existing.Source == database.ModelPricingSourceCustom {
				result.Skipped++
				continue
			}
			ov.Source = database.ModelPricingSourceSynced
			current[key] = ov
			result.Applied++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func fetchModelPricingJSON(ctx context.Context, syncURL, proxyURL string) (map[string]database.ModelPricingOverride, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, syncURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build pricing request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex2api")

	// 目标是 GitHub 域（默认定价 JSON 在 raw.githubusercontent.com）时套用专用代理；
	// URL 可被部署方改成任意地址，故这里绝不附加 github_token。
	client := &http.Client{Transport: newCodexStandardTransport(GithubProxyOrDefault(syncURL, proxyURL)), Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("pricing upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read pricing response: %w", err)
	}
	return parseModelPricingPayload(body)
}

// modelsDevCostTier 是 models.dev 单个分档价（顶层与 tiers 元素共用形态）。
type modelsDevCostTier struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Tier       struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"tier"`
}

type modelsDevCost struct {
	modelsDevCostTier
	Tiers []modelsDevCostTier `json:"tiers"`
}

type modelsDevModel struct {
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

// parseModelPricingPayload 解析定价源 JSON，自动识别两种格式：
//   - 扁平覆盖表：{ "<model>": {input,cached_input,output,...}, ... }
//   - models.dev api.json：{ "<provider>": { "models": { "<model>": {cost:...} } }, ... }
//
// 扁平格式的值不含 "models" 对象，据此区分。
func parseModelPricingPayload(body []byte) (map[string]database.ModelPricingOverride, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("parse pricing json: %w", err)
	}
	for _, p := range providers {
		if len(p.Models) > 0 {
			return convertModelsDevPricing(providers), nil
		}
	}
	var raw map[string]database.ModelPricingOverride
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse pricing json: %w", err)
	}
	return raw, nil
}

// modelsDevFirstPartyProviders selects vendor prices, preferring China-region
// endpoints for models used by TRAECN. Resellers and subscription plans are excluded.
// 其余 provider 是转售商，同名模型加价不一，取了会串价，故不参与。
var modelsDevFirstPartyProviders = []string{
	"openai", "xai", "anthropic", "google",
	"deepseek", "zhipuai", "moonshotai-cn", "minimax-cn", "alibaba-cn", "volcengine",
	"zai", "moonshotai", "minimax", "alibaba",
}

// convertModelsDevPricing 把 models.dev 数据转成覆盖表。只取第一方 provider
// （其余 provider 的同名模型定价可能不同，避免串价）；键归一为规范定价键。
// 两轮写入：先写模型 ID 即规范键的条目，别名 ID（如 chat-latest 变体）只在
// 规范键尚无条目时补位，避免别名价覆盖正主价。
func convertModelsDevPricing(providers map[string]modelsDevProvider) map[string]database.ModelPricingOverride {
	models := mergeModelsDevProviders(providers, modelsDevFirstPartyProviders)
	if len(models) == 0 {
		// 自建镜像可能只留了别的 provider 名，退化为合并全部（按名序保证确定性）。
		providerNames := make([]string, 0, len(providers))
		for name := range providers {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
		models = mergeModelsDevProviders(providers, providerNames)
	}

	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make(map[string]database.ModelPricingOverride, len(models))
	for _, exactPass := range []bool{true, false} {
		for _, id := range ids {
			key := database.PricingManagementModelKey(id)
			if key == "" || models[id].Cost == nil {
				continue
			}
			if exact := key == strings.ToLower(strings.TrimSpace(id)); exact != exactPass {
				continue
			}
			if _, ok := out[key]; ok {
				continue
			}
			ov := modelsDevCostToOverride(*models[id].Cost)
			if ov.IsEmpty() {
				continue
			}
			out[key] = ov
		}
	}
	// Trae config names differ from the names published by the model vendors.
	// Keep aliases independent so administrators can still override each row.
	for alias, target := range map[string]string{
		"doubao-seed-2.1-pro":        "doubao-seed-2-1-pro-260628",
		"doubao-seed-2.1-turbo":      "doubao-seed-2-1-turbo-260628",
		"deepseek-v4-pro-official":   "deepseek-v4-pro",
		"deepseek-v4-flash-official": "deepseek-v4-flash",
		"qwen-3.7-plus":              "qwen3.7-plus",
	} {
		if _, exists := out[alias]; exists {
			continue
		}
		if price, exists := out[target]; exists {
			out[alias] = price
		}
	}
	return out
}

// mergeModelsDevProviders 按给定顺序合并多个 provider 的模型表，先出现的 provider
// 在同名模型上优先（列表按优先级排列，缺失的 provider 直接跳过）。
func mergeModelsDevProviders(providers map[string]modelsDevProvider, names []string) map[string]modelsDevModel {
	merged := make(map[string]modelsDevModel)
	for _, name := range names {
		for id, m := range providers[name].Models {
			if _, ok := merged[id]; ok {
				continue
			}
			merged[id] = m
		}
	}
	return merged
}

func modelsDevCostToOverride(cost modelsDevCost) database.ModelPricingOverride {
	ov := database.ModelPricingOverride{
		Input:        cost.Input,
		CachedInput:  cost.CacheRead,
		CacheWrite5m: cost.CacheWrite,
		Output:       cost.Output,
	}
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			continue
		}
		ov.ContextTiers = append(ov.ContextTiers, database.ModelPricingContextTier{
			ThresholdTokens: int(tier.Tier.Size), Input: tier.Input, Output: tier.Output,
			CachedInput: tier.CacheRead, CacheWrite5m: tier.CacheWrite,
		})
	}
	sort.Slice(ov.ContextTiers, func(i, j int) bool { return ov.ContextTiers[i].ThresholdTokens < ov.ContextTiers[j].ThresholdTokens })
	if len(ov.ContextTiers) > 0 {
		first := ov.ContextTiers[0]
		ov.LongContextThresholdTokens = first.ThresholdTokens
		ov.InputLong, ov.OutputLong, ov.CachedInputLong = first.Input, first.Output, first.CachedInput
	}
	// A single band uses the existing long-context and service-tier rules.
	if len(ov.ContextTiers) == 1 {
		ov.ContextTiers = nil
	}
	return ov
}
