package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/memory"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/model"
)

// Compressor 使用 LLM 对记忆进行 LOD 压缩
type Compressor struct {
	llm      llm.LLMClient
	embedder embedding.Embedder
	memStore memory.MemoryStore
	db       *database.DB
}

// NewCompressor 创建压缩器
func NewCompressor(l llm.LLMClient, emb embedding.Embedder, mem memory.MemoryStore, db *database.DB) *Compressor {
	return &Compressor{llm: l, embedder: emb, memStore: mem, db: db}
}

// MaybeCompress 检查并触发压缩（L0→L1 和 L1→L2）
// 在每次对话后异步调用
func (c *Compressor) MaybeCompress(ctx context.Context, userID int64) {
	// L0→L1：原始对话超过阈值时压缩
	if err := c.compressL0ToL1(ctx, userID); err != nil {
		log.Printf("compressor: L0→L1: %v", err)
	}

	// L1→L2：episode 摘要超过阈值时聚合
	if err := c.compressL1ToL2(ctx, userID); err != nil {
		log.Printf("compressor: L1→L2: %v", err)
	}
}

// ─── L0→L1: 原始对话 → Episode Summary ───

func (c *Compressor) compressL0ToL1(ctx context.Context, userID int64) error {
	const (
		threshold = 40 // 原始对话超过此数触发压缩
		batchSize = 20 // 每次压缩的条数
	)

	count, err := c.db.CountConversations(ctx, userID)
	if err != nil {
		return fmt.Errorf("count conversations: %w", err)
	}
	if count < threshold {
		return nil
	}

	// 取最老的 N 条对话
	convs, err := c.db.GetOldestConversations(ctx, userID, batchSize)
	if err != nil || len(convs) == 0 {
		return fmt.Errorf("get oldest conversations: %w", err)
	}

	// 构造压缩 prompt
	dialogue := formatConversations(convs)
	prompt := buildCompressPrompt(dialogue)

	resp, err := c.llm.Chat(ctx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: compressSystemPrompt},
			{Role: llm.RoleUser, Content: prompt},
		},
	})
	if err != nil {
		return fmt.Errorf("llm compress: %w", err)
	}

	// 解析 LLM 输出
	var result compressResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		// LLM 输出不是合法 JSON，做 fallback：整段当 detailed
		result = compressResult{
			Brief:    truncate(resp.Content, 100),
			Detailed: resp.Content,
			Facts:    []string{},
		}
	}

	factsJSON, _ := json.Marshal(result.Facts)

	episode := &model.EpisodeSummary{
		UserID:       userID,
		Brief:        result.Brief,
		Detailed:     result.Detailed,
		Facts:        factsJSON,
		CoveredCount: len(convs),
		FirstConvoID: convs[0].ID,
		LastConvoID:  convs[len(convs)-1].ID,
	}

	// 先存摘要
	if err := c.db.SaveEpisodeSummary(ctx, episode); err != nil {
		return fmt.Errorf("save episode: %w", err)
	}

	// 再删原文
	if err := c.db.DeleteConversationsInRange(ctx, userID, episode.FirstConvoID, episode.LastConvoID); err != nil {
		log.Printf("compressor: delete compressed conversations: %v", err)
	}

	log.Printf("compressor: L0→L1 压缩完成, user=%d, %d条→1条摘要", userID, len(convs))
	return nil
}

// ─── L1→L2: Episode Summaries → Topic Cluster ───

