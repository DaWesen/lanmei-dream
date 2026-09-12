package embedding

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino-ext/components/embedding/openai"
	"github.com/cloudwego/eino/components/embedding"
)

// EinoOptions eino Embedding 创建参数（provider 无关）
type EinoOptions struct {
	BaseURL   string // API 基础地址
	APIKey    string // API 密钥
	Model     string // 模型名
	Dimension int    // 向量维度
}

// EinoEmbedder 基于 eino 的 Embedder 实现。
// 通过 eino-ext 的 OpenAI 兼容层调用，天然支持多 provider。
type EinoEmbedder struct {
	embedder  embedding.Embedder
	dimension int
}

// NewEinoEmbedder 创建 eino Embedding 客户端
func NewEinoEmbedder(ctx context.Context, opts *EinoOptions) (*EinoEmbedder, error) {
	// 注意：不传 Dimensions。eino 会在请求体携带 dimensions 参数，
	// 但硅基流动 /embeddings 不接受该参数（BAAI/bge-m3 固定输出 1024 维），
	// 传了会返回 400 "The parameter is invalid"。
	emb, err := openai.NewEmbedder(ctx, &openai.EmbeddingConfig{
		BaseURL: opts.BaseURL,
		APIKey:  opts.APIKey,
		Model:   opts.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: eino init: %w", err)
	}
	return &EinoEmbedder{embedder: emb, dimension: opts.Dimension}, nil
}

// Dimension 实现 Embedder 接口
func (e *EinoEmbedder) Dimension() int {
	return e.dimension
}

// Embed 实现 Embedder 接口
func (e *EinoEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedding: empty response")
	}
	return vecs[0], nil
}

// EmbedBatch 实现 Embedder 接口
func (e *EinoEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// eino Embedder 接口：EmbedStrings 输入 []string，返回 [][]float64
	vecs64, err := e.embedder.EmbedStrings(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embedding: eino embed: %w", err)
	}

	// float64 → float32
	vecs32 := make([][]float32, len(vecs64))
	for i, v64 := range vecs64 {
		v32 := make([]float32, len(v64))
		for j, f := range v64 {
			v32[j] = float32(f)
		}
		vecs32[i] = v32
	}
	return vecs32, nil
}
