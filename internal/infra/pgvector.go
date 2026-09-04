package infra

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	"github.com/DaWesen/lanmei-dream/internal/ai/memory"
	"github.com/DaWesen/lanmei-dream/internal/model"
)

// 确保 PGVectorStore 实现 memory.MemoryStore 接口
var _ memory.MemoryStore = (*PGVectorStore)(nil)

// PGVectorStore 基于 PostgreSQL + pgvector 的 MemoryStore 实现
type PGVectorStore struct {
	orm *gorm.DB
}

// NewPGVectorStore 创建基于 pgvector 的记忆存储
func NewPGVectorStore(db *gorm.DB) *PGVectorStore {
	return &PGVectorStore{orm: db}
}

// Store 存储一条记忆（含向量）。
// mem.GroupID 非空时写入群级记忆（user_id=0 的场景由调用方自行设置）。
func (s *PGVectorStore) Store(ctx context.Context, mem *memory.Memory) error {
	row := &model.MemoryVector{
		UserID:    mem.UserID,
		GroupID:   mem.GroupID,
		Content:   mem.Content,
		Embedding: pgvector.NewVector(mem.Vector),
	}
	return s.orm.WithContext(ctx).Create(row).Error
}

// memoryGroupScope 构造记忆检索的群级过滤条件。
//   - groupID 为空（私聊）：仅用户个人记忆（group_id=”）；
//   - groupID 非空（群聊）：该群的群级记忆（group_id=gid）或该用户的个人记忆，
//     避免跨群污染与个人记忆混淆。
func memoryGroupScope(groupID string, userID int64) (scope string, args []any) {
	if groupID == "" {
		return "user_id = ? AND group_id = ''", []any{userID}
	}
	return "(group_id = ?) OR (user_id = ? AND group_id = '')", []any{groupID, userID}
}

// Retrieve 根据查询向量检索最相关的 N 条记忆（向量召回）
func (s *PGVectorStore) Retrieve(ctx context.Context, queryVec []float32, userID int64, groupID string, limit int) ([]*memory.Memory, error) {
	// 向量以参数形式传入并显式 ::vector 转换：参数化后以 text 到达，
	// <=> 操作符无法从 $n 推断 vector 类型，缺 cast 会报
	// operator does not exist: vector <=> text。
	// （kb/provider/local 的向量召回是同模式的既有正确实现。）
	vecStr := formatVector(queryVec)
	scope, args := memoryGroupScope(groupID, userID)

	var rows []model.MemoryVector
	err := s.orm.WithContext(ctx).Raw(
		`SELECT * FROM memory_vectors WHERE `+scope+` ORDER BY embedding <=> ?::vector LIMIT ?`,
		append(args, vecStr, limit)...,
	).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("pgvector retrieve: %w", err)
	}

	return rowsToMemories(rows), nil
}

// Delete 删除指定 ID 的记忆
func (s *PGVectorStore) Delete(ctx context.Context, id string) error {
	pk, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q: %w", id, err)
	}
	return s.orm.WithContext(ctx).Delete(&model.MemoryVector{}, pk).Error
}

// RetrieveByKeyword 根据关键词全文搜索检索记忆（关键词召回）
// 使用 PostgreSQL tsvector 全文搜索，simple 配置按空白切割适合中文
func (s *PGVectorStore) RetrieveByKeyword(ctx context.Context, query string, userID int64, groupID string, limit int) ([]*memory.Memory, error) {
	// 将查询文本转换为 tsquery：按空白分割，用 & (AND) 连接
	tsQuery := toSimpleTSQuery(query)
	if tsQuery == "" {
		return nil, nil
	}
	scope, args := memoryGroupScope(groupID, userID)
	// 拼装 WHERE：scope AND search_vec @@ to_tsquery('simple', ?)
	whereSQL := scope + " AND search_vec @@ to_tsquery('simple', ?)"
	args = append(args, tsQuery)

	var rows []model.MemoryVector
	err := s.orm.WithContext(ctx).Raw(
		"SELECT * FROM memory_vectors WHERE "+whereSQL+
			" ORDER BY ts_rank(search_vec, to_tsquery('simple', ?)) DESC LIMIT ?",
		append(args, tsQuery, limit)...,
	).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("pgvector keyword retrieve: %w", err)
	}

	return rowsToMemories(rows), nil
}

// RetrieveByTime 根据时间倒序检索最近的 N 条记忆（时间召回）
func (s *PGVectorStore) RetrieveByTime(ctx context.Context, userID int64, groupID string, limit int) ([]*memory.Memory, error) {
	scope, args := memoryGroupScope(groupID, userID)

	var rows []model.MemoryVector
	err := s.orm.WithContext(ctx).
		Where(scope, args...).
		Order("created_at DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("pgvector time retrieve: %w", err)
	}

	return rowsToMemories(rows), nil
}

// toSimpleTSQuery 将自然语言查询转为 simple 配置的 tsquery。
//
// 按 Unicode 空白分割后用 & (AND) 连接各词项；单词项时直接返回词项本身。
// 注意：词项必须先收集再用 " & " 连接，不能逐项追加 "&" 后缀——
// 否则 "你好啊" 会变成 "你好啊&"（& 是 tsquery 前缀操作符，后无操作数时
// PostgreSQL 报 "no operand in tsquery"）。
func toSimpleTSQuery(query string) string {
	var parts []string
	for _, w := range splitWhitespace(query) {
		if w != "" {
			parts = append(parts, w)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " & ")
}

// splitWhitespace 按空白字符分割字符串（模拟 strings.Fields 但返回可遍历切片）
func splitWhitespace(s string) []string {
	var fields []string
	var buf []rune
	for _, r := range s {
		if isWhitespace(r) {
			if len(buf) > 0 {
				fields = append(fields, string(buf))
				buf = buf[:0]
			}
		} else {
			buf = append(buf, r)
		}
	}
	if len(buf) > 0 {
		fields = append(fields, string(buf))
	}
	return fields
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// rowsToMemories 将数据库行转换为 Memory 切片
func rowsToMemories(rows []model.MemoryVector) []*memory.Memory {
	memories := make([]*memory.Memory, len(rows))
	for i, row := range rows {
		memories[i] = &memory.Memory{
			ID:      strconv.FormatInt(row.ID, 10),
			UserID:  row.UserID,
			GroupID: row.GroupID,
			Content: row.Content,
			Vector:  row.Embedding.Slice(),
		}
	}
	return memories
}

// formatVector 将 float32 切片格式化为 SQL 向量字面量 '[0.1,0.2,...]'
func formatVector(vec []float32) string {
	s := "["
	for i, v := range vec {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%f", v)
	}
	s += "]"
	return s
}
