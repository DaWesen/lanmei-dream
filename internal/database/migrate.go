package database

import (
	"context"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
	"go.uber.org/zap"
)

// defaultKnowledgeVectorDim 知识库向量列的默认维度（与 memory_vectors 保持一致）。
// 若配置的 ai.embedding_dim 不同，迁移时会 ALTER 到配置维度。
const defaultKnowledgeVectorDim = 1024

// Migrate 使用 GORM AutoMigrate 自动建表（幂等），并确保 pgvector / pg_trgm 扩展和索引就绪。
//
// vectorDim 为知识库向量列的目标维度（来自 ai.embedding_dim 配置）；
// 若 >0 且与默认维度不同，迁移时对 knowledge_chunks.embedding 执行 ALTER 自适应。
func (db *DB) Migrate(ctx context.Context, vectorDim int) error {
	// 启用 pgvector 扩展
	if err := db.Orm.WithContext(ctx).Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("enable pgvector: %w", err)
	}
	db.logger.Info("pgvector 扩展已启用")

	// 启用 pg_trgm 扩展（本地知识库模糊召回倒排索引）
	if err := db.Orm.WithContext(ctx).Exec("CREATE EXTENSION IF NOT EXISTS pg_trgm").Error; err != nil {
		return fmt.Errorf("enable pg_trgm: %w", err)
	}
	db.logger.Info("pg_trgm 扩展已启用")

	if err := db.Orm.WithContext(ctx).AutoMigrate(
		&model.User{},
		&model.Conversation{},
		&model.Memory{},
		&model.EpisodeSummary{},
		&model.TopicCluster{},
		&model.MemoryVector{},
		&model.MediaFile{},
		&model.PluginInstallation{},
		&model.PluginKV{},
		&model.KnowledgeChunk{},
		&model.StickerLibrary{},
		&model.BotAdmin{},
		&model.GroupFact{},
		// 管理面板（Manager）专属表
		&model.ManagerAdmin{},
		&model.AuthCredential{},
		&model.AuthSession{},
		&model.LoginAttempt{},
		&model.AuditLog{},
		&model.ConfigRevision{},
		&model.ConduitTrace{},
		&model.NodeTraffic{},
		&model.LLMProvider{},
		&model.TokenUsage{},
		&model.GroupConfig{},
		&model.ScheduledJob{},
	); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// 创建 HNSW 索引（幂等，已存在则跳过）
	db.Orm.WithContext(ctx).Exec(
		"CREATE INDEX IF NOT EXISTS idx_memory_vectors_embedding ON memory_vectors USING hnsw (embedding vector_cosine_ops)",
	)
	db.logger.Info("HNSW 向量索引已就绪")

	// 创建 GIN 全文搜索索引（用于关键词召回）
	db.Orm.WithContext(ctx).Exec(
		"CREATE INDEX IF NOT EXISTS idx_memory_vectors_search_vec ON memory_vectors USING gin (search_vec)",
	)
	db.logger.Info("GIN 全文搜索索引已就绪")

	// 创建触发器函数：INSERT/UPDATE 时自动从 content 生成 tsvector
	// 使用 simple 配置（不分词，按空白切割），适合中文等非空格分词语言
	db.Orm.WithContext(ctx).Exec(`
CREATE OR REPLACE FUNCTION memory_vectors_search_vec_trigger() RETURNS trigger AS $$
BEGIN
  NEW.search_vec := to_tsvector('simple', COALESCE(NEW.content, ''));
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`)

	// 幂等创建触发器（DROP IF EXISTS 再 CREATE，避免重复绑定）
	db.Orm.WithContext(ctx).Exec(`DROP TRIGGER IF EXISTS trg_memory_vectors_search_vec ON memory_vectors`)
	db.Orm.WithContext(ctx).Exec(`
CREATE TRIGGER trg_memory_vectors_search_vec
  BEFORE INSERT OR UPDATE OF content ON memory_vectors
  FOR EACH ROW EXECUTE FUNCTION memory_vectors_search_vec_trigger()`)
	db.logger.Info("全文搜索触发器已就绪")

	// ── 知识库索引 ──
	// 向量召回（HNSW）
	db.Orm.WithContext(ctx).Exec(
		"CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_embedding ON knowledge_chunks USING hnsw (embedding vector_cosine_ops)",
	)
	// 模糊召回（pg_trgm GIN 倒排索引，中英文子串/模糊匹配）
	db.Orm.WithContext(ctx).Exec(
		"CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_trgm ON knowledge_chunks USING gin (content gin_trgm_ops)",
	)
	db.logger.Info("知识库索引已就绪")

	// 向量维度自适应：与配置的 ai.embedding_dim 保持一致
	// 注意：vector(N) 的类型修饰符无法参数化，N 为配置的整数维度（非用户输入），直接拼接安全。
	if vectorDim > 0 && vectorDim != defaultKnowledgeVectorDim {
		if err := db.Orm.WithContext(ctx).Exec(
			fmt.Sprintf("ALTER TABLE knowledge_chunks ALTER COLUMN embedding TYPE vector(%d)", vectorDim),
		).Error; err != nil {
			return fmt.Errorf("alter knowledge_chunks embedding dimension: %w", err)
		}
		db.logger.Info("知识库向量维度已调整", zap.Int("dim", vectorDim))
	}

	return nil
}
