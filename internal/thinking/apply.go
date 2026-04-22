/**
 * 思考配置应用模块
 * 将解析后的思考配置应用到 Codex 请求体中
 * Codex 使用 reasoning.effort 字段设置思考级别
 */
package thinking

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * levelToBudgetMap 级别到预算的映射表
 */
var levelToBudgetMap = map[string]int{
	"none":    0,
	"auto":    -1,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

/**
 * ApplyThinking 将思考配置和服务层级应用到请求体
 * 解析模型名中的思考后缀、-fast、-1m 后缀，写入请求 JSON
 *
 * 无后缀模型（如 gpt-5、gpt-5-codex）且客户端未传思考相关参数时，不设置 reasoning.effort，
 * 即默认按「不传递」处理，由上游按自身策略（如 auto）生效。
 *
 * @param body - 原始请求体 JSON
 * @param model - 模型名
 * @returns []byte - 处理后的请求体 JSON
 * @returns string - 去除思考后缀与 -fast 后的基础模型名
 * @returns bool - 是否启用了 image 模式
 */
func ApplyThinking(body []byte, model string) ([]byte, string, bool) {
	parsed := ParseModelSuffix(model)
	baseModel := strings.TrimSpace(parsed.ModelName)

	if parsed.IsImage {
		if parsed.IsFast {
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
		}
		return body, baseModel, true
	}

	var config ThinkingConfig
	if parsed.HasSuffix {
		config = ParseSuffixToConfig(parsed.RawSuffix)
	} else {
		config = extractConfigFromBody(body)
	}
	if hasThinkingConfig(config) {
		body = applyCodexThinking(body, config)
		if parsed.HasSuffix {
			body, _ = sjson.DeleteBytes(body, "reasoning_effort")
			body, _ = sjson.DeleteBytes(body, "variant")
		}
	}
	if parsed.IsFast {
		body, _ = sjson.SetBytes(body, "service_tier", "priority")
	}

	return body, baseModel, false
}

/**
 * extractConfigFromBody 从请求体中提取思考配置
 * @param body - 请求体 JSON
 * @returns ThinkingConfig - 提取的思考配置
 */
func extractConfigFromBody(body []byte) ThinkingConfig {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ThinkingConfig{}
	}
	if effort := gjson.GetBytes(body, "reasoning.effort"); effort.Exists() {
		value := strings.ToLower(strings.TrimSpace(effort.String()))
		if value == "none" {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		if value != "" {
			return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(value)}
		}
	}
	if effort := gjson.GetBytes(body, "reasoning_effort"); effort.Exists() {
		value := strings.ToLower(strings.TrimSpace(effort.String()))
		if value == "none" {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		if value != "" {
			return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(value)}
		}
	}
	if variant := gjson.GetBytes(body, "variant"); variant.Exists() {
		value := strings.ToLower(strings.TrimSpace(variant.String()))
		if value == "none" {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		if value != "" {
			return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(value)}
		}
	}

	return ThinkingConfig{}
}

/**
 * applyCodexThinking 将思考配置写入 Codex 请求体
 * @param body - 请求体 JSON
 * @param config - 思考配置
 * @returns []byte - 修改后的请求体
 */
func applyCodexThinking(body []byte, config ThinkingConfig) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	var effort string
	switch config.Mode {
	case ModeLevel:
		if config.Level == "" {
			return body
		}
		effort = string(config.Level)
	case ModeNone:
		effort = "none"
	case ModeAuto:
		effort = "medium"
	case ModeBudget:
		effort = budgetToLevel(config.Budget)
		if effort == "" {
			return body
		}
	default:
		return body
	}

	result, _ := sjson.SetBytes(body, "reasoning.effort", effort)
	return result
}

/**
 * budgetToLevel 将数字预算转换为最近的思考级别
 * @param budget - token 预算值
 * @returns string - 对应的思考级别
 */
func budgetToLevel(budget int) string {
	switch {
	case budget <= 0:
		return "none"
	case budget <= 512:
		return "auto"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

/**
 * LevelToBudget 将思考级别转换为预算值
 * @param level - 思考级别字符串
 * @returns int - 预算值
 * @returns bool - 是否为有效级别
 */
func LevelToBudget(level string) (int, bool) {
	budget, ok := levelToBudgetMap[strings.ToLower(level)]
	return budget, ok
}

/**
 * hasThinkingConfig 检查是否包含有效的思考配置
 * @param config - 思考配置
 * @returns bool - 是否有配置
 */
func hasThinkingConfig(config ThinkingConfig) bool {
	return config.Mode != ModeBudget || config.Budget != 0 || config.Level != ""
}

/**
 * StripThinkingConfig 从请求体中移除思考配置字段
 * @param body - 请求体 JSON
 * @returns []byte - 移除后的请求体
 */
func StripThinkingConfig(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	result := body
	result, _ = sjson.DeleteBytes(result, "reasoning.effort")
	result, _ = sjson.DeleteBytes(result, "reasoning_effort")
	return result
}
