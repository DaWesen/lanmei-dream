package embedding

import "context"

// Embedder 抽象文本向量化能力，用于 RAG 检索。
// 具体实现（OpenAI embedding / 本地模型等）由外部注入。
type Embedder interface {
	// Embed 将单段文本转为向量
	Embed(ctx context.Context, text string) ([]float32, error)
	// EmbedBatch 批量向量化
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	// Dimension 返回向量维度
	Dimension() int
}
