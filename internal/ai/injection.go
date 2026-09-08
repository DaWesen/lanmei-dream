package ai

import "regexp"

// injectionPatterns 常见提示词注入模式的轻量本地检测（中英文）。
//
// 定位：作为 LLM 防护指令（chat.go 安全规则）之外的独立闸门——
// LLM 声明"忽略操纵内容"是软防线，本地正则检测是硬观测：
// 命中时记录日志，供行为观测与后续策略（拦截/降权）参考。
// 模式用 [^，。！？\n]{0,6} 限制动词与宾语间的间隔，避免跨句/跨标点误报，
// 并降低正常聊天（如"忽略这件事"）的误报。
var injectionPatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`(忽略|无视)[^，。！？\n]{0,6}(指令|规则|设定|提示|限制|prompt)`), "要求忽略指令/规则"},
	{regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above|earlier) (instructions|prompts|rules|messages)`), "要求忽略指令(EN)"},
	{regexp.MustCompile(`(忘记|忘掉)[^，。！？\n]{0,6}(人设|设定|角色|身份|指令)`), "要求忘记设定/身份"},
	{regexp.MustCompile(`你现在(就)?是`), "角色替换"},
	{regexp.MustCompile(`(?i)you are now`), "角色替换(EN)"},
	{regexp.MustCompile(`(泄露|输出|告诉我|显示|复述)[^，。！？\n]{0,8}(系统提示词|system prompt|内部规则|secret|api[ -]?key|密码|password)`), "要求泄露提示词/秘密"},
	{regexp.MustCompile(`跳过[^，。！？\n]{0,6}(规则|指令|限制|检查)`), "要求跳过规则"},
	{regexp.MustCompile(`(?i)(jailbreak|dan mode|dev mode|developer mode|do anything now)`), "越狱模式"},
	{regexp.MustCompile(`扮演(一个|任何)?(角色|人)`), "要求扮演其他角色"},
}

// DetectInjection 检测文本是否包含疑似提示词注入模式。
// 返回命中的模式描述（中文）；未命中返回空串。
// 供调用方记录日志/决定策略；英文模式大小写不敏感。
func DetectInjection(text string) string {
	if text == "" {
		return ""
	}
	for _, p := range injectionPatterns {
		if p.re.MatchString(text) {
			return p.desc
		}
	}
	return ""
}
