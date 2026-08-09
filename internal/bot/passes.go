package bot

import (
	"fmt"
	"strings"

	"github.com/zrurf/conduit"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
	"github.com/DaWesen/lanmei-dream/internal/topic"
)

// ── 上下文键 ──

const (
	KeyPlatform       = "platform"         // string 平台标识（qq/wechat/telegram/...）
	KeyPlatformUserID = "platform_user_id" // string 平台用户 ID
	KeyNickname       = "nickname"         // string 昵称
	KeyMessageID      = "message_id"       // string 消息 ID
	KeyConnID         = "conn_id"          // string 来源连接 ID
	KeySelfID         = "self_id"          // string 机器人自身 ID
	KeyIsSegment      = "bot.is_segment"   // bool 标记流式段落重入消息
	KeyStreamChannel  = "bot.stream.ch"    // chan string 流式段落通道

	// ── 事件输入（Extra，由 OnMessage 注入，只读）──
	KeyMessageType  = "bot.message_type"   // gateway.MessageType：message/notice/request
	KeySegments     = "bot.segments"       // []gateway.NormalizedSegment 完整消息段
	KeyMimeTypes    = "bot.mime_types"     // []string 去重后的 MIME 类型
	KeyAtTargets    = "bot.at_targets"     // []string at 目标 user_id 列表
	KeyEventType    = "bot.event.type"     // string 规范化事件类型（普通消息为空）
	KeyEventSubType = "bot.event.sub_type" // string 事件子类型
	KeyEventData    = "bot.event.data"     // map[string]any 事件全字段

	// ── 媒体处理中间结果（data，由 MediaPass 写入）──
	KeyImageDesc    = "bot.image_desc"    // string 图片理解描述
	KeyMediaHandled = "bot.media_handled" // bool 媒体已处理（RouterPass 路由依据）

	// ── 富文本输出（data，插件经 conduit.Set 写入，makeRichContentCallback 读取）──
	KeyRichContent = "bot.rich.content" // map[string]any{text/at/image} 富文本发送内容（无键走纯文本）

	// ── 群聊话题（TopicGatePass 写入，供对话管线消费）──
	KeyTopicID      = "bot.topic.id"      // string 命中话题 ID（data）
	KeyTopicLabel   = "bot.topic.label"   // string 话题描述（data）
	KeyTopicContext = "bot.topic.ctx"     // *llm.TopicContext 话题上下文（data）
	KeyMentionMode  = "bot.topic.mention" // topic.MentionMode 提及模式（data）
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
		Platform:       platformFromCtx(ctx),
		PlatformUserID: platformUserIDFromCtx(ctx),
		GroupID:        ctx.GroupID,
		IsGroup:        ctx.IsGroup,
		Message:        ctx.RawMsg,
		Reply:          func(s string) { replies = append(replies, s) },
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

// ── CommandRouterPass：解析斜杠命令并将命令信息写入 MessageContext ──

// CommandRouterPass 解析斜杠命令并将命令信息写入 MessageContext
type CommandRouterPass struct {
	CmdSys *command.System
}

const (
	commandNameKey    = "bot.command.name"
	commandArgsKey    = "bot.command.args"
	commandHandlerKey = "bot.command.handler"
)

func (p *CommandRouterPass) Execute(ctx *conduit.MessageContext) error {
	name := strings.TrimPrefix(ctx.RawMsg, "/")
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmdName := parts[0]
	cmd, ok := p.CmdSys.Lookup(cmdName)
	if !ok {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: fmt.Sprintf("未知命令: /%s\n输入 /帮助 查看可用命令", cmdName),
		})
		return nil
	}

	// 将命令信息写入 MessageContext
	args := parts[1:]
	conduit.Set(ctx, commandNameKey, cmdName)
	conduit.Set(ctx, commandArgsKey, args)
	conduit.Set(ctx, commandHandlerKey, cmd.Handler)

	return nil
}

// ── ExecuteCommandPass：从 MessageContext 读取命令信息并执行 ──

// ExecuteCommandPass 从 MessageContext 读取命令信息并执行
type ExecuteCommandPass struct{}

func (p *ExecuteCommandPass) Execute(ctx *conduit.MessageContext) error {
	nameRaw, _ := conduit.Get[string](ctx, commandNameKey)
	argsRaw, _ := conduit.Get[[]string](ctx, commandArgsKey)
	handlerRaw, _ := conduit.Get[func(*command.Context) error](ctx, commandHandlerKey)

	if handlerRaw == nil {
		return nil
	}

	var replies []string
	cmdCtx := &command.Context{
		Platform:       platformFromCtx(ctx),
		PlatformUserID: platformUserIDFromCtx(ctx),
		GroupID:        ctx.GroupID,
		IsGroup:        ctx.IsGroup,
		CommandName:    nameRaw,
		CommandArgs:    argsRaw,
		Message:        ctx.RawMsg,
		Reply:          func(s string) { replies = append(replies, s) },
	}

	if err := handlerRaw(cmdCtx); err != nil {
		return err
	}

	for _, r := range replies {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: r,
		})
	}

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

