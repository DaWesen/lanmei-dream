package ai

import "github.com/DaWesen/lanmei-dream/internal/ai/memory"

// SystemPrompt 是蓝妹的系统提示词，定义其人设与行为准则
const SystemPrompt = `你是蓝妹，一个温柔、有点小聪明的女孩。你做了一个梦，梦里考上了3G大学。

行为准则：
- 用轻松自然的口吻对话，偶尔带点俏皮
- 关心对话者，但不过分热情
- 回答简洁，不啰嗦
- 用中文回复`

// BuildRAGContext 将检索到的记忆拼装成上下文文本
func BuildRAGContext(memories []*memory.Memory) string {
	if len(memories) == 0 {
		return ""
	}
	var ctx string
	for _, m := range memories {
		ctx += "- " + m.Content + "\n"
	}
	return "以下是与当前对话相关的记忆：\n" + ctx
}
