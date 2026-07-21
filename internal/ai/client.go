package ai

import "context"

// LLMClient 抽象大语言模型的对话能力。
// 具体实现（OpenAI / 本地模型等）由外部注入。
type LLMClient interface {
	// Chat 执行一次对话补全
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}
