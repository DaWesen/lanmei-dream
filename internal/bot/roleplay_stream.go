package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zrurf/conduit"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/model"
	"github.com/DaWesen/lanmei-dream/internal/topic"
)

// streamTimeout 流式回复的最大持续时间。
// 覆盖绝大多数 LLM 响应场景（含多轮工具调用 + 长文本生成）。
const streamTimeout = 60 * time.Second

// segmentChannelBufferSize 段落通道缓冲区大小。
// 较大的缓冲区允许 LLM 流式生成不被段落投递阻塞，
// 但段落仍按序逐条投递（由 streamSegments 的 <-done 同步点保证）。
const segmentChannelBufferSize = 32

// RoleplayStreamPass 流式角色扮演 Pass。
//
// 启动 LLM 流式生成，将段落通道存入上下文后挂起管线（yield）。
// 引擎回调检测到 yield 后，由 Bot.streamSegments 消费段落通道，
// 逐条创建子消息重入引擎，走 pipeline.roleplay_segment 交付管线。
//
// 流式 goroutine 使用独立 context（不复用 ctx.Ctx），
// 因为 yield 后引擎会调用 ctx.Cancel() 取消消息上下文。
//
// TopicMgr 非 nil 时，群聊话题命中（黑板块 KeyTopicContext）的回复完成后
// 会调用 TopicManager.RecordBotReply 记录 Bot 回复（追加窗口、授回复配额）。
type RoleplayStreamPass struct {
	Chat     *ai.ChatService
	DB       *database.DB
	Logger   *zap.Logger
	TopicMgr *topic.Manager // 可 nil：topic 系统未启用
}

// Execute 启动流式生成并挂起管线。
func (p *RoleplayStreamPass) Execute(ctx *conduit.MessageContext) error {
	userMsg := ctx.RawMsg
	if userMsg == "" {
		return nil
	}

	// 防御性检查：LLM 未配置时 chatSvc 为 nil，直接返回错误以触发 fallback
	if p.Chat == nil {
		return fmt.Errorf("roleplay: chat service not available")
	}

	platform := platformFromCtx(ctx)
	platformUserID := platformUserIDFromCtx(ctx)
	nickname := nicknameFromCtx(ctx)

	// 确保用户存在
	user, err := p.DB.GetOrCreateUser(ctx.Ctx, platform, platformUserID, nickname)
	if err != nil {
		return fmt.Errorf("roleplay: get_or_create_user: %w", err)
	}

	// 群聊话题上下文（TopicGatePass 命中话题时写入黑板；nil = 私聊/无话题）
	var topicCtx *llm.TopicContext
	var topicID, selfID string
	if p.TopicMgr != nil {
		if tc, ok := conduit.Get[*llm.TopicContext](ctx, KeyTopicContext); ok && tc != nil {
			topicCtx = tc
			topicID = tc.TopicID
		}
		selfID = SelfIDFromCtx(ctx)
	}

	// 段落通道：流式 goroutine 写入，Bot.streamSegments 读取
	segCh := make(chan string, segmentChannelBufferSize)

	// 独立 context：yield 后 ctx.Ctx 会被取消，流式必须使用独立上下文
	streamCtx, streamCancel := context.WithTimeout(context.Background(), streamTimeout)

	// 启动流式生成 goroutine
	go p.runStream(streamCtx, streamCancel, segCh, userMsg, user.ID, nickname, ctx.GroupID, ctx.UserID, platform, topicID, selfID, topicCtx)

	// 将段落通道存入上下文，供回调消费
	conduit.Set(ctx, KeyStreamChannel, segCh)

	// 挂起管线，引擎将调用 ResponseCallback
	return conduit.ErrPassYielded
}

