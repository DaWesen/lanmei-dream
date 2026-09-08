# bizplugin 包 —— 业务插件

## 职责

存放蓝妹的 Go 内置业务插件。每个插件实现 `pluginpkg.Plugin` 接口（`Info` / `OnInit` / `OnStart` / `OnStop`），在 `OnInit` 中注册 Pass、Pipeline、行为树 Subtree 到 Conduit 引擎，由 `cmd/lanmei/main.go` 统一注册。

## 插件列表

| 插件 | 文件 | 类型 | 说明 |
|------|------|------|------|
| `signin` | signin.go | 命令类 | 每日签到，通过斜杠命令触发 |
| `welcome` | welcome.go | **事件类（模范示例）** | 新人入群欢迎（@新人 + 文案 + 图片，走出站段通道） |
| `poke` | poke.go | **事件类** | 戳一戳回复（@戳人者 + 随机文案，走出站段通道） |
| `three_g` | three_g.go | **关键词触发类** | 消息含 3G/3g 时科普重邮 3G（@对方 + 固定文本，走出站段通道） |
| `random_beauty` | random_beauty/ | **外部 API 命令类** | `/随机美图` 获取 Pixiv 插画；固定分级、元数据、文件与视觉四层审核，审核失败时拒绝发送 |

`welcome` 是**消费 QQ 事件的模范示例**——事件类插件均参照其结构开发。下文完整说明从上游事件接入到下游插件消费的链路。

`random_beauty` 是外部图片 API 插件的参考实现：只接受同源相对图片路径，限制重定向、响应大小、MIME 和实际像素尺寸，并确保视觉审核与最终发送使用同一份字节。普通单元测试使用 `httptest.Server`，不依赖第三方网络。

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
插件管线 → conduit.Set 出站段键（或 AppendOutput 纯文本）
  → makeEventCallback → hub.SendSegments → 网关发回群
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

事件消息由事件专用回调 `makeEventCallback` 处理：处理出错只记日志不回复；正常完成时发送管线输出（出站段优先，纯文本兜底，插件的欢迎消息由此发出）。

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

    content := welcomeMessage
    newUserID, _ := eventData["user_id"].(string)

    // 缺 user_id（异常事件）：降级纯文本欢迎语，不 @ 任何人
    if newUserID == "" {
        conduit.AppendOutput(ctx, &conduit.Message{
            UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
            Content: content,
        })
        return nil
    }

    // 出站段通道：[@新人 + 固定文案 + 固定图片] 一条消息
    conduit.Set(ctx, "bot.send.segments", []map[string]any{
        {"type": "at",    "data": map[string]any{"user_id": newUserID}},
        {"type": "text",  "data": map[string]any{"text": content}},
        {"type": "image", "data": map[string]any{"file": welcomeImageURL}},
    })
    return nil
}
```

**出站段通道（富文本发送）**：普通文本用 `AppendOutput`；需要富媒体（at / image 组合）时用 `conduit.Set(ctx, "bot.send.segments", []map[string]any{...})` 写入 OneBot 原生段列表，由 bot 回调读取后按段发送（v11/v12 协议差异自动收敛，见 spec `lanmei-rich-send-spec.md` 方案 C）。规则：

- 键 `"bot.send.segments"`，值 `[]map[string]any`，每段 `{"type": "...", "data": {...}}`，**顺序即发送顺序**
- 永远按 OneBot 12 语义组装（at 段用 `user_id`），插件不感知协议
- 段键存在（且非空）时回调只发段、忽略 Output；无键 / 类型不符 / 空列表时自动降级纯文本
- 纯文本场景继续用 `AppendOutput`，两种方式互不冲突

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
- 回复统一 `conduit.AppendOutput`（纯文本）或 `conduit.Set("bot.send.segments", 段列表)`（富文本），Pass 内禁止直接发送
- 跨 Pass 传数据用 `conduit.Set/Get`，key 带插件前缀（如 `plugin.welcome.xxx`）
- 事件键直接使用字符串字面量（键定义见 `internal/bot/passes.go`），**不 import bot / gateway 包**

## 关键依赖

- `github.com/zrurf/conduit` — 引擎、行为树、管线（`MessageContext.Extra` 即黑板）
- `internal/gateway` — 事件归一化与事件键契约（`notice.go`、`message.go`）
- `internal/plugin` — 插件接口与资源注册（`PluginContext` / `PassID` / `PipelineID` / `StoreKey`）
- `internal/bot` — 事件写入黑板（`OnMessage` 分流、`makeEventCallback`、事件键定义）
