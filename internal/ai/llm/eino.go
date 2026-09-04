package llm

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// DisableThinkingOption 返回关闭推理模型思考的 eino Option。
// 非流式 Chat 与流式直连路径（chatModel.Stream）共用，
// 经 OpenAI 兼容层扩展字段 thinking={"type":"disabled"} 透传（DeepSeek 等支持）。
func DisableThinkingOption() model.Option {
	return openai.WithExtraFields(map[string]any{
		"thinking": map[string]any{"type": "disabled"},
	})
}

// EinoOptions eino ChatModel 创建参数（provider 无关）
type EinoOptions struct {
	Provider    string  // Provider 名称（如 deepseek/qwen/openai），用于计费统计
	BaseURL     string  // API 基础地址（OpenAI/DeepSeek/Qwen/Moonshot/Ark/Ollama 通用）
	APIKey      string  // API 密钥
	Model       string  // 模型名
	MaxTokens   int     // 单次回复最大 token 数
	Temperature float64 // 生成温度
}

// UsageHook 用量上报回调（由计费模块注入；nil 表示不上报）。
type UsageHook func(record UsageRecord)

// EinoClient 基于 eino 的 LLMClient 实现。
// 通过 eino-ext 的 OpenAI 兼容层调用，天然支持多 provider。
type EinoClient struct {
	model     model.BaseChatModel
	provider  string
	modelName string
	usageHook UsageHook
}

// NewEinoClient 创建 eino LLM 客户端
func NewEinoClient(ctx context.Context, opts *EinoOptions) (*EinoClient, error) {
	mt := opts.MaxTokens
	t := float32(opts.Temperature)

	cm, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     opts.BaseURL,
		APIKey:      opts.APIKey,
		Model:       opts.Model,
		MaxTokens:   &mt,
		Temperature: &t,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: eino init: %w", err)
	}
	return &EinoClient{model: cm, provider: opts.Provider, modelName: opts.Model}, nil
}

// SetUsageHook 注入用量上报回调（nil 清除）。
func (c *EinoClient) SetUsageHook(hook UsageHook) { c.usageHook = hook }

// ProviderName 返回 Provider 名称
func (c *EinoClient) ProviderName() string { return c.provider }

// ModelName 返回模型名
func (c *EinoClient) ModelName() string { return c.modelName }

// reportUsage 上报一次用量记录（无 hook 或 0 token 时跳过）。
func (c *EinoClient) reportUsage(req *ChatRequest, input, output int) {
	if c.usageHook == nil || (input <= 0 && output <= 0) {
		return
	}
	c.usageHook(UsageRecord{
		Provider:     c.provider,
		Model:        c.modelName,
		Scene:        req.Scene,
		UserID:       req.UserID,
		GroupID:      req.GroupID,
		Platform:     req.Platform,
		InputTokens:  int64(input),
		OutputTokens: int64(output),
		TotalTokens:  int64(input + output),
	})
}

// usageOf 从 eino 响应中提取输入/输出 token 数。
func usageOf(resp *schema.Message) (input, output int) {
	if resp == nil || resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return 0, 0
	}
	return resp.ResponseMeta.Usage.PromptTokens, resp.ResponseMeta.Usage.CompletionTokens
}

// SupportsToolCalling 检查底层模型是否支持工具调用
func (c *EinoClient) SupportsToolCalling() bool {
	_, ok := c.model.(model.ToolCallingChatModel)
	return ok
}

// BaseModel 返回底层 eino BaseChatModel，供调用方直接调用 Stream/Generate。
// 用于流式工具调用循环中直接操作 schema.Message 列表。
func (c *EinoClient) BaseModel() model.BaseChatModel {
	return c.model
}

// ChatWithTools 返回绑定了工具的模型实例
func (c *EinoClient) ChatWithTools(tools []*schema.ToolInfo) (model.BaseChatModel, error) {
	tccm, ok := c.model.(model.ToolCallingChatModel)
	if !ok {
		return c.model, nil
	}
	return tccm.WithTools(tools)
}

