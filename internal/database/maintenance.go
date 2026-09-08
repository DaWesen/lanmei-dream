package database

import (
	"context"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/model"
)

// CleanupOldGroupConversations 清理群聊原始对话：每个 (user_id, group_id) 维度
// 仅保留最近 keepPerScope 条，删除更旧的记录。
//
// 群聊 L0 不参与压缩（压缩仅针对私聊 group_id=''），且 topic 系统已对群聊做
// 归档摘要，因此按维度保留上限即可控制 conversations 表无限膨胀，
// 不影响 LOD 组装（L2/L1 摘要 + 近期 L0 原文仍在）。
func (db *DB) CleanupOldGroupConversations(ctx context.Context, keepPerScope int) (int64, error) {
	if keepPerScope <= 0 {
		return 0, nil
	}
	res := db.Orm.WithContext(ctx).Exec(`
		DELETE FROM conversations
		WHERE group_id != ''
		  AND id NOT IN (
		    SELECT id FROM (
		      SELECT id,
		             ROW_NUMBER() OVER (PARTITION BY user_id, group_id ORDER BY id DESC) AS rn
		      FROM conversations
		      WHERE group_id != ''
		    ) ranked
		    WHERE rn <= ?
		  )`, keepPerScope)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// CleanupExpiredMemories 删除早于 olderThan 的长期向量记忆。
// 长期未命中的记忆随时间衰减淘汰，避免 memory_vectors 无限积累。
func (db *DB) CleanupExpiredMemories(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	res := db.Orm.WithContext(ctx).
		Where("created_at < ?", time.Now().Add(-olderThan)).
		Delete(&model.MemoryVector{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// CleanupOldTopics 清理 L2 主题聚类：每个用户仅保留最近 keepPerUser 个，删除更旧的。
//
// L2 聚合（compressL1ToL2）持续产生 TopicCluster 且只删 episodes 不删 topic，
// 表会无限增长；LOD 消费仅取每用户最近 10 个主题，保留最近 N 个足够。
func (db *DB) CleanupOldTopics(ctx context.Context, keepPerUser int) (int64, error) {
	if keepPerUser <= 0 {
		return 0, nil
	}
	res := db.Orm.WithContext(ctx).Exec(`
		DELETE FROM topic_clusters
		WHERE id NOT IN (
		  SELECT id FROM (
		    SELECT id,
		           ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY id DESC) AS rn
		    FROM topic_clusters
		  ) ranked
		  WHERE rn <= ?
		)`, keepPerUser)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
