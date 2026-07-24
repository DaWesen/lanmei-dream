package database

import (
	"context"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
)

// ─── L0: 原始对话 ───

// CountConversations 统计用户的原始对话条数
func (db *DB) CountConversations(ctx context.Context, userID int64) (int, error) {
	var count int64
	err := db.Orm.WithContext(ctx).Model(&model.Conversation{}).
		Where("user_id = ?", userID).
		Count(&count).Error
	return int(count), err
}

// GetOldestConversations 获取用户最老的 N 条对话（用于压缩）
func (db *DB) GetOldestConversations(ctx context.Context, userID int64, limit int) ([]*model.Conversation, error) {
	var convs []*model.Conversation
	err := db.Orm.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at ASC").
		Limit(limit).
		Find(&convs).Error
	if err != nil {
		return nil, fmt.Errorf("get_oldest_conversations: %w", err)
	}
	return convs, nil
}

// DeleteConversationsInRange 删除指定 ID 范围内的对话（压缩后清理）
// 使用参数化查询防止 SQL 注入
func (db *DB) DeleteConversationsInRange(ctx context.Context, userID int64, firstID, lastID int64) error {
	err := db.Orm.WithContext(ctx).
		Where("user_id = ? AND id BETWEEN ? AND ?", userID, firstID, lastID).
		Delete(&model.Conversation{}).Error
	if err != nil {
		return fmt.Errorf("delete_conversations_in_range: %w", err)
	}
	return nil
}

// ─── L1: Episode Summary ───

// SaveEpisodeSummary 存储一条对话摘要
func (db *DB) SaveEpisodeSummary(ctx context.Context, e *model.EpisodeSummary) error {
	if err := db.Orm.WithContext(ctx).Create(e).Error; err != nil {
		return fmt.Errorf("save_episode_summary: %w", err)
	}
	return nil
}

// GetRecentEpisodes 获取用户最近的 N 条 L1 摘要（按时间正序）
func (db *DB) GetRecentEpisodes(ctx context.Context, userID int64, limit int) ([]*model.EpisodeSummary, error) {
	var episodes []*model.EpisodeSummary
	err := db.Orm.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Limit(limit).
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("get_recent_episodes: %w", err)
	}

	for i, j := 0, len(episodes)-1; i < j; i, j = i+1, j-1 {
		episodes[i], episodes[j] = episodes[j], episodes[i]
	}
	return episodes, nil
}

// CountEpisodes 统计用户的 L1 摘要条数
func (db *DB) CountEpisodes(ctx context.Context, userID int64) (int, error) {
	var count int64
	err := db.Orm.WithContext(ctx).Model(&model.EpisodeSummary{}).
		Where("user_id = ?", userID).
		Count(&count).Error
	return int(count), err
}

// GetOldestEpisodes 获取最老的 N 条 L1 摘要（用于 L2 聚合）
func (db *DB) GetOldestEpisodes(ctx context.Context, userID int64, limit int) ([]*model.EpisodeSummary, error) {
	var episodes []*model.EpisodeSummary
	err := db.Orm.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at ASC").
		Limit(limit).
		Find(&episodes).Error
	if err != nil {
		return nil, fmt.Errorf("get_oldest_episodes: %w", err)
	}
	return episodes, nil
}

// DeleteEpisodesByID 删除指定 ID 列表的 L1 摘要
// 使用参数化 IN 查询防止 SQL 注入
func (db *DB) DeleteEpisodesByID(ctx context.Context, userID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	err := db.Orm.WithContext(ctx).
		Where("user_id = ? AND id IN ?", userID, ids).
		Delete(&model.EpisodeSummary{}).Error
	if err != nil {
		return fmt.Errorf("delete_episodes_by_id: %w", err)
	}
	return nil
}

// ─── L2: Topic Cluster ───

// SaveTopicCluster 存储一条主题聚类
func (db *DB) SaveTopicCluster(ctx context.Context, t *model.TopicCluster) error {
	if err := db.Orm.WithContext(ctx).Create(t).Error; err != nil {
		return fmt.Errorf("save_topic_cluster: %w", err)
	}
	return nil
}

// GetRecentTopics 获取用户最近的 N 条 L2 主题（按时间正序）
func (db *DB) GetRecentTopics(ctx context.Context, userID int64, limit int) ([]*model.TopicCluster, error) {
	var topics []*model.TopicCluster
	err := db.Orm.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Limit(limit).
		Find(&topics).Error
	if err != nil {
		return nil, fmt.Errorf("get_recent_topics: %w", err)
	}

	for i, j := 0, len(topics)-1; i < j; i, j = i+1, j-1 {
		topics[i], topics[j] = topics[j], topics[i]
	}
	return topics, nil
}

// ─── 多级上下文组装 ───

// LODContext 是多级上下文组装的结果
type LODContext struct {
	TopicBriefs      []string               // L2 主题一句话
	EpisodeBriefs    []string               // L1 摘要一句话
	EpisodeDetails   []string               // L1 摘要详细版
	RawConversations []*model.Conversation   // L0 原始对话
}

// GetLODContext 按 Token 预算组装多级上下文
// budget 是大致的 token 预算，函数按 L2→L1→L0 优先级填充
func (db *DB) GetLODContext(ctx context.Context, userID int64, budget int) (*LODContext, error) {
	result := &LODContext{}
	used := 0
	// 粗估：1 个中文字 ≈ 1.5 token，这里用字符数粗算
	charsPerToken := 1.5

	// L2：主题 brief（最便宜，先填）
	topics, err := db.GetRecentTopics(ctx, userID, 10)
	if err != nil {
		return nil, fmt.Errorf("lod l2: %w", err)
	}
	for _, t := range topics {
		cost := int(float64(len(t.Brief)) / charsPerToken)
		if used+cost > budget {
			break
		}
		result.TopicBriefs = append(result.TopicBriefs, t.Topic+": "+t.Brief)
		used += cost
	}

	// L1：episode brief + detailed
	episodes, err := db.GetRecentEpisodes(ctx, userID, 10)
	if err != nil {
		return nil, fmt.Errorf("lod l1: %w", err)
	}
	for _, e := range episodes {
		briefCost := int(float64(len(e.Brief)) / charsPerToken)
		if used+briefCost <= budget {
			result.EpisodeBriefs = append(result.EpisodeBriefs, e.Brief)
			used += briefCost
		}
		detailCost := int(float64(len(e.Detailed)) / charsPerToken)
		if used+detailCost <= budget {
			result.EpisodeDetails = append(result.EpisodeDetails, e.Detailed)
			used += detailCost
		}
	}

	// L0：原始对话（剩余预算全给原文）
	rawBudget := budget - used
	if rawBudget > 0 {
		rawLimit := rawBudget / 30 // 粗估每条对话约 30 token
		if rawLimit < 2 {
			rawLimit = 2
		}
		if rawLimit > 40 {
			rawLimit = 40
		}
		convs, err := db.GetRecentConversations(ctx, userID, rawLimit)
		if err != nil {
			return nil, fmt.Errorf("lod l0: %w", err)
		}
		result.RawConversations = convs
	}

	return result, nil
}
