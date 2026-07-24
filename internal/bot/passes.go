package bot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zrurf/conduit"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/database"
)

// ── 上下文键 ──

const (
	KeyQQUserID = "qq_user_id" // int64 QQ 用户 ID（存在 Extra 中）
	KeyNickname = "nickname"   // string 昵称（存在 Extra 中）
)

// ── CommandPass：处理斜杠命令 ──

// CommandPass 把 command.System 包装成 Conduit Pass
type CommandPass struct {
	CmdSys *command.System
}

func (p *CommandPass) Execute(ctx *conduit.MessageContext) error {
	// 收集命令回复
	var replies []string
	err := p.CmdSys.Process(ctx.RawMsg, &command.Context{
		UserID:  qqUserID(ctx),
		Message: ctx.RawMsg,
		Reply:   func(s string) { replies = append(replies, s) },
	})

	// 将回复追加到输出
	for _, r := range replies {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID:  ctx.UserID,
			GroupID: ctx.GroupID,
			Content: r,
			IsGroup: ctx.IsGroup,
		})
	}

	return err
}

// ── RoleplayPass：AI 角色扮演对话 ──

// RoleplayPass 调用 AI 对话服务
type RoleplayPass struct {
	Chat *ai.ChatService
	DB   *database.DB
}

func (p *RoleplayPass) Execute(ctx *conduit.MessageContext) error {
	userMsg := ctx.RawMsg
	if userMsg == "" {
		return nil
	}

	qqID := qqUserID(ctx)
	nickname, _ := conduit.Get[string](ctx, KeyNickname)

	// 确保用户存在
	user, err := p.DB.GetOrCreateUser(ctx.Ctx, qqID, nickname)
	if err != nil {
		return fmt.Errorf("roleplay: get_or_create_user: %w", err)
	}

	// ChatService 内部通过 LOD 组装上下文（L2→L1→L0 + RAG）
	// 这里只传当前用户消息，不再手动拉对话历史
	resp, err := p.Chat.Chat(ctx.Ctx, &llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		UserID:   user.ID,
	})
	if err != nil {
		return fmt.Errorf("roleplay: chat: %w", err)
	}

	// 存储本轮对话（L0 原始记录，后续由 Compressor 自动压缩）
	if err := p.DB.SaveConversation(ctx.Ctx, user.ID, "user", userMsg); err != nil {
		return conduit.NewSoftError(fmt.Errorf("roleplay: save user conversation: %w", err))
	}
	if err := p.DB.SaveConversation(ctx.Ctx, user.ID, "assistant", resp.Content); err != nil {
		return conduit.NewSoftError(fmt.Errorf("roleplay: save assistant conversation: %w", err))
	}

	// 输出
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID:  ctx.UserID,
		GroupID: ctx.GroupID,
		Content: resp.Content,
		IsGroup: ctx.IsGroup,
	})

	return nil
}

// ── FallbackPass：兜底回复 ──

// FallbackPass 超时或未匹配时的兜底
type FallbackPass struct{}

func (p *FallbackPass) Execute(ctx *conduit.MessageContext) error {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID:  ctx.UserID,
		GroupID: ctx.GroupID,
		Content: "蓝妹现在有点迷糊，稍等一下...",
		IsGroup: ctx.IsGroup,
	})
	return nil
}

// ── 条件判断函数 ──

// IsCommand 判断消息是否以 / 开头
func IsCommand(ctx *conduit.MessageContext) bool {
	return strings.HasPrefix(ctx.RawMsg, "/")
}

// IsAdminCommand 判断消息是否以 /admin 开头
func IsAdminCommand(ctx *conduit.MessageContext) bool {
	return strings.HasPrefix(ctx.RawMsg, "/admin")
}

// ── 辅助函数 ──

func qqUserID(ctx *conduit.MessageContext) int64 {
	// 优先从 Extra 取 int64 类型的 QQ ID
	if raw, ok := ctx.Extra[KeyQQUserID]; ok {
		if id, ok := raw.(int64); ok {
			return id
		}
	}
	// fallback：从 UserID 字符串解析
	id, err := strconv.ParseInt(ctx.UserID, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
