package executor

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	promptCacheMarkerPrefix = "<!-- codex-proxy-prompt-cache:"
	promptCacheMarkerSuffix = " -->"
	promptCacheMinBlockSize = 256
)

// PromptCacheReplacement 用显式正则规则把 system/developer 提示词中的易变片段替换为稳定文本。
// 规则仅处理 instructions 和可安全提升的前置 developer message，不处理用户、助手或工具内容。
type PromptCacheReplacement struct {
	Pattern string
	Replace string
}

type compiledPromptCacheReplacement struct {
	pattern *regexp.Regexp
	replace string
}

// PromptCacheOptions 是请求提示词缓存优化的不可变配置快照。
// 通过 NewPromptCacheOptions 构造；配置校验层保证传入的正则表达式有效。
type PromptCacheOptions struct {
	Enabled             bool
	Tag                 string
	NormalizeWhitespace bool
	MergeRepeatedBlocks bool
	replacements        []compiledPromptCacheReplacement
}

// NewPromptCacheOptions 构造缓存优化配置。无效正则不会进入运行时规则；配置文件加载路径会在启动前报告该错误。
func NewPromptCacheOptions(enabled bool, tag string, normalizeWhitespace bool, mergeRepeatedBlocks bool, replacements []PromptCacheReplacement) PromptCacheOptions {
	compiled := make([]compiledPromptCacheReplacement, 0, len(replacements))
	for _, rule := range replacements {
		if strings.TrimSpace(rule.Pattern) == "" {
			continue
		}
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			continue
		}
		compiled = append(compiled, compiledPromptCacheReplacement{
			pattern: pattern,
			replace: rule.Replace,
		})
	}
	return PromptCacheOptions{
		Enabled:             enabled,
		Tag:                 strings.TrimSpace(tag),
		NormalizeWhitespace: normalizeWhitespace,
		MergeRepeatedBlocks: mergeRepeatedBlocks,
		replacements:        compiled,
	}
}

// NewPromptCacheFingerprint 创建不携带指纹、粘性或 turn-state 状态的请求缓存优化上下文。
// 供 Claude 等尚未接入指纹收敛的入口使用，避免提示词缓存优化意外改变其身份字段。
func NewPromptCacheFingerprint(options PromptCacheOptions) *RequestsFingerprint {
	if !options.Enabled {
		return nil
	}
	return &RequestsFingerprint{promptCache: options}
}

// OptimizeCacheForCodex 对已转换为 Responses 格式的请求体执行保守缓存优化。
// 仅重排不含 previous_response_id 的前置纯文本 developer message；连续对话不会提升 message，防止改变历史回合语义。
func (r *RequestsFingerprint) OptimizeCacheForCodex(body []byte, _ string) []byte {
	if r == nil || !r.promptCache.Enabled {
		return body
	}
	return optimizeCodexPromptCache(body, r.promptCache)
}

// SetPromptCacheOptions 绑定本次请求使用的不可变缓存优化配置。
func (r *RequestsFingerprint) SetPromptCacheOptions(options PromptCacheOptions) {
	if r == nil {
		return
	}
	r.promptCache = options
}

// optimizeCodexPromptCache 将稳定的前置 developer 提示词置入 instructions，以保证请求前缀稳定。
func optimizeCodexPromptCache(body []byte, options PromptCacheOptions) []byte {
	if !options.Enabled || !gjson.ParseBytes(body).IsObject() {
		return body
	}

	instructions, instructionsOK := promptCacheInstructions(body)
	if !instructionsOK {
		return body
	}
	instructions = options.prepareText(instructions)

	result := body
	moved := 0
	input := gjson.GetBytes(body, "input")
	if previousResponseIDFromBody(body) == "" && input.IsArray() {
		items := input.Array()
		for _, item := range items {
			text, ok := promptCacheLeadingDeveloperText(item)
			if !ok {
				break
			}
			instructions = joinPromptCacheText(instructions, options.prepareText(text), options.MergeRepeatedBlocks)
			moved++
		}
		if moved > 0 {
			result = replacePromptCacheInput(result, items[moved:])
		}
	}

	if options.MergeRepeatedBlocks {
		instructions = deduplicateLargePromptBlocks(instructions)
	}
	if marker := promptCacheMarker(options.Tag); marker != "" && !strings.HasPrefix(instructions, marker) {
		instructions = joinPromptCacheText(marker, instructions, false)
	}
	if instructions == gjson.GetBytes(body, "instructions").String() && moved == 0 {
		return body
	}
	updated, err := sjson.SetBytes(result, "instructions", instructions)
	if err != nil {
		return body
	}
	return updated
}

