package topic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/memory"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/model"
)

// archiveMaxDialogueRunes 归档摘要输入的对话文本长度上限（rune），防 LLM 成本失控。
const archiveMaxDialogueRunes = 4000

// archiveMaxEmbedRunes 归档向量化文本长度上限（rune）。
const archiveMaxEmbedRunes = 400

// Archiver 冷却话题归档器：将话题窗口沉淀为群级长期记忆。
//
// 归档内容（与个人 L1 压缩同构的 brief/detailed/facts 结构）：
//   - memories 表：{user_id:0(群级), group_id, metadata:{topic_id,label,members}}
//   - memory_vectors 表：窗口摘要文本的 embedding，供后续 RAG 群聊召回
//
// 降级链：无 LLM → brief=标签, detailed=原文拼接, facts=成员列表；
// 无 Embedder → 仅写 memories 元数据表，跳过向量化。
type Archiver struct {
	llmClient llm.LLMClient
	embedder  embedding.Embedder
	memStore  memory.MemoryStore
	db        *database.DB
	logger    *zap.Logger
}

// NewArchiver 创建归档器（各依赖可 nil，均自动降级）。
func NewArchiver(llmClient llm.LLMClient, emb embedding.Embedder, mem memory.MemoryStore,
	db *database.DB, logger *zap.Logger) *Archiver {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Archiver{llmClient: llmClient, embedder: emb, memStore: mem, db: db, logger: logger}
}

// ArchiveSnapshot 归档输入快照：由 Manager 在持锁状态下冻结后传入，
// 归档全程只读快照，与话题被重入时的并发修改完全隔离（无数据竞争）。
type ArchiveSnapshot struct {
	ID       string     // 话题 ID
	Platform string     // 来源平台
	GroupID  string     // 群 ID
	Label    string     // 话题标签（可能为空，归档时展示名兜底为默认值）
	Window   []TopicMsg // 消息窗口
	Members  []string   // 成员昵称列表
}

// Archive 归档一个冷却话题。返回 error 时由 Manager 保留话题并重试。
// 空窗口话题返回 nil（无内容可沉淀）。
func (a *Archiver) Archive(ctx context.Context, snap *ArchiveSnapshot) error {
	if snap == nil {
		return errors.New("topic archive: nil snapshot")
	}
	if len(snap.Window) == 0 {
		return nil
	}

	// 1. 生成摘要（LLM 或降级）
	brief, detailed, facts := a.summarize(ctx, snap)
	label := snap.Label
	if label == "" {
		label = defaultTopicLabel
	}
	if brief != "" {
		label = brief
	}
	content := label + "：" + brief
	timeRange := fmt.Sprintf("%s ~ %s", snap.Window[0].SentAt.Format("01-02 15:04"), snap.Window[len(snap.Window)-1].SentAt.Format("01-02 15:04"))

	// 2. 写 memories 表（群级 user_id=0）
	if a.db != nil {
		meta, _ := json.Marshal(map[string]any{
			"topic_id":      snap.ID,
			"label":         label,
			"members":       snap.Members,
			"facts":         facts,
			"message_count": len(snap.Window),
			"time_range":    timeRange,
		})
		if err := a.db.SaveGroupMemory(ctx, &model.Memory{
			UserID:   0, // 群级记忆
			GroupID:  snap.GroupID,
			Content:  content,
			Metadata: meta,
		}); err != nil {
			a.logger.Warn("topic: 归档写 memories 失败", zap.String("topic", snap.ID), zap.Error(err))
		}
	}

	// 3. 写 memory_vectors（向量化，供群聊 RAG 召回）
	if a.memStore != nil && a.embedder != nil {
		vec, err := a.embedder.Embed(ctx, truncateRunes(brief+" "+detailed, archiveMaxEmbedRunes))
		if err == nil && len(vec) > 0 {
			if err := a.memStore.Store(ctx, &memory.Memory{
				UserID:  0, // 群级记忆
				GroupID: snap.GroupID,
				Content: content,
				Vector:  vec,
				Metadata: map[string]any{
					"topic_id": snap.ID,
					"members":  snap.Members,
				},
			}); err != nil {
				a.logger.Warn("topic: 归档写 memory_vectors 失败", zap.String("topic", snap.ID), zap.Error(err))
			}
		} else {
			a.logger.Warn("topic: 归档向量化失败，跳过向量记忆", zap.String("topic", snap.ID), zap.Error(err))
		}
	}

	// 4. 群画像：归档事实三态合并进 group_facts（跨话题沉淀群级长期记忆）
	if a.db != nil && len(facts) > 0 {
		for i := range facts {
			facts[i].Evidence = append(facts[i].Evidence, snap.ID)
		}
		if err := a.db.MergeGroupFacts(ctx, snap.GroupID, facts); err != nil {
			a.logger.Warn("topic: 归档合并群画像失败", zap.String("topic", snap.ID), zap.Error(err))
		}
	}

	a.logger.Info("topic: 话题已归档", zap.String("topic", snap.ID), zap.Int("msgs", len(snap.Window)), zap.String("label", label))
	return nil
}

