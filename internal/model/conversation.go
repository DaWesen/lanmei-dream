package model

import "time"

// Conversation 对应 conversations 表，存储对话历史
type Conversation struct {
	ID        int64     `json:"id"         db:"id"`
	UserID    int64     `json:"user_id"    db:"user_id"`
	Role      string    `json:"role"       db:"role"`
	Content   string    `json:"content"    db:"content"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}
