package model

import "time"

// Memory 对应 memories 表，存储结构化记忆元数据
// （向量本身存储在 Milvus 中，此处仅存元数据 + 文本）
type Memory struct {
	ID        int64     `json:"id"         db:"id"`
	UserID    int64     `json:"user_id"    db:"user_id"`
	Content   string    `json:"content"    db:"content"`
	Metadata  []byte    `json:"metadata"   db:"metadata"` // JSONB
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}
