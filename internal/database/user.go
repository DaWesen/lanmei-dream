package database

import (
	"context"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
	"gorm.io/gorm/clause"
)

// GetOrCreateUser 按 QQ 号查找或创建用户（使用 GORM clause.OnConflict 实现幂等 upsert）
func (db *DB) GetOrCreateUser(ctx context.Context, qqID int64, nickname string) (*model.User, error) {
	var u model.User
	result := db.Orm.WithContext(ctx).
		Where(model.User{QQID: qqID}).
		Attrs(model.User{Nickname: nickname}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "qq_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"nickname", "updated_at"}),
		}).
		FirstOrCreate(&u)
	if result.Error != nil {
		return nil, fmt.Errorf("get_or_create_user: %w", result.Error)
	}
	return &u, nil
}
