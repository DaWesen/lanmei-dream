package model

import "time"

// User 对应 users 表
type User struct {
	ID        int64     `json:"id"          db:"id"`
	QQID      int64     `json:"qq_id"       db:"qq_id"`
	Nickname  string    `json:"nickname"    db:"nickname"`
	CreatedAt time.Time `json:"created_at"  db:"created_at"`
	UpdatedAt time.Time `json:"updated_at"  db:"updated_at"`
}
