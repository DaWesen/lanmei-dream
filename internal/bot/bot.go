package bot

import (
	"strings"

	"github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/driver"

	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/plugin/roleplay"
)

// BotConfig 是 Bot 的配置
type BotConfig struct {
	WebSocketURL string // LLOneBot 的 WebSocket 地址
	AccessToken  string // 鉴权 token（可为空）
	NickName     string // 机器人昵称
	SuperUsers   []int64
}

// Bot 封装 ZeroBot 的初始化与消息分发
type Bot struct {
	cfg      *zero.Config
	cmdSys   *command.System
	roleplay *roleplay.Plugin
}

// New 创建 Bot 实例
func New(cfg *BotConfig, cmdSys *command.System, rp *roleplay.Plugin) *Bot {
	nick := cfg.NickName
	if nick == "" {
		nick = "蓝妹"
	}

	zeroCfg := &zero.Config{
		NickName:      []string{nick},
		CommandPrefix: "/",
		SuperUsers:    cfg.SuperUsers,
		Driver: []zero.Driver{
			driver.NewWebSocketClient(cfg.WebSocketURL, cfg.AccessToken),
		},
	}

	return &Bot{
		cfg:      zeroCfg,
		cmdSys:   cmdSys,
		roleplay: rp,
	}
}

// Run 注册消息处理并阻塞运行
func (b *Bot) Run() {
	zero.OnMessage().Handle(b.handleMessage)
	zero.RunAndBlock(b.cfg, nil)
}

// handleMessage 消息分流：/ 开头走命令系统，其余走角色扮演
func (b *Bot) handleMessage(ctx *zero.Ctx) {
	msg := ctx.Event.RawMessage
	if msg == "" {
		msg = ctx.ExtractPlainText()
	}

	if strings.HasPrefix(msg, "/") {
		b.cmdSys.Process(msg, &command.Context{
			UserID:  ctx.Event.UserID,
			Message: msg,
			Reply:   func(s string) { ctx.Send(s) },
		})
	} else if b.roleplay != nil {
		b.roleplay.Handle(ctx)
	}
}
