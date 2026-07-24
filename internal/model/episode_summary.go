package model

import "time"

// EpisodeSummary 对应 episode_summaries 表（L1 层）
// 一条摘要由多轮原始对话压缩而来，保留 brief + detailed 双粒度 + 结构化事实
type EpisodeSummary struct {
	ID           int64     `json:"id"              gorm:"primaryKey;autoIncrement;comment:摘要ID"`
	UserID       int64     `json:"user_id"         gorm:"index:idx_episode_user;not null;comment:用户ID"`
	Brief        string    `json:"brief"           gorm:"not null;comment:一句话总结"`
	Detailed     string    `json:"detailed"        gorm:"not null;comment:详细摘要"`
	Facts        []byte    `json:"facts"           gorm:"type:jsonb;comment:结构化事实列表"`
	CoveredCount int       `json:"covered_count"   gorm:"not null;default:0;comment:被压缩的原文条数"`
	FirstConvoID int64     `json:"first_convo_id"  gorm:"not null;default:0;comment:被压缩的第一条对话ID"`
	LastConvoID  int64     `json:"last_convo_id"   gorm:"not null;default:0;comment:被压缩的最后一条对话ID"`
	CreatedAt    time.Time `json:"created_at"      gorm:"autoCreateTime;index:idx_episode_user;comment:创建时间"`
}
