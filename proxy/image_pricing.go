package proxy

import (
	"bufio"
	"strings"

	"github.com/codex2api/database"
)

// 官方图片价格按同一模型的 Image/Text 两行发布；只读取图片章节第一张
// Standard 表，避免后面的 Batch 价格覆盖线上同步请求的费率。
func parseGPTImage25Pricing(body []byte) map[string]database.ModelPricingOverride {
	out := make(map[string]database.ModelPricingOverride)
	text := string(body)
	start := strings.Index(text, "Image generation models")
	if start < 0 {
		return out
	}
	text = text[start:]
	start = strings.Index(text, "### Grouped Pricing Table data")
	if start < 0 {
		return out
	}
	scanner := bufio.NewScanner(strings.NewReader(text[start:]))
	seenRows := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") {
			if seenRows {
				break
			}
			continue
		}
		seenRows = true
		cells := splitMarkdownRow(line)
		if len(cells) != 5 {
			continue
		}
		model := database.GPTImage25BillingModel(normalizeOfficialPricingModel(cells[0]))
		if model == "" {
			continue
		}
		price := out[model]
		switch strings.ToLower(cells[1]) {
		case "image":
			price.ImageInput, price.CachedImageInput, price.Output = parseOfficialPrice(cells[2]), parseOfficialPrice(cells[3]), parseOfficialPrice(cells[4])
		case "text":
			price.Input, price.CachedInput = parseOfficialPrice(cells[2]), parseOfficialPrice(cells[3])
		}
		out[model] = price
	}
	for model, price := range out {
		if price.Input <= 0 || price.ImageInput <= 0 || price.Output <= 0 {
			delete(out, model)
		}
	}
	return out
}
