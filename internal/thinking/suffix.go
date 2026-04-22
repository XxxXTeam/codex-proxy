package thinking

import (
	"strconv"
	"strings"
)

/**
 * validThinkingSuffixes 存储所有有效的思考级别后缀
 * 用于快速判断尾段是否为思考配置
 */
var validThinkingSuffixes = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
	"max":     true,
	"none":    true,
	"auto":    true,
}

/**
 * knownAmbiguousModels 以思考关键字结尾的完整基础模型名白名单
 * 这些模型名的尾段虽然与思考后缀同名，但实际是模型名的一部分
 * 仅精确匹配整个模型名，不使用模式匹配，避免误伤其他模型的后缀解析
 */
var knownAmbiguousModels = map[string]bool{
	"gpt-5.1-codex-max": true,
}

/**
 * ParseModelSuffix 从模型名尾部逆向解析后缀
 * @param model - 原始模型名
 * @returns ParseResult - 解析结果
 */
func ParseModelSuffix(model string) ParseResult {
	model = strings.TrimSpace(model)
	if model == "" {
		return ParseResult{ModelName: model}
	}

	result := ParseResult{}
	lower := strings.ToLower(model)
	if strings.HasSuffix(lower, "-fast") && len(model) > 5 {
		result.IsFast = true
		result.ServiceTier = "fast"
		model = model[:len(model)-5]
		lower = strings.ToLower(model)
	}
	if strings.HasSuffix(lower, "-image") && len(model) > 6 {
		result.IsImage = true
		model = model[:len(model)-6]
		lower = strings.ToLower(model)
	}
	if !result.IsImage {
		if strings.HasSuffix(lower, "-1m") && len(model) > 3 {
			result.Is1M = true
			model = model[:len(model)-3]
			lower = strings.ToLower(model)
		}
		lastDash := strings.LastIndex(model, "-")
		if lastDash > 0 && lastDash < len(model)-1 {
			tail := strings.ToLower(model[lastDash+1:])
			isAmbiguous := knownAmbiguousModels[strings.ToLower(model)]

			if !isAmbiguous {
				if validThinkingSuffixes[tail] {
					/* 匹配到思考级别后缀 */
					result.HasSuffix = true
					result.RawSuffix = tail
					model = model[:lastDash]
				} else if v, err := strconv.Atoi(tail); err == nil && v > 100 {
					result.HasSuffix = true
					result.RawSuffix = tail
					model = model[:lastDash]
				}
			}
		}
	}
	result.ModelName = model
	return result
}

/**
 * ParseSuffixToConfig 将原始后缀字符串转换为 ThinkingConfig
 * @param rawSuffix - 原始后缀字符串
 * @returns ThinkingConfig - 解析后的思考配置
 */
func ParseSuffixToConfig(rawSuffix string) ThinkingConfig {
	rawSuffix = strings.TrimSpace(strings.ToLower(rawSuffix))
	if rawSuffix == "" {
		return ThinkingConfig{}
	}
	switch rawSuffix {
	case "none":
		return ThinkingConfig{Mode: ModeNone, Budget: 0}
	case "auto", "-1":
		return ThinkingConfig{Mode: ModeAuto, Budget: -1}
	}
	switch rawSuffix {
	case "minimal":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelMinimal}
	case "low":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelLow}
	case "medium":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelMedium}
	case "high":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelHigh}
	case "xhigh":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelXHigh}
	case "max":
		return ThinkingConfig{Mode: ModeLevel, Level: LevelMax}
	}
	if value, err := strconv.Atoi(rawSuffix); err == nil {
		if value == 0 {
			return ThinkingConfig{Mode: ModeNone, Budget: 0}
		}
		if value > 0 {
			return ThinkingConfig{Mode: ModeBudget, Budget: value}
		}
	}
	return ThinkingConfig{}
}
