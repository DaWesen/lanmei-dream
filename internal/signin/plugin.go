package signin

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/database"
)

// Plugin 签到插件
type Plugin struct {
	db *database.DB
}

// New 创建签到插件
func New(db *database.DB) *Plugin {
	return &Plugin{db: db}
}

// HandleSignin 处理 /签到 命令
func (p *Plugin) HandleSignin(ctx *command.Context) error {
	bgCtx := context.Background()

	// 确保用户存在
	user, err := p.db.GetOrCreateUser(bgCtx, ctx.UserID, "")
	if err != nil {
		log.Printf("signin: get_or_create_user: %v", err)
		ctx.Reply("签到失败，请稍后再试。")
		return fmt.Errorf("get_or_create_user: %w", err)
	}

	// TODO: 检查今天是否已签到（需要签到记录表，等用户确认后再加）
	// 目前先做最简实现：直接返回签到成功

	now := time.Now()
	ctx.Reply(fmt.Sprintf("✅ 签到成功！\n用户ID: %d\n时间: %s",
		user.ID,
		now.Format("2006-01-02 15:04:05"),
	))

	return nil
}
