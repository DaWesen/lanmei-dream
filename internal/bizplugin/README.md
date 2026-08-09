# bizplugin 包 —— 业务插件

## 职责

存放蓝妹的 Go 内置业务插件。每个插件实现 `pluginpkg.Plugin` 接口（`Info` / `OnInit` / `OnStart` / `OnStop`），在 `OnInit` 中注册 Pass、Pipeline、行为树 Subtree 到 Conduit 引擎，由 `cmd/lanmei/main.go` 统一注册。

## 插件列表

| 插件 | 文件 | 类型 | 说明 |
|------|------|------|------|
| `signin` | signin.go | 命令类 | 每日签到，通过斜杠命令触发 |
| `welcome` | welcome.go | **事件类（模范示例）** | 新人入群欢迎，消费 QQ 通知事件 |

`welcome` 是**消费 QQ 事件的模范示例**——事件类插件均参照其结构开发。下文完整说明从上游事件接入到下游插件消费的链路。

## QQ 事件类插件开发指南（从上游接入到下游消费）

### 全链路总览

```
OneBot 事件（入群 / 戳一戳 / ...）
  │  ① gateway 归一化（白名单映射，仅收支持的事件）
  ▼
NormalizedMessage（+ EventType / EventSubType / EventData）
  │  ② bot.OnMessage 分流，事件信息写入黑板 Extra
  ▼
引擎 Submit → 行为树
  │  ③ 插件子树（第一层，优先）← 事件类插件在这里消费
  │     （未被插件接住的事件会继续滑向后继分支直至意图分析——因此事件插件必须存在）
  ▼
插件管线 → AppendOutput → makeRichContentCallback → 网关发回群
```

### 1. 上游：事件接收通道（gateway 层，一般无需改动）

事件归一化已由 gateway 层完成，事件类插件**不需要**接触协议解析：

- `internal/gateway/notice.go`：事件白名单映射（`mapNoticeV12/V11`）与字段提取（`noticeEventDataV12/V11`）
- `internal/gateway/message.go`：`NormalizeV12/V11` 将通知事件解析为 `NormalizedMessage`（事件时 `Content` 为空、`EventType` 非空）
- **新增事件类型**只需在 `notice.go` 的映射表加常量与映射条目（如 `group_decrease`、`poke`），结构无需改动

### 2. 中游：事件写入黑板（bot 层，一般无需改动）

`bot.OnMessage` 将事件三字段写入引擎黑板 `Extra`，键定义于 `internal/bot/passes.go`：

| 键 | 类型 | 说明 |
|---|---|---|
| `bot.event.type` | string | 规范化事件类型，如 `group_increase`（空 = 普通消息）|
| `bot.event.sub_type` | string | 事件子类型，如 `approve` / `invite` |
| `bot.event.data` | map[string]any | 事件全字段（user_id / group_id / operator_id / sub_type 等）|

事件消息由富文本发送回调 `makeRichContentCallback` 处理：处理出错只记日志不回复；正常完成时若插件写入了富文本键 `bot.rich.content`（map：`text` / `at` / `image`，见 `internal/bot/passes.go` 的 `KeyRichContent`）则按富文本发送，否则遍历输出发送纯文本（插件的欢迎文案由此发出）。

### 3. 下游：插件消费（本包，以 welcome 为模范示例）

#### 3.1 结构（与 signin 模板一致）

```go
type WelcomePlugin struct { logger *zap.Logger }

func (p *WelcomePlugin) Info() pluginpkg.PluginInfo {
    return pluginpkg.PluginInfo{
        ID: "welcome", Name: "入群欢迎", Description: "新人入群时发送欢迎消息",
        Version: "1.0.0", SubtreeID: pluginpkg.SubtreeID("welcome"),
        // 事件类插件通常没有 Commands / Tools
    }
}

// OnInit 注册 Pass → Pipeline → Subtree，全部必须 Track
func (p *WelcomePlugin) OnInit(ctx *pluginpkg.PluginContext) error { ... }

func (p *WelcomePlugin) OnStart(*pluginpkg.PluginContext) error { return nil }
func (p *WelcomePlugin) OnStop(*pluginpkg.PluginContext) error  { return nil }
```

#### 3.2 子树条件：直接读黑板事件键（不导包）

事件类插件的子树条件判断"当前消息是否是我关心的事件"。按项目约定，插件**不依赖 bot / gateway 包**，直接以字符串键读取黑板 `Extra`：

```go
const eventKeyType = "bot.event.type" // 键定义见 internal/bot/passes.go

// isGroupIncreaseEvent 判断当前消息是否为新人入群事件
func isGroupIncreaseEvent(ctx *conduit.MessageContext) bool {
    eventType, _ := ctx.Extra[eventKeyType].(string)
    return eventType == "group_increase" // 规范化类型，见 gateway/notice.go
}
```

#### 3.3 子树与管线注册（仿 signin 三步）

