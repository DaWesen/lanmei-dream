package database

import (
	"context"
	"fmt"

	"github.com/DaWesen/lanmei-dream/internal/model"
)

// Migrate 使用 GORM AutoMigrate 自动建表（幂等）
func (db *DB) Migrate(ctx context.Context) error {
	if err := db.Orm.WithContext(ctx).AutoMigrate(
		&model.User{},
		&model.Conversation{},
		&model.Memory{},
		&model.EpisodeSummary{},
		&model.TopicCluster{},
	); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
