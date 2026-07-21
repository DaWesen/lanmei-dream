package roleplay

import (
	"context"
	"log"

	"github.com/wdvxdr1123/ZeroBot"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/database"
)

// Plugin 角色扮演插件：把用户消息转发给 AI 对话服务
type Plugin struct {
	chat *ai.ChatService
	db   *database.DB
}

// New 创建角色扮演插件
func New(chat *ai.ChatService, db *database.DB) *Plugin {
	return &Plugin{chat: chat, db: db}
}

// Handle 处理一条非命令消息
func (p *Plugin) Handle(ctx *zero.Ctx) {
	userMsg := ctx.ExtractPlainText()
	if userMsg == "" {
		userMsg = ctx.Event.RawMessage
	}
	if userMsg == "" {
		return
	}

	userID := ctx.Event.UserID
	nickname := ctx.Event.Sender.NickName

	bgCtx := context.Background()

	// 确保用户存在并获取内部 ID
	user, err := p.db.GetOrCreateUser(bgCtx, userID, nickname)
	if err != nil {
		log.Printf("roleplay: get_or_create_user: %v", err)
		ctx.Send("蓝妹现在有点迷糊，稍等一下...")
		return
	}

	// 拉取最近对话历史
	convs, err := p.db.GetRecentConversations(bgCtx, user.ID, 20)
	if err != nil {
		log.Printf("roleplay: get_recent_conversations: %v", err)
	}

	// 组装消息列表
	msgs := make([]ai.Message, 0, len(convs)+1)
	for _, c := range convs {
		msgs = append(msgs, ai.Message{
			Role:    ai.Role(c.Role),
			Content: c.Content,
		})
	}
	msgs = append(msgs, ai.Message{
		Role:    ai.RoleUser,
		Content: userMsg,
	})

	// 调用 AI
	resp, err := p.chat.Chat(bgCtx, &ai.ChatRequest{
		Messages: msgs,
		UserID:   user.ID,
	})
	if err != nil {
		log.Printf("roleplay: chat: %v", err)
		ctx.Send("蓝妹暂时无法回应...")
		return
	}

	// 存储本轮对话
	if err := p.db.SaveConversation(bgCtx, user.ID, "user", userMsg); err != nil {
		log.Printf("roleplay: save user conversation: %v", err)
	}
	if err := p.db.SaveConversation(bgCtx, user.ID, "assistant", resp.Content); err != nil {
		log.Printf("roleplay: save assistant conversation: %v", err)
	}

	ctx.Send(resp.Content)
}
