/**
 * 思考配置类型定义模块
 * 定义思考模式、级别、配置结构和模型名解析结果
 */
package thinking

/**
 * ThinkingMode 思考配置模式枚举
 */
type ThinkingMode int

const (
	/* ModeBudget 使用数字预算（如 16384） */
	ModeBudget ThinkingMode = iota
	/* ModeLevel 使用离散级别（如 high） */
	ModeLevel
	/* ModeNone 禁用思考 */
	ModeNone
	/* ModeAuto 自动/动态思考 */
	ModeAuto
)

/**
 * String 返回 ThinkingMode 的字符串表示
 * @returns string - 模式名称
 */
func (m ThinkingMode) String() string {
	switch m {
	case ModeBudget:
		return "budget"
	case ModeLevel:
		return "level"
	case ModeNone:
		return "none"
	case ModeAuto:
		return "auto"
	default:
		return "unknown"
	}
}

/**
 * ThinkingLevel 离散思考级别
 */
type ThinkingLevel string

const (
	LevelNone    ThinkingLevel = "none"
	LevelAuto    ThinkingLevel = "auto"
	LevelMinimal ThinkingLevel = "minimal"
	LevelLow     ThinkingLevel = "low"
	LevelMedium  ThinkingLevel = "medium"
	LevelHigh    ThinkingLevel = "high"
	LevelXHigh   ThinkingLevel = "xhigh"
	LevelMax     ThinkingLevel = "max"
)

/**
 * ThinkingConfig 统一的思考配置
 * @field Mode - 配置模式
 * @field Budget - 思考预算（仅 ModeBudget 时有效）
 * @field Level - 思考级别（仅 ModeLevel 时有效）
 */
type ThinkingConfig struct {
	Mode   ThinkingMode
	Budget int
	Level  ThinkingLevel
}

/**
 * ParseResult 模型名解析结果
 * @field ModelName - 去除所有后缀后的真实模型名
 * @field HasSuffix - 是否检测到有效的思考后缀
 * @field RawSuffix - 原始思考后缀值
 * @field IsFast - 是否启用 fast 模式
 * @field Is1M - 是否启用 1M 上下文模式
 * @field ServiceTier - 服务层级
 */
type ParseResult struct {
	ModelName   string
	HasSuffix   bool
	RawSuffix   string
	IsFast      bool
	Is1M        bool
	IsImage     bool
	ServiceTier string
}
