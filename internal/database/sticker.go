package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
	"gorm.io/gorm"
)

// ── 表情库数据访问（sticker_library 表）──

// CreateSticker 插入一条表情记录。
func (db *DB) CreateSticker(ctx context.Context, sticker *model.StickerLibrary) error {
	if db.Orm == nil {
		return errors.New("database: orm is nil")
	}
	return db.Orm.WithContext(ctx).Create(sticker).Error
}

// stickerSimilarityThreshold 标签相似命中阈值（pg_trgm similarity），
// 低于该值视为不相关，不进入结果。
const stickerSimilarityThreshold = 0.15

// SearchStickers 按标签检索表情，供 LLM pick_sticker 工具调用。
// keyword 是 LLM 生成的情绪短语（如"委屈挽留"），而库内标签多为单词（如"委屈"），
// 因此命中条件按 OR 组合三种：
//   - 反向命中（核心）：某标签是情绪短语的子串（"委屈" ⊂ "委屈挽留"）；
//   - 正向命中：情绪短语是某标签的子串（原 ILIKE 行为）；
//   - 相似命中：pg_trgm 对展开后的独立标签逐个算 similarity 取最大值，超过阈值。
//
// 排序分 score：反向/正向命中强制置顶 1.0，否则取最大相似度；同分按最新优先。
func (db *DB) SearchStickers(ctx context.Context, keyword string, limit int) ([]model.StickerLibrary, error) {
	if db.Orm == nil {
		return nil, errors.New("database: orm is nil")
	}
	if limit <= 0 {
		limit = 5
	}
	if keyword == "" {
		return nil, nil
	}

	var stickers []model.StickerLibrary
	like := "%" + keyword + "%"
	// score 仅用于排序，Scan 到 []model.StickerLibrary 时多余列会被 GORM 忽略。
	err := db.Orm.WithContext(ctx).Raw(
		`SELECT s.*,
		    CASE WHEN EXISTS (SELECT 1 FROM jsonb_array_elements_text(s.tags::jsonb) t WHERE ? ILIKE '%'||t||'%')
		           OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(s.tags::jsonb) t WHERE t ILIKE ?)
		         THEN 1.0
		         ELSE (SELECT COALESCE(max(similarity(t, ?)), 0) FROM jsonb_array_elements_text(s.tags::jsonb) t)
		    END AS score
		 FROM sticker_library s
		 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements_text(s.tags::jsonb) t WHERE ? ILIKE '%'||t||'%')
		    OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(s.tags::jsonb) t WHERE t ILIKE ?)
		    OR (SELECT COALESCE(max(similarity(t, ?)), 0) FROM jsonb_array_elements_text(s.tags::jsonb) t) > ?
		 ORDER BY score DESC, s.created_at DESC
		 LIMIT ?`,
		keyword, like, keyword,
		keyword, like, keyword,
		stickerSimilarityThreshold, limit,
	).Scan(&stickers).Error
	if err != nil {
		return nil, fmt.Errorf("database: search stickers %q: %w", keyword, err)
	}
	return stickers, nil
}

// ListStickers 按最新优先列出表情（无参 /发表情 用），limit 为条数上限。
func (db *DB) ListStickers(ctx context.Context, limit int) ([]model.StickerLibrary, error) {
	if db.Orm == nil {
		return nil, errors.New("database: orm is nil")
	}
	if limit <= 0 {
		limit = 20
	}
	var stickers []model.StickerLibrary
	err := db.Orm.WithContext(ctx).
		Order("created_at DESC").
		Limit(limit).
		Find(&stickers).Error
	if err != nil {
		return nil, fmt.Errorf("database: list stickers: %w", err)
	}
	return stickers, nil
}

// GetStickerByObjectKey 按对象键查询表情（幂等入库判重用）。
func (db *DB) GetStickerByObjectKey(ctx context.Context, objectKey string) (*model.StickerLibrary, error) {
	if db.Orm == nil {
		return nil, errors.New("database: orm is nil")
	}
	var sticker model.StickerLibrary
	err := db.Orm.WithContext(ctx).Where("object_key = ?", objectKey).First(&sticker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("database: get sticker by object key: %w", err)
	}
	return &sticker, nil
}

// DeleteStickersByTag 按单个标签精确删除表情记录，并返回被删除的记录。
// Tags 虽以 TEXT 保存，但内容始终为 JSON 字符串数组；转 jsonb 后用 @> 做数组包含判断，
// 不使用 ILIKE，避免删除“耍帅气”等仅包含关键词但标签并不相等的表情。
func (db *DB) DeleteStickersByTag(ctx context.Context, tag string) ([]model.StickerLibrary, error) {
	if db.Orm == nil {
		return nil, errors.New("database: orm is nil")
	}
	if tag == "" {
		return nil, nil
	}
	tagJSON, err := json.Marshal([]string{tag})
	if err != nil {
		return nil, fmt.Errorf("database: encode sticker tag %q: %w", tag, err)
	}

	var stickers []model.StickerLibrary
	err = db.Orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tags::jsonb @> ?::jsonb", string(tagJSON)).Find(&stickers).Error; err != nil {
			return err
		}
		if len(stickers) == 0 {
			return nil
		}
		ids := make([]uint, 0, len(stickers))
		for _, sticker := range stickers {
			ids = append(ids, sticker.ID)
		}
		return tx.Where("id IN ?", ids).Delete(&model.StickerLibrary{}).Error
	})
	if err != nil {
		return nil, fmt.Errorf("database: delete stickers by tag %q: %w", tag, err)
	}
	return stickers, nil
}

// IsMediaObjectReferenced 判断对象键是否仍被媒体缓存表引用。
// 表情记录删除后若这里返回 true，调用方必须保留 RustFS 对象。
func (db *DB) IsMediaObjectReferenced(ctx context.Context, objectKey string) (bool, error) {
	if db.Orm == nil {
		return false, errors.New("database: orm is nil")
	}
	var count int64
	if err := db.Orm.WithContext(ctx).
		Model(&model.MediaFile{}).
		Where("object_key = ?", objectKey).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("database: check media object reference %q: %w", objectKey, err)
	}
	return count > 0, nil
}
