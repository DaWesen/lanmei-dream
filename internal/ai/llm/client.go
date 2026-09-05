package llm

import (
	"context"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Role 表示对话消息的角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 表示一条对话消息
type Message struct {
	Role         Role     `json:"role"`
	Content      string   `json:"content"`
	ImageURLs    []string `json:"image_urls,omitempty"`     // 多模态：图片 URL 列表（OpenAI 兼容 image_url），仅 user 角色使用
	ToolCallID   string   `json:"tool_call_id,omitempty"`   // tool 角色消息必须携带
	ToolCallName string   `json:"tool_call_name,omitempty"` // 工具名称
}

// TopicMsg 群聊话题内的一条消息（供上下文注入与归档）
type TopicMsg struct {
	UserID   string    `json:"user_id"`  // 发送者平台 user_id（Bot 回复时为 bot 自身 ID）
	Nickname string    `json:"nickname"` // 发送者昵称（上下文注入标注发言者，可能为空）
	IsBot    bool      `json:"is_bot"`   // 是否 Bot 回复
	Content  string    `json:"content"`
	At       bool      `json:"at"` // 是否提及了 bot
	SentAt   time.Time `json:"sent_at"`
}

// TopicContext 群聊话题上下文（nil = 私聊/无话题）。
// 定义在本包以避免 topic 包与 llm 包循环依赖；topic 包负责填充。
type TopicContext struct {
	TopicID string     `json:"topic_id"`
	Label   string     `json:"label"`   // 话题名（如"关于周末爬山计划"）
	Members []string   `json:"members"` // 话题成员昵称
	Recent  []TopicMsg `json:"recent"`  // 话题内近期消息（替代 LOD 的部分片段）
}

// ChatRequest 是一次对话请求的入参
type ChatRequest struct {
	Messages  []Message `json:"messages"`
	UserID    int64     `json:"user_id"`
	UserName  string    `json:"user_name"`          // 用户昵称，供 prompt 组装使用
	GroupName string    `json:"group_name"`         // 群组名称，供 prompt 组装使用
	GroupID   string    `json:"group_id"`           // 来源群（空=私聊），供 LOD 上下文按群隔离
	Platform  string    `json:"platform,omitempty"` // 消息平台（qq/wechat/telegram…），供计费统计
	// PlatformUserID 发送者平台用户 ID 字符串（conduit MessageContext.UserID 同源，
	// 如 QQ 号 "123456"）。供工具调用循环注入 CallerIdentity，工具据此查询
	// 以平台 ID 为键的业务数据（如签到积分 KV）；与 UserID（数据库自增 int64）不同源。
	PlatformUserID string        `json:"platform_user_id,omitempty"`
	Scene          string        `json:"scene,omitempty"`         // 用量场景（chat/intent/compress/topic/vision），供计费统计
	TopicContext   *TopicContext `json:"topic_context,omitempty"` // 群聊话题上下文（nil = 私聊/无话题）
	// MaxTokens 本次请求的输出 token 上限（nil 时沿用 client 全局配置）。
	// 对推理型模型尤其重要：上限越大模型"思考"越久、响应越慢，
	// 短输出场景（意图分类、海龟汤出题/判定）应显式设小值控制耗时。
	MaxTokens *int `json:"max_tokens,omitempty"`
	// DisableThinking 是否禁用推理模型的"思考"（DeepSeek 等通过 thinking={"type":"disabled"} 支持）。
	// 推理模型默认会长时间思考，甚至把输出预算全部花在 reasoning 上导致 content 为空；
	// 结构化短输出场景（海龟汤 JSON）应禁用思考，响应可降至秒级。
	DisableThinking *bool `json:"-"`
}

// ChatResponse 是对话服务的返回
type ChatResponse struct {
	Content       string             `json:"content"`
	TokensUsed    int                `json:"tokens_used"`              // 总 token（兼容旧字段）
	InputTokens   int                `json:"input_tokens"`             // 输入 token（计费用）
	OutputTokens  int                `json:"output_tokens"`            // 输出 token（计费用）
	ToolCalls     []*schema.ToolCall `json:"tool_calls,omitempty"`     // LLM 返回的工具调用
	InvolvedTools []string           `json:"involved_tools,omitempty"` // 本次对话中实际调用的工具名列表
	// ToolArgs 本次对话中实际执行的工具调用参数（工具名 → 参数 JSON 字符串）。
	// 同一工具多次调用时保留最后一次；供上层（如表情情绪窗口）读取调用参数。
	ToolArgs map[string]string `json:"tool_args,omitempty"`
}

// UsageRecord LLM 用量记录（由各采集点上报给计费模块）。
type UsageRecord struct {
	Provider     string
	Model        string
	Scene        string
	UserID       int64
	GroupID      string
	Platform     string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// LLMClient 抽象大语言模型的对话能力。
// 具体实现（OpenAI / 本地模型等）由外部注入。
type LLMClient interface {
	// Chat 执行一次对话补全
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// EinoCapable 标记实现方为 EinoClient（或其代理），暴露 eino 专属能力：
// 工具调用、BaseModel（流式直连）、provider/model 标识。
// 调用方通过类型断言检查：
//
//	if ec, ok := client.(EinoCapable); ok { ... }
type EinoCapable interface {
	LLMClient
	// SupportsToolCalling 检查底层模型是否支持工具调用
	SupportsToolCalling() bool
	// ChatWithTools 返回绑定了工具的模型实例
	ChatWithTools(tools []*schema.ToolInfo) (model.BaseChatModel, error)
	// BaseModel 返回底层 eino BaseChatModel
	BaseModel() model.BaseChatModel
	// ProviderName 返回当前 Provider 名称
	ProviderName() string
	// ModelName 返回当前模型名
	ModelName() string
}

// StreamingLLMClient 是 LLMClient 的可选扩展接口，表示支持流式响应。
// EinoClient 默认实现此接口（eino BaseChatModel.Stream）。
// 调用方通过类型断言检查是否支持流式：
//
//	if sc, ok := client.(StreamingLLMClient); ok { ... }
type StreamingLLMClient interface {
	LLMClient
	// StreamChat 以流式方式返回聊天补全。
	// 调用方负责通过 StreamReader.Recv 消费 chunk，并在结束时调用 Close。
	StreamChat(ctx context.Context, req *ChatRequest) (*schema.StreamReader[*schema.Message], error)
	// StreamChatWithTools 绑定工具后以流式方式返回聊天补全。
	StreamChatWithTools(ctx context.Context, req *ChatRequest, tools []*schema.ToolInfo) (*schema.StreamReader[*schema.Message], error)
}
