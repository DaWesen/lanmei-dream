package ai

import (
	"context"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/database"
	"go.uber.org/zap"
)

// 记忆维护默认参数：
//   - groupConvKeepPerScope：群聊每个 (user, group) 维度保留的 L0 原文条数上限
//   - topicKeepPerUser：每个用户保留的 L2 主题聚类条数上限（LOD 仅取最近 10 个）
//   - memoryRetention：长期向量记忆的超龄保留期（到期未更新即淘汰）
//   - maintainInterval：后台清理周期
const (
	groupConvKeepPerScope = 200
	topicKeepPerUser      = 50
	memoryRetention       = 180 * 24 * time.Hour
	maintainInterval      = 6 * time.Hour
)

// MemoryMaintainer 后台记忆维护：按周期清理膨胀的对话表与超龄的向量记忆。
//
// 背景：群聊 L0 原始对话不参与压缩（压缩仅针对私聊），只增不减；
// L2 聚合持续写入 memory_vectors 且 TopicCluster 只增不删。
// 维护器按"保留上限 + 时间衰减"策略清理（借鉴长期记忆的遗忘思想）：
//   - 群聊对话：每个 (user_id, group_id) 维度仅保留最近 N 条；
//   - L2 主题：每个用户仅保留最近 N 个聚类；
//   - 向量记忆：删除超过保留期未变动的记忆。
//
// 不影响检索质量：LOD 组装的 L2/L1 摘要与近期 L0 原文均保留。
type MemoryMaintainer struct {
	db          *database.DB
	logger      *zap.Logger
	interval    time.Duration
	convKeep    int
	topicKeep   int
	expireAfter time.Duration
}

// NewMemoryMaintainer 创建记忆维护器（使用默认参数）。
func NewMemoryMaintainer(db *database.DB, logger *zap.Logger) *MemoryMaintainer {
	return &MemoryMaintainer{
		db:          db,
		logger:      logger,
		interval:    maintainInterval,
		convKeep:    groupConvKeepPerScope,
		topicKeep:   topicKeepPerUser,
		expireAfter: memoryRetention,
	}
}

// Start 启动后台清理 goroutine：立即执行一次，之后按 interval 周期执行。
// ctx 取消时退出（不阻塞）。
func (m *MemoryMaintainer) Start(ctx context.Context) {
	if m.db == nil {
		return
	}
	go func() {
		m.runOnce(context.Background())
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runOnce(context.Background())
			}
		}
	}()
}

// runOnce 执行一次清理并记录结果；失败仅记日志不中断周期。
func (m *MemoryMaintainer) runOnce(ctx context.Context) {
	convs, err := m.db.CleanupOldGroupConversations(ctx, m.convKeep)
	if err != nil {
		m.logger.Warn("memory maintainer: 清理群聊对话失败", zap.Error(err))
		return
	}
	topics, err := m.db.CleanupOldTopics(ctx, m.topicKeep)
	if err != nil {
		m.logger.Warn("memory maintainer: 清理主题聚类失败", zap.Error(err))
		return
	}
	mems, err := m.db.CleanupExpiredMemories(ctx, m.expireAfter)
	if err != nil {
		m.logger.Warn("memory maintainer: 清理过期记忆失败", zap.Error(err))
		return
	}
	if convs > 0 || topics > 0 || mems > 0 {
		m.logger.Info("memory maintainer: 记忆清理完成",
			zap.Int64("group_conversations", convs),
			zap.Int64("old_topics", topics),
			zap.Int64("expired_memories", mems))
	}
}
