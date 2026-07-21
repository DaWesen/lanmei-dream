package database

import (
	"context"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
)

// GetOrCreateUser 按 QQ 号查找或创建用户
func (db *DB) GetOrCreateUser(ctx context.Context, qqID int64, nickname string) (*model.User, error) {
	var u model.User
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (qq_id, nickname)
		VALUES ($1, $2)
		ON CONFLICT (qq_id) DO UPDATE SET nickname = EXCLUDED.nickname
		RETURNING id, qq_id, nickname, created_at, updated_at
	`, qqID, nickname).Scan(&u.ID, &u.QQID, &u.Nickname, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get_or_create_user: %w", err)
	}
	return &u, nil
}

// SaveConversation 存储一条对话记录
func (db *DB) SaveConversation(ctx context.Context, userID int64, role, content string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO conversations (user_id, role, content)
		VALUES ($1, $2, $3)
	`, userID, role, content)
	if err != nil {
		return fmt.Errorf("save_conversation: %w", err)
	}
	return nil
}

// GetRecentConversations 获取用户最近的 N 条对话（按时间正序）
func (db *DB) GetRecentConversations(ctx context.Context, userID int64, limit int) ([]*model.Conversation, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, user_id, role, content, created_at
		FROM conversations
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get_recent_conversations: %w", err)
	}
	defer rows.Close()

	var convs []*model.Conversation
	for rows.Next() {
		var c model.Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Role, &c.Content, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		convs = append(convs, &c)
	}

	// 反转为时间正序
	for i, j := 0, len(convs)-1; i < j; i, j = i+1, j-1 {
		convs[i], convs[j] = convs[j], convs[i]
	}
	return convs, rows.Err()
}

// SearchConversations 按关键词模糊搜索用户对话
func (db *DB) SearchConversations(ctx context.Context, userID int64, keyword string, limit int) ([]*model.Conversation, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, user_id, role, content, created_at
		FROM conversations
		WHERE user_id = $1 AND content ILIKE '%' || $2 || '%'
		ORDER BY created_at DESC
		LIMIT $3
	`, userID, keyword, limit)
	if err != nil {
		return nil, fmt.Errorf("search_conversations: %w", err)
	}
	defer rows.Close()

	var convs []*model.Conversation
	for rows.Next() {
		var c model.Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Role, &c.Content, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		convs = append(convs, &c)
	}
	return convs, rows.Err()
}