// runStream 在独立 goroutine 中执行流式对话。
// 职责：
//  1. 调用 ChatService.ChatStream，将段落增量写入 segCh
//  2. 流结束后保存对话记录（L0 原始记录）
//  3. 群聊话题命中时记录 Bot 回复到话题（RecordBotReply）
//  4. 发生错误时发送错误提示段
//  5. defer close(segCh) 确保消费方能正常退出
func (p *RoleplayStreamPass) runStream(
	streamCtx context.Context,
	streamCancel context.CancelFunc,
	segCh chan<- string,
	userMsg string,
	userID int64,
	nickname string,
	groupID string,
	senderID string,
	platform string,
	topicID string,
	selfID string,
	topicCtx *llm.TopicContext,
) {
	defer streamCancel()
	defer close(segCh)

	req := &llm.ChatRequest{
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		UserID:         userID,
		UserName:       nickname,
		GroupName:      groupID,
		GroupID:        groupID,
		PlatformUserID: senderID, // 平台用户 ID（conduit ctx.UserID），供工具调用身份注入
		TopicContext:   topicCtx,
	}

	// 记录流式生成耗时与内容长度，便于排查"决策回复但无消息"类问题
	start := time.Now()
	resp, err := p.Chat.ChatStream(streamCtx, req, segCh)
	p.Logger.Info("roleplay: 流式生成结束",
		zap.Duration("elapsed", time.Since(start)),
		zap.String("user", senderID),
		zap.Int("content_len", respContentLen(resp)),
		zap.Error(err))

	if err != nil {
		p.Logger.Error("roleplay stream: chat stream failed", zap.Error(err))
		// 发送错误提示段，让用户知道出了问题。
		// 使用独立 context 而非 streamCtx（后者可能已超时），
		// 确保 timeout 场景下用户也能收到错误提示。
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		select {
		case segCh <- "蓝妹现在有点迷糊，稍后再试~":
		case <-sendCtx.Done():
			p.Logger.Warn("roleplay stream: send error segment timed out")
		}
		sendCancel()
		return
	}

	// LLM 空响应防护：流式无任何 token 时 segCh 无段落，
	// 表现为"决策了回复但用户侧静默"。重试一次；仍空则发提示段。
	// 注意：assembleContext 会原地修改 req.Messages，重试必须用新请求。
	if strings.TrimSpace(resp.Content) == "" {
		p.Logger.Warn("roleplay: LLM 返回空响应，重试一次", zap.String("user", senderID))
		// 空响应多为推理模型思考耗尽输出预算，重试时关闭思考（与 stream 层超限重试同思路）
		retryDisableThinking := true
		retryReq := &llm.ChatRequest{
			Messages:         []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
			UserID:           userID,
			UserName:         nickname,
			GroupName:        groupID,
			GroupID:          groupID,
			PlatformUserID:   senderID,
			TopicContext:     topicCtx,
			DisableThinking:  &retryDisableThinking,
		}
		retryCtx, retryCancel := context.WithTimeout(context.Background(), 30*time.Second)
		retryResp, retryErr := p.Chat.ChatStream(retryCtx, retryReq, segCh)
		retryCancel()
		if retryErr == nil && strings.TrimSpace(retryResp.Content) != "" {
			resp = retryResp
			p.Logger.Info("roleplay: 空响应重试成功", zap.String("user", senderID), zap.Int("content_len", respContentLen(resp)))
		} else {
			p.Logger.Error("roleplay: LLM 空响应（重试后仍为空）", zap.String("user", senderID), zap.Error(retryErr))
			sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
			select {
			case segCh <- "刚才没听清，能再说一次吗~":
			case <-sendCtx.Done():
			}
			sendCancel()
			return
		}
	}

	// 表情提示词计数器：统计 Bot 在本群/私聊中发出的自然语言（LLM 触发）回复数。
	// 本轮回复实际调用了 pick_sticker（回复携带表情）→ 清零重新计数，否则累加。
	if p.Chat != nil {
		scope := replyScopeFor(groupID, senderID)
		sentSticker := false
		for _, t := range resp.InvolvedTools {
			if t == "pick_sticker" {
				sentSticker = true
				break
			}
		}
		if sentSticker {
			p.Chat.ResetReplyCount(scope)
		} else {
			p.Chat.IncReplyCount(scope)
		}
	}

	// 保存对话记录（L0 原始记录，后续由 Compressor 自动压缩）
	// 使用 context.Background() 因为 streamCtx 可能已接近超时
	bgCtx := context.Background()
	if saveErr := p.DB.SaveConversation(bgCtx, userID, groupID, "user", userMsg, model.SourceChat, ""); saveErr != nil {
		p.Logger.Error("roleplay stream: save user conversation", zap.Error(saveErr))
	}
	// 根据是否调用了工具决定 assistant 消息的来源标记
	source := model.SourceChat
	pluginTag := ""
	if len(resp.InvolvedTools) > 0 {
		source = model.SourcePlugin
		pluginTag = resp.InvolvedTools[0] // 取首个工具名作为标签
	}
	// 空回复不落库（LLM 空响应场景），否则空内容消息会污染对话历史，
	// 后续组装请求时被 API 以 "missing field content" 拒绝。
	if strings.TrimSpace(resp.Content) != "" {
		if saveErr := p.DB.SaveConversation(bgCtx, userID, groupID, "assistant", resp.Content, source, pluginTag); saveErr != nil {
			p.Logger.Error("roleplay stream: save assistant conversation", zap.Error(saveErr))
		}
	}

	// 群聊话题命中：记录 Bot 回复（追加窗口、授回复配额，保持话题活跃）
	if p.TopicMgr != nil && topicID != "" && resp.Content != "" {
		p.TopicMgr.RecordBotReply(bgCtx, platform, groupID, topicID, selfID, senderID, resp.Content)
	}
}

// respContentLen 返回 ChatResponse 内容长度（nil 安全），用于流式完成日志。
func respContentLen(resp *llm.ChatResponse) int {
	if resp == nil {
		return 0
	}
	return utf8.RuneCountInString(resp.Content)
}

// replyScopeFor 计算表情计数的作用域：群聊用 groupID，私聊用 "dm:"+平台用户ID。
// 与 ai 包内 assembleContext 注入提示词时的作用域保持一致。
func replyScopeFor(groupID, platformUserID string) string {
	if groupID != "" {
		return groupID
	}
	return "dm:" + platformUserID
}

// ── RoleplaySegmentPass：流式段落交付 ──

// RoleplaySegmentPass 将流式段落追加到输出，由引擎回调发送。
//
// 每个段落作为子消息重入引擎，走此管线。
// 段落原文存储在 ctx.RawMsg 中（由 NewChildInput 设置）。
type RoleplaySegmentPass struct{}

// Execute 将段落追加到输出队列。
func (p *RoleplaySegmentPass) Execute(ctx *conduit.MessageContext) error {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID:  ctx.UserID,
		GroupID: ctx.GroupID,
		Content: ctx.RawMsg,
		IsGroup: ctx.IsGroup,
	})
	return nil
}