// Chat 实现 LLMClient 接口
func (c *EinoClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	msgs := ToSchemaMessages(req.Messages)

	var opts []model.Option
	if req.MaxTokens != nil {
		// 按请求覆盖输出上限（推理模型对 max_tokens 敏感，短输出场景必须显式设小值）
		opts = append(opts, model.WithMaxTokens(*req.MaxTokens))
	}
	if req.DisableThinking != nil && *req.DisableThinking {
		// DeepSeek 等推理模型通过 thinking={"type":"disabled"} 关闭思考，
		// 经 eino 的 ExtraFields 透传（OpenAI 兼容扩展字段）
		opts = append(opts, DisableThinkingOption())
	}

	resp, err := c.model.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, fmt.Errorf("llm: eino generate: %w", err)
	}

	input, output := usageOf(resp)
	c.reportUsage(req, input, output)

	return &ChatResponse{
		Content:      resp.Content,
		TokensUsed:   input + output,
		InputTokens:  input,
		OutputTokens: output,
		ToolCalls:    ToToolCallPointers(resp.ToolCalls),
	}, nil
}

// StreamChat 实现 StreamingLLMClient 接口，以流式方式返回聊天补全。
func (c *EinoClient) StreamChat(ctx context.Context, req *ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	msgs := ToSchemaMessages(req.Messages)
	reader, err := c.model.Stream(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("llm: eino stream: %w", err)
	}
	return reader, nil
}

// StreamChatWithTools 绑定工具后以流式方式返回聊天补全。
func (c *EinoClient) StreamChatWithTools(ctx context.Context, req *ChatRequest, tools []*schema.ToolInfo) (*schema.StreamReader[*schema.Message], error) {
	chatModel, err := c.ChatWithTools(tools)
	if err != nil {
		return nil, fmt.Errorf("llm: bind tools for stream: %w", err)
	}
	msgs := ToSchemaMessages(req.Messages)
	reader, err := chatModel.Stream(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("llm: eino stream with tools: %w", err)
	}
	return reader, nil
}

// ChatWithModel 使用指定模型执行对话（用于工具调用流程中切换绑定了工具的模型）
func (c *EinoClient) ChatWithModel(ctx context.Context, req *ChatRequest, chatModel model.BaseChatModel) (*ChatResponse, error) {
	msgs := ToSchemaMessages(req.Messages)

	resp, err := chatModel.Generate(ctx, msgs)
	if err != nil {
		return nil, fmt.Errorf("llm: eino generate: %w", err)
	}

	input, output := usageOf(resp)
	c.reportUsage(req, input, output)

	return &ChatResponse{
		Content:      resp.Content,
		TokensUsed:   input + output,
		InputTokens:  input,
		OutputTokens: output,
		ToolCalls:    ToToolCallPointers(resp.ToolCalls),
	}, nil
}

// ToSchemaMessages 将内部 llm.Message 列表转为 eino schema.Message 列表。
// 保留 ToolCallID 以支持多轮工具调用场景；
// 带 ImageURLs 的 user 消息转为多模态（text + image_url 分段），纯文本消息保持原 Content 路径。
func ToSchemaMessages(msgs []Message) []*schema.Message {
	result := make([]*schema.Message, len(msgs))
	for i, m := range msgs {
		schemaMsg := &schema.Message{
			Role:    ToSchemaRole(m.Role),
			Content: m.Content,
		}
		if m.ToolCallID != "" {
			schemaMsg.ToolCallID = m.ToolCallID
		}
		if len(m.ImageURLs) > 0 {
			schemaMsg.UserInputMultiContent = buildMultiContent(m.Content, m.ImageURLs)
		}
		result[i] = schemaMsg
	}
	return result
}

// buildMultiContent 构造多模态分段：text 在前，随后按序追加 image_url 分段。
func buildMultiContent(text string, imageURLs []string) []schema.MessageInputPart {
	parts := make([]schema.MessageInputPart, 0, len(imageURLs)+1)
	if text != "" {
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeText,
			Text: text,
		})
	}
	for _, u := range imageURLs {
		if u == "" {
			continue
		}
		url := u
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &url},
			},
		})
	}
	return parts
}

// ToSchemaRole 将内部 Role 转为 eino schema.RoleType
func ToSchemaRole(r Role) schema.RoleType {
	switch r {
	case RoleSystem:
		return schema.System
	case RoleAssistant:
		return schema.Assistant
	case RoleTool:
		return schema.Tool
	default:
		return schema.User
	}
}

// ToToolCallPointers 将 []schema.ToolCall 转为 []*schema.ToolCall
func ToToolCallPointers(calls []schema.ToolCall) []*schema.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]*schema.ToolCall, len(calls))
	for i := range calls {
		result[i] = &calls[i]
	}
	return result
}