// summarize 通过 LLM 生成 brief/detailed/facts（facts 为带置信度的结构化事实）；
// 无 LLM 或调用失败时降级（facts 返回 nil，不写群画像）。
func (a *Archiver) summarize(ctx context.Context, snap *ArchiveSnapshot) (brief, detailed string, facts []model.FactItem) {
	dialogue := truncateRunes(formatWindow(snap.Window), archiveMaxDialogueRunes)
	defaultBrief := snap.Label
	if defaultBrief == "" {
		defaultBrief = defaultTopicLabel
	}

	if a.llmClient == nil {
		return defaultBrief, dialogue, nil
	}
	resp, err := a.llmClient.Chat(ctx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: groupArchiveSystemPrompt},
			{Role: llm.RoleUser, Content: dialogue},
		},
	})
	if err != nil || resp == nil {
		return defaultBrief, dialogue, nil
	}
	var res archiveResult
	if err := json.Unmarshal([]byte(resp.Content), &res); err != nil {
		return defaultBrief, truncateRunes(resp.Content, 300), nil
	}
	facts = model.ParseFacts(res.Facts) // 兼容 []FactItem 与旧 []string
	if strings.TrimSpace(res.Brief) == "" {
		return defaultBrief, res.Detailed, facts
	}
	return res.Brief, res.Detailed, facts
}

// archiveResult LLM 归档输出结构。
type archiveResult struct {
	Brief    string          `json:"brief"`
	Detailed string          `json:"detailed"`
	Facts    json.RawMessage `json:"facts"` // []FactItem（带置信度）；兼容旧 []string
}

// groupArchiveSystemPrompt 群聊话题归档 prompt（与个人 L1 压缩同构，输出严格 JSON）。
const groupArchiveSystemPrompt = `你是一个群聊话题记忆归档引擎。阅读一段群聊话题的对话记录，生成压缩后的记忆。

输出格式（严格 JSON）：
{
  "brief": "一句话总结这个话题的核心内容（不超过50字）",
  "detailed": "详细摘要，保留关键事实、决策、参与者观点（不超过300字）",
  "facts": [{"key": "周末活动", "value": "张三(10001)提议周末去爬山", "confidence": 0.85}]
}

facts 规则：
- 只提取客观事实，不提取寒暄/闲聊
- key：命题主题（2-6字，细粒度——同一 key 应只有一种取值，如"周末活动"、"餐厅推荐"）
- value：每条事实不超过20字，格式如"张三(10001)提议周末去爬山"、"李四(10002)推荐了某家餐厅"
- 发言者必须保留括号内的用户ID（稳定身份锚点），即使昵称后来改了也能对应到同一人
- confidence：0~1，表示该事实在本次对话中的确信度——明确陈述/重复提及 → 0.8~0.95；仅一次 → 0.6~0.8；间接 → 0.4~0.6
- 每条事实都必须给 key、value、confidence，禁止省略或编造

注意：只输出 JSON，不要任何额外文字。`

// formatWindow 将话题消息窗口格式化为对话文本（昵称/机器人交替行）。
// 供归档摘要与话题标签生成共用。用户消息以「昵称(用户ID)」标注发言者：
// 用户ID 是稳定身份锚点（群昵称常变，只留昵称会让归档记忆"认不出"同一人），
// 缺失昵称时退化为 user_id，避免匿名 user_id 导致记忆串线（历史教训）。
func formatWindow(window []TopicMsg) string {
	var b strings.Builder
	for i, tm := range window {
		if i >= 100 { // 防御上限：最多格式前 100 条
			break
		}
		who := "用户"
		if tm.IsBot {
			who = "机器人"
		} else if tm.Nickname != "" || tm.UserID != "" {
			who = SpeakerLabel(tm.Nickname, tm.UserID)
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(who)
		b.WriteString(": ")
		b.WriteString(tm.Content)
	}
	return b.String()
}