func (c *Compressor) compressL1ToL2(ctx context.Context, userID int64) error {
	const (
		threshold = 10 // episode 超过此数触发聚合
		batchSize = 5  // 每次聚合的条数
	)

	count, err := c.db.CountEpisodes(ctx, userID)
	if err != nil {
		return fmt.Errorf("count episodes: %w", err)
	}
	if count < threshold {
		return nil
	}

	episodes, err := c.db.GetOldestEpisodes(ctx, userID, batchSize)
	if err != nil || len(episodes) == 0 {
		return fmt.Errorf("get oldest episodes: %w", err)
	}

	// 拼接 episode 内容
	var episodeTexts string
	for i, e := range episodes {
		episodeTexts += fmt.Sprintf("【片段%d】\n摘要: %s\n详细: %s\n\n", i+1, e.Brief, e.Detailed)
	}

	prompt := buildClusterPrompt(episodeTexts)

	resp, err := c.llm.Chat(ctx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: clusterSystemPrompt},
			{Role: llm.RoleUser, Content: prompt},
		},
	})
	if err != nil {
		return fmt.Errorf("llm cluster: %w", err)
	}

	var result clusterResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		result = clusterResult{
			Topic:    "综合话题",
			Brief:    truncate(resp.Content, 100),
			Detailed: resp.Content,
			Facts:    []string{},
		}
	}

	factsJSON, _ := json.Marshal(result.Facts)

	topic := &model.TopicCluster{
		UserID:       userID,
		Topic:        result.Topic,
		Brief:        result.Brief,
		Detailed:     result.Detailed,
		Facts:        factsJSON,
		CoveredCount: len(episodes),
	}

	// 存主题
	if err := c.db.SaveTopicCluster(ctx, topic); err != nil {
		return fmt.Errorf("save topic: %w", err)
	}

	// 向量化后存入 Milvus（用于语义检索）
	if c.embedder != nil && c.memStore != nil {
		vec, err := c.embedder.Embed(ctx, result.Brief+" "+result.Detailed)
		if err == nil {
			_ = c.memStore.Store(ctx, &memory.Memory{
				UserID:   userID,
				Content:  result.Topic + ": " + result.Brief,
				Vector:   vec,
				Metadata: map[string]any{"level": "L2", "topic_id": topic.ID},
			})
		}
	}

	// 删除已聚合的 episodes
	var ids []int64
	for _, e := range episodes {
		ids = append(ids, e.ID)
	}
	if err := c.db.DeleteEpisodesByID(ctx, userID, ids); err != nil {
		log.Printf("compressor: delete clustered episodes: %v", err)
	}

	log.Printf("compressor: L1→L2 聚合完成, user=%d, %d条→1条主题", userID, len(episodes))
	return nil
}

// ─── 压缩 prompt ───

const compressSystemPrompt = `你是一个记忆压缩引擎。你的任务是阅读一段对话记录，生成压缩后的记忆。

输出格式（严格 JSON）：
{
  "brief": "一句话总结这轮对话的核心内容（不超过50字）",
  "detailed": "详细摘要，保留关键事实、情感、决策（不超过200字）",
  "facts": ["结构化事实1", "结构化事实2"]
}

facts 规则：
- 只提取客观事实，不提取寒暄/闲聊
- 格式："用户喜欢猫"、"用户的猫叫小雪"、"用户提到贫血"
- 每条事实不超过20字

注意：只输出 JSON，不要任何额外文字。`

const clusterSystemPrompt = `你是一个记忆聚合引擎。你的任务是阅读多段对话摘要，聚合为一个主题。

输出格式（严格 JSON）：
{
  "topic": "主题名称（2-6字，如'宠物话题'、'健康咨询'）",
  "brief": "一句话概括这些对话的主题（不超过50字）",
  "detailed": "详细描述该主题下的关键信息（不超过300字）",
  "facts": ["聚合后的关键事实1", "聚合后的关键事实2"]
}

注意：
- 合并重复事实
- 保留最重要的信息
- 只输出 JSON，不要任何额外文字`

// compressResult L0→L1 压缩结果
type compressResult struct {
	Brief    string   `json:"brief"`
	Detailed string   `json:"detailed"`
	Facts    []string `json:"facts"`
}

// clusterResult L1→L2 聚合结果
type clusterResult struct {
	Topic    string   `json:"topic"`
	Brief    string   `json:"brief"`
	Detailed string   `json:"detailed"`
	Facts    []string `json:"facts"`
}

func formatConversations(convs []*model.Conversation) string {
	var s string
	for _, c := range convs {
		role := "用户"
		if c.Role == "assistant" {
			role = "蓝妹"
		}
		s += fmt.Sprintf("%s: %s\n", role, c.Content)
	}
	return s
}

func buildCompressPrompt(dialogue string) string {
	return fmt.Sprintf("请压缩以下对话记录：\n\n%s", dialogue)
}

func buildClusterPrompt(episodeTexts string) string {
	return fmt.Sprintf("请将以下对话片段聚合为一个主题：\n\n%s", episodeTexts)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
