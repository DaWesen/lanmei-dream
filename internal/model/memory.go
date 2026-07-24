package model

import "time"

// Memory 对应 memories 表，存储结构化记忆元数据
// （向量本身存储在 Milvus 中，此处仅存元数据 + 文本）
type Memory struct {
	ID        int64     `json:"id"         gorm:"primaryKey;autoIncrement;comment:记忆ID"`
	UserID    int64     `json:"user_id"    gorm:"index;not null;comment:用户ID"`
	Content   string    `json:"content"    gorm:"type:text;not null;comment:记忆文本"`
	Metadata  []byte    `json:"metadata"   gorm:"type:jsonb;comment:元数据"`
	CreatedAt time.Time `json:"created_at" gorm:"autoCreateTime;comment:创建时间"`
}
