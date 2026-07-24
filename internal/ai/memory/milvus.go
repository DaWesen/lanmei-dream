package memory

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// MilvusMemoryStore 基于 Milvus 的 MemoryStore 实现
type MilvusMemoryStore struct {
	cli            client.Client
	collectionName string
	dimension      int
}

// NewMilvusMemoryStore 连接 Milvus 并确保集合和索引就绪
func NewMilvusMemoryStore(ctx context.Context, addr, collectionName string, dim int) (*MilvusMemoryStore, error) {
	cli, err := client.NewClient(ctx, client.Config{
		Address: addr,
	})
	if err != nil {
		return nil, fmt.Errorf("milvus connect: %w", err)
	}

	m := &MilvusMemoryStore{
		cli:            cli,
		collectionName: collectionName,
		dimension:      dim,
	}

	if err := m.ensureCollection(ctx); err != nil {
		cli.Close()
		return nil, err
	}

	return m, nil
}

// Close 关闭 Milvus 连接
func (m *MilvusMemoryStore) Close() error {
	m.cli.Close()
	return nil
}

// ensureCollection 检查集合是否存在，不存在则创建 + 建索引 + 加载
func (m *MilvusMemoryStore) ensureCollection(ctx context.Context) error {
	has, err := m.cli.HasCollection(ctx, m.collectionName)
	if err != nil {
		return fmt.Errorf("check collection: %w", err)
	}
	if has {
		// 确保已加载
		if err := m.cli.LoadCollection(ctx, m.collectionName, false); err != nil {
			log.Printf("milvus: load collection (may already be loaded): %v", err)
		}
		return nil
	}

	schema := entity.NewSchema().WithName(m.collectionName).
		WithField(entity.NewField().
			WithName("id").
			WithDataType(entity.FieldTypeInt64).
			WithIsPrimaryKey(true).
			WithIsAutoID(true)).
		WithField(entity.NewField().
			WithName("user_id").
			WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName("content").
			WithDataType(entity.FieldTypeVarChar).
			WithMaxLength(2048)).
		WithField(entity.NewField().
			WithName("embedding").
			WithDataType(entity.FieldTypeFloatVector).
			WithDim(int64(m.dimension)))

	if err := m.cli.CreateCollection(ctx, schema, 1); err != nil {
		return fmt.Errorf("create collection: %w", err)
	}

	// 为向量字段创建索引
	idx, err := entity.NewIndexIvfFlat(entity.L2, 1024)
	if err != nil {
		return fmt.Errorf("create index param: %w", err)
	}
	if err := m.cli.CreateIndex(ctx, m.collectionName, "embedding", idx, false); err != nil {
		return fmt.Errorf("create index: %w", err)
	}

	// 加载集合到内存
	if err := m.cli.LoadCollection(ctx, m.collectionName, false); err != nil {
		return fmt.Errorf("load collection: %w", err)
	}

	log.Printf("milvus: collection %q created and loaded", m.collectionName)
	return nil
}

// Store 存储一条记忆
func (m *MilvusMemoryStore) Store(ctx context.Context, mem *Memory) error {
	userIDCol := entity.NewColumnInt64("user_id", []int64{mem.UserID})
	contentCol := entity.NewColumnVarChar("content", []string{mem.Content})
	embeddingCol := entity.NewColumnFloatVector("embedding", m.dimension, [][]float32{mem.Vector})

	_, err := m.cli.Insert(ctx, m.collectionName, "", userIDCol, contentCol, embeddingCol)
	if err != nil {
		return fmt.Errorf("milvus insert: %w", err)
	}

	// 刷新以确保可检索
	_ = m.cli.Flush(ctx, m.collectionName, false)
	return nil
}

// Retrieve 根据查询向量检索最相关的 N 条记忆
func (m *MilvusMemoryStore) Retrieve(ctx context.Context, queryVec []float32, userID int64, limit int) ([]*Memory, error) {
	vectors := []entity.Vector{entity.FloatVector(queryVec)}

	sp, err := entity.NewIndexIvfFlatSearchParam(10)
	if err != nil {
		return nil, fmt.Errorf("search param: %w", err)
	}

	expr := fmt.Sprintf("user_id == %d", userID)

	results, err := m.cli.Search(ctx, m.collectionName, nil, expr,
		[]string{"id", "user_id", "content"},
		vectors, "embedding", entity.L2, limit, sp)
	if err != nil {
		return nil, fmt.Errorf("milvus search: %w", err)
	}

	var memories []*Memory
	if len(results) == 0 {
		return memories, nil
	}

	// 遍历搜索结果提取字段
	for _, result := range results {
		for i := 0; i < result.ResultCount; i++ {
			mem := &Memory{}
			id, err := result.IDs.Get(i)
			if err == nil {
				mem.ID = fmt.Sprintf("%v", id)
			}
			// 通过 GetColumn 按字段名取值
			contentCol := result.Fields.GetColumn("content")
			if contentCol != nil {
				if v, err := contentCol.Get(i); err == nil {
					if s, ok := v.(string); ok {
						mem.Content = s
					}
				}
			}
			userIDCol := result.Fields.GetColumn("user_id")
			if userIDCol != nil {
				if v, err := userIDCol.Get(i); err == nil {
					if n, ok := v.(int64); ok {
						mem.UserID = n
					}
				}
			}
			memories = append(memories, mem)
		}
	}

	return memories, nil
}

// Delete 删除指定 ID 的记忆
func (m *MilvusMemoryStore) Delete(ctx context.Context, id string) error {
	// id 为 Milvus 自增主键
	pk, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q: %w", id, err)
	}

	err = m.cli.Delete(ctx, m.collectionName, "", fmt.Sprintf("id == %d", pk))
	if err != nil {
		return fmt.Errorf("milvus delete: %w", err)
	}
	return nil
}

// Ping 检查 Milvus 连接是否正常
func (m *MilvusMemoryStore) Ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 通过列出集合来验证连接
	_, err := m.cli.ListCollections(pctx)
	return err
}