// IsSegment 判断消息是否为流式段落重入消息。
// 由 Bot.streamSegments 在提交子消息时通过 InputMessage.Extra 标记，
// BT 据此路由到段落交付管线。
//
// 注意：KeyIsSegment 设置在 ctx.Extra（来自 InputMessage.Extra），
// 不能用 conduit.Get（从 ctx.data 读取），必须直接读 ctx.Extra。
func IsSegment(ctx *conduit.MessageContext) bool {
	if raw, ok := ctx.Extra[KeyIsSegment]; ok {
		if b, ok := raw.(bool); ok {
			return b
		}
	}
	return false
}

// IsNotice 判断消息是否为互动事件（notice/request 类）。
// 行为树据此将事件路由到 pipeline.notice（预留节点）或插件子树。
func IsNotice(ctx *conduit.MessageContext) bool {
	mt, _ := ctx.Extra[KeyMessageType].(gateway.MessageType)
	return mt == gateway.MessageTypeNotice || mt == gateway.MessageTypeRequest
}

// IsMedia 判断消息是否含多媒体段（image/audio/video/file/record）。
// 行为树据此将消息路由到 pipeline.media 进行下载/缓存/理解。
func IsMedia(ctx *conduit.MessageContext) bool {
	segs, _ := ctx.Extra[KeySegments].([]gateway.NormalizedSegment)
	for _, s := range segs {
		switch s.Type {
		case "image", "audio", "video", "file", "record":
			return true
		}
	}
	return false
}

// ── 多模态上下文读取辅助函数（供 Pass 与插件子树使用）──

// SegmentsFromCtx 从 ctx.Extra 读取完整消息段列表。
func SegmentsFromCtx(ctx *conduit.MessageContext) []gateway.NormalizedSegment {
	segs, _ := ctx.Extra[KeySegments].([]gateway.NormalizedSegment)
	return segs
}

// MimeTypesFromCtx 从 ctx.Extra 读取去重后的 MIME 类型列表。
func MimeTypesFromCtx(ctx *conduit.MessageContext) []string {
	mimes, _ := ctx.Extra[KeyMimeTypes].([]string)
	return mimes
}

// AtTargetsFromCtx 从 ctx.Extra 读取 at 目标 user_id 列表。
func AtTargetsFromCtx(ctx *conduit.MessageContext) []string {
	ats, _ := ctx.Extra[KeyAtTargets].([]string)
	return ats
}

// MessageTypeFromCtx 从 ctx.Extra 读取事件类型（message/notice/request）。
func MessageTypeFromCtx(ctx *conduit.MessageContext) gateway.MessageType {
	mt, _ := ctx.Extra[KeyMessageType].(gateway.MessageType)
	return mt
}

// EventTypeFromCtx 从 ctx.Extra 读取规范化事件类型（普通消息为空串）。
func EventTypeFromCtx(ctx *conduit.MessageContext) string {
	et, _ := ctx.Extra[KeyEventType].(string)
	return et
}

// EventSubTypeFromCtx 从 ctx.Extra 读取事件子类型（透传原始 sub_type，可为空）。
func EventSubTypeFromCtx(ctx *conduit.MessageContext) string {
	est, _ := ctx.Extra[KeyEventSubType].(string)
	return est
}

// EventDataFromCtx 从 ctx.Extra 读取事件全字段（普通消息为 nil）。
func EventDataFromCtx(ctx *conduit.MessageContext) map[string]any {
	ed, _ := ctx.Extra[KeyEventData].(map[string]any)
	return ed
}

// ImageDescFromCtx 从 ctx.data 读取图片理解描述（由 MediaPass 写入）。
func ImageDescFromCtx(ctx *conduit.MessageContext) string {
	desc, _ := conduit.Get[string](ctx, KeyImageDesc)
	return desc
}

// TopicContextFromCtx 从 ctx.data 读取群聊话题上下文（由 TopicGatePass 写入）。
// 返回 nil 表示未命中话题（私聊或群聊 SKIP）。
func TopicContextFromCtx(ctx *conduit.MessageContext) *llm.TopicContext {
	tc, _ := conduit.Get[*llm.TopicContext](ctx, KeyTopicContext)
	return tc
}

// TopicMentionModeFromCtx 从 ctx.data 读取提及模式（MentionNone 表示未提及）。
func TopicMentionModeFromCtx(ctx *conduit.MessageContext) topic.MentionMode {
	mode, _ := conduit.Get[topic.MentionMode](ctx, KeyMentionMode)
	return mode
}

// SelfIDFromCtx 从 ctx.Extra 读取机器人自身 ID。
func SelfIDFromCtx(ctx *conduit.MessageContext) string {
	if raw, ok := ctx.Extra[KeySelfID]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

// ── 辅助函数 ──

func platformFromCtx(ctx *conduit.MessageContext) string {
	if raw, ok := ctx.Extra[KeyPlatform]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return "unknown"
}

func platformUserIDFromCtx(ctx *conduit.MessageContext) string {
	if raw, ok := ctx.Extra[KeyPlatformUserID]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ctx.UserID
}

// nicknameFromCtx 从 ctx.Extra 读取用户昵称。
// KeyNickname 由 OnMessage 通过 InputMessage.Extra 设置，存储在 ctx.Extra 中，
// 不能用 conduit.Get（从 ctx.data 读取）。
func nicknameFromCtx(ctx *conduit.MessageContext) string {
	if raw, ok := ctx.Extra[KeyNickname]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}
