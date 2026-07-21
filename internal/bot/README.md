# bot 包 —— ZeroBot 消息处理层

## 职责

负责 ZeroBot 的初始化和消息分发，是整个程序的入口对接层。

## 核心设计

### 消息分流

所有用户消息从这里进入，按规则分发：

```
ZeroBot 收到消息
        │
        ▼
   bot/handler.go
        │
        ├─ 以 / 开头 → command.System.Process()
        │
        └─ 普通文本 → plugin/roleplay.Handle()
```

### 初始化流程

```go
// bot/bot.go
type Bot struct {
    cfg     *zero.Config        // ZeroBot 配置
    cmdSys  *command.System     // 命令系统
    roleplay *roleplay.Plugin   // 角色扮演插件
}

func New(cfg *BotConfig, cmdSys *command.System, rp *roleplay.Plugin) *Bot {
    return &Bot{
        cfg:      buildZeroConfig(cfg),
        cmdSys:   cmdSys,
        roleplay: rp,
    }
}

func (b *Bot) Run() {
    // 注册消息处理
    zero.OnMessage().Handle(b.handleMessage)
    // 启动
    zero.RunAndBlock(b.cfg, nil)
}
```

### Handler 逻辑

```go
func (b *Bot) handleMessage(ctx *zero.Ctx) {
    msg := ctx.Event.RawMessage

    if strings.HasPrefix(msg, "/") {
        // 走命令系统
        b.cmdSys.Process(msg, &command.Context{
            UserID:  ctx.Event.UserID,
            Message: msg,
            Reply:   func(s string) { ctx.Send(s) },
        })
    } else {
        // 走角色扮演
        b.roleplay.Handle(ctx)
    }
}
```