```go
// 1. 注册 Pass（业务逻辑）
passID := pluginpkg.PassID("welcome", "welcome")
ctx.Engine.RegisterPass(passID, &welcomePass{logger: p.logger})
ctx.Registry.TrackPass("welcome", passID)

// 2. 注册动态管线（通过 Pass ID 引用，支持热替换）
pipelineID := pluginpkg.PipelineID("welcome", "main")
ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(pipelineID, passID))
ctx.Registry.TrackPipeline("welcome", pipelineID)

// 3. 注册行为树子树（事件路由）
subtree := conduit.NewSequence(
    conduit.NewCondition(isGroupIncreaseEvent),
    conduit.NewAction(pipelineID),
)
ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("welcome"), subtree)
```

#### 3.4 Pass：消费事件数据并输出

```go
func (pass *welcomePass) Execute(ctx *conduit.MessageContext) error {
    // 从事件全字段读取数据（模范示例：记录入群者 / 拉人者）
    eventData, _ := ctx.Extra["bot.event.data"].(map[string]any)
    pass.logger.Info("welcome: 新人入群",
        zap.Any("user_id", eventData["user_id"]),
        zap.Any("operator_id", eventData["operator_id"]),
        zap.Any("sub_type", eventData["sub_type"]),
        zap.String("group_id", ctx.GroupID),
    )

    // 回复统一写入 AppendOutput，由引擎回调统一发送
    conduit.AppendOutput(ctx, &conduit.Message{
        UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
        Content: welcomeMessages[rand.IntN(len(welcomeMessages))],
    })
    return nil
}
```

**为什么事件必须在插件子树消费**：插件子树挂在行为树 Selector 最前（优先级最高）。事件被插件接住后不会滑向后继分支——否则会落入意图分析，用空文本调 LLM 产生无关回复。

### 4. 注册（cmd/lanmei/main.go）

内置业务插件由 `BusinessRegistry`（见 `internal/bizplugin/registry.go`）按配置开关统一注册，替代逐插件硬编码 if 块：

```go
bizReg := bizplugin.NewBusinessRegistry(&cfg.Plugin.Builtins, pluginReg, inf.DB, logger)
if err := bizReg.RegisterBuiltins(); err != nil {
    logger.Fatal("内置业务插件注册失败", zap.Error(err))
}
```

`[plugin.builtins]` 配置节控制各插件启停（`signin` / `welcome`），改配置即可生效、无需改代码。注册表在注册前先查 Registry 是否已被同名 wasm 插件占用（wasm 优先，避免 ID 冲突）；插件生命周期（Init/Start/Stop）统一由 Registry 管理。

新增内置插件步骤：实现 `Plugin` 接口 → 在 `registry.go` 的 `RegisterBuiltins` 追加注册分支（含配置开关）→ 在 `config.go` 的 `PluginBuiltinsConfig` 加开关字段。

### 5. 本地验证方法

1. 起依赖：`docker compose up -d postgres redis`（等 PG healthy）
2. 启动蓝妹（环境变量直接 export，配置不读 `.env` 文件）：

```bash
export LANMEI_DATABASE_URL='postgres://lanmei:lanmei@localhost:5432/lanmei?sslmode=disable'
export LANMEI_REDIS_ADDR='localhost:6379'
export LANMEI_BOT_GATEWAY_LISTEN_ADDR='127.0.0.1:8080'
go run ./cmd/lanmei
```

3. 用 WebSocket 客户端（Node ≥22 原生 `WebSocket` 即可）连接 `ws://127.0.0.1:8080/onebot/v12`（或 `/onebot/v11`），模拟 OneBot 实现推送事件，观察回发动作：

```json
// v12 入群事件（detail_type 按 OneBot 12 规范）
{"id":"evt-1","impl":"test","platform":"qq","self_id":"11111","time":1754700000,
 "type":"notice","detail_type":"group_member_increase","sub_type":"invite",
 "user_id":"22222","group_id":"33333","operator_id":"44444"}
```

4. 预期：收到 `send_message` 动作、消息为欢迎文案；插件日志出现 `welcome: 新人入群`。验证完 `docker compose down` 清理。

## 硬性约定

- 插件结构、注册顺序、Track 清理等规范详见 [PLUGIN_DEVELOPMENT.md](../../PLUGIN_DEVELOPMENT.md)（Go 内置插件章节）
- 回复统一 `conduit.AppendOutput`，Pass 内禁止直接发送
- 跨 Pass 传数据用 `conduit.Set/Get`，key 带插件前缀（如 `plugin.welcome.xxx`）
- 事件键直接使用字符串字面量（键定义见 `internal/bot/passes.go`），**不 import bot / gateway 包**

## 关键依赖

- `github.com/zrurf/conduit` — 引擎、行为树、管线（`MessageContext.Extra` 即黑板）
- `internal/gateway` — 事件归一化与事件键契约（`notice.go`、`message.go`）
- `internal/plugin` — 插件接口与资源注册（`PluginContext` / `PassID` / `PipelineID` / `StoreKey`）
- `internal/bot` — 事件写入黑板（`OnMessage` 分流、`makeRichContentCallback`、事件键定义）
