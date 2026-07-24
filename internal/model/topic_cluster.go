package model

import "time"

// TopicCluster 对应 topic_clusters 表（L2 层）
// 多条 episode_summaries 聚合为一个主题，向量化后存入 Milvus
type TopicCluster struct {
	ID           int64     `json:"id"              gorm:"primaryKey;autoIncrement;comment:主题ID"`
	UserID       int64     `json:"user_id"         gorm:"index:idx_topic_user;not null;comment:用户ID"`
	Topic        string    `json:"topic"           gorm:"size:255;not null;comment:主题名称"`
	Brief        string    `json:"brief"           gorm:"not null;comment:一句话主题总结"`
	Detailed     string    `json:"detailed"        gorm:"not null;comment:详细主题描述"`
	Facts        []byte    `json:"facts"           gorm:"type:jsonb;comment:聚合后的关键事实"`
	CoveredCount int       `json:"covered_count"   gorm:"not null;default:0;comment:被聚合的episode条数"`
	CreatedAt    time.Time `json:"created_at"      gorm:"autoCreateTime;comment:创建时间"`
	UpdatedAt    time.Time `json:"updated_at"      gorm:"autoUpdateTime;index:idx_topic_user;comment:更新时间"`
}
