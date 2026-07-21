package database

import (
	"context"
	"fmt"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
    id         BIGSERIAL PRIMARY KEY,
    qq_id      BIGINT      UNIQUE NOT NULL,
    nickname   VARCHAR(255),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS conversations (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       VARCHAR(20) NOT NULL,
    content    TEXT        NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS memories (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content    TEXT        NOT NULL,
    metadata   JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_conversations_user_id    ON conversations(user_id);
CREATE INDEX IF NOT EXISTS idx_conversations_created   ON conversations(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memories_user_id        ON memories(user_id);
`

// Migrate 执行建表 SQL（幂等）
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
