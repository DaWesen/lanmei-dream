package ai

import "context"

// Memory 表示一条长期记忆，用于 RAG 上下文增强
type Memory struct {
	ID       string
	UserID   int64
	Content  string
	Vector   []float32
	Metadata map[string]any
}

// MemoryStore 抽象记忆的存储与检索。
// MilvusMemoryStore 是其基于 Milvus 的实现。
type MemoryStore interface {
	// Store 存储一条记忆（含向量）
	Store(ctx context.Context, mem *Memory) error
	// Retrieve 根据查询向量检索最相关的 N 条记忆
	Retrieve(ctx context.Context, queryVec []float32, userID int64, limit int) ([]*Memory, error)
	// Delete 删除指定记忆
	Delete(ctx context.Context, id string) error
}
