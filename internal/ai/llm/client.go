package llm

import "context"

// Role 表示对话消息的角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 表示一条对话消息
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 是一次对话请求的入参
type ChatRequest struct {
	Messages []Message `json:"messages"`
	UserID   int64     `json:"user_id"`
}

// ChatResponse 是对话服务的返回
type ChatResponse struct {
	Content    string `json:"content"`
	TokensUsed int    `json:"tokens_used"`
}

// LLMClient 抽象大语言模型的对话能力。
// 具体实现（OpenAI / 本地模型等）由外部注入。
type LLMClient interface {
	// Chat 执行一次对话补全
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}