// promptCacheInstructions 读取可安全改写的 instructions。不存在时按空字符串创建；非字符串值不改写。
func promptCacheInstructions(body []byte) (string, bool) {
	value := gjson.GetBytes(body, "instructions")
	if !value.Exists() {
		return "", true
	}
	if value.Type != gjson.String {
		return "", false
	}
	return value.String(), true
}

// promptCacheLeadingDeveloperText 仅接受纯文本的 developer message，避免改写含图像、文件或其它结构化 content 的消息。
func promptCacheLeadingDeveloperText(item gjson.Result) (string, bool) {
	if item.Get("type").String() != "message" || item.Get("role").String() != "developer" {
		return "", false
	}
	content := item.Get("content")
	if content.Type == gjson.String && content.String() != "" {
		return content.String(), true
	}
	if !content.IsArray() {
		return "", false
	}
	parts := content.Array()
	if len(parts) != 1 || parts[0].Get("type").String() != "input_text" {
		return "", false
	}
	text := parts[0].Get("text")
	if text.Type != gjson.String || text.String() == "" {
		return "", false
	}
	return text.String(), true
}

// replacePromptCacheInput 删除已提升到 instructions 的前置消息，其余 input item 保持原始 JSON 片段与顺序。
func replacePromptCacheInput(body []byte, items []gjson.Result) []byte {
	if len(items) == 0 {
		updated, err := sjson.SetRawBytes(body, "input", []byte("[]"))
		if err != nil {
			return body
		}
		return updated
	}
	rawItems := make([]string, 0, len(items))
	for _, item := range items {
		rawItems = append(rawItems, item.Raw)
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rawItems, ",")+"]"))
	if err != nil {
		return body
	}
	return updated
}

// prepareText 对明确定义的稳定区执行替换与可选格式化；规则以配置列表顺序依次应用。
func (o PromptCacheOptions) prepareText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, rule := range o.replacements {
		text = rule.pattern.ReplaceAllStringFunc(text, func(string) string {
			return rule.replace
		})
	}
	if !o.NormalizeWhitespace {
		return text
	}
	return normalizePromptCacheWhitespace(text)
}

// normalizePromptCacheWhitespace 统一稳定提示词的行尾空白、首尾空行与连续空行；仅在显式配置开启时调用。
func normalizePromptCacheWhitespace(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, 0, len(lines))
	previousBlank := false
	for _, line := range lines {
		blank := line == ""
		if blank && previousBlank {
			continue
		}
		out = append(out, line)
		previousBlank = blank
	}
	return strings.Join(out, "\n")
}

// joinPromptCacheText 用固定双换行连接非空文本段，保持 instructions 中的语义顺序。
func joinPromptCacheText(left, right string, _ bool) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + "\n\n" + right
}

// deduplicateLargePromptBlocks 合并同一 instructions 中完全相同且长度足够大的文本块，避免短重复列表项被错误折叠。
func deduplicateLargePromptBlocks(text string) string {
	blocks := strings.Split(text, "\n\n")
	seen := make(map[string]struct{}, len(blocks))
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		key := strings.TrimSpace(block)
		if len(key) >= promptCacheMinBlockSize {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		out = append(out, block)
	}
	return strings.Join(out, "\n\n")
}

// promptCacheMarker 返回固定、无动态值的缓存标签。空 tag 明确表示不注入标签。
func promptCacheMarker(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.ContainsAny(tag, "\r\n") {
		return ""
	}
	return promptCacheMarkerPrefix + tag + promptCacheMarkerSuffix
}
