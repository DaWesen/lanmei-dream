# 蓝妹 (lanmei-dream)

> 蓝妹做了一个梦...梦里她考上了3G大学(🫵🤓)
> 梦醒之后，她决定留在蓝山工作室，当大家的吉祥物兼话痨担当 (≧▽≦)

## 我是谁

你好呀，我是蓝妹，重庆邮电大学蓝山工作室的虚拟吉祥物，这个仓库就是「我本人」——一个跨平台 AI 聊天机器人。

我平时就住在 IM 群里：微信、钉钉、Telegram 都能接（走 Onebots 网关），QQ 也能直连（NapCat）。大家发消息过来，我先是靠「行为树 + 管线」的 Conduit 引擎判断该干嘛——是执行命令、陪你聊天、翻知识库，还是调个工具把活儿干了。聊天的时候我会记住你的偏好（LOD 三级记忆），记不清的就去查知识库（RAG 检索），隔一段时间还会把旧对话压缩成摘要，省点脑容量。

顺便说一句：我嘴硬心软、爱吐槽，聊到二次元、galgame、技术梗会突然来劲。要是有人提「3G大学」，那可说到我心坎里了 (￣ω￣)

## 我会什么

- **跨平台网关**：反向 WS 服务端（lxzan/gws），OneBot 12/11 自动适配；用户按 `(platform, platform_user_id)` 区分，换平台不串台
- **AI 角色扮演**：基于 CloudWeGo Eino，LLM / Embedding 随便换——OpenAI、DeepSeek、Qwen、Moonshot、火山方舟都行，还支持 doubao-embedding-vision 多模态向量化
- **记性好**：L0 原文 → L1 摘要（EpisodeSummary）→ L2 主题（TopicCluster + pgvector 向量），LLM 驱动压缩，按 token 预算拼上下文
- **RAG 检索**：向量 + 模糊 + 时间加权召回；知识库支持本地 Markdown / CSV 规则表，也接飞书 Wiki / 表格
- **意图分析**：自然语言先让 LLM 猜我想干嘛（command / tool / chat / ignore），所以直接说「帮我签到」也能听懂
- **群聊话题系统**：不是每条都理你，只在被提及或话题相关时回，免得刷屏惹人嫌
- **技能系统**：梅花易数占卜、猜词之类的小技能，动态加载
- **插件系统**：内置业务插件 + Wasm 插件（Extism），支持远程安装；Casbin RBAC 权限、资源配额、审计日志一应俱全，规矩立得明明白白
- **流式回复**：模拟真人打字节奏分段发送，不会一股脑倒出来吓到人
- **看得懂图**：可选多模态模型给图片写描述
- **富媒体**：RustFS（S3 兼容）对象存储缓存图 / 语音；网易云点歌能发语音、卡片或链接
- **内容插件**：编程答题游戏（题库外部化、多语言、热更新）、每日一句、海龟汤、表情库收藏 / 管理 / 语义发送
- **管理面板**：Fiber API + Vue 3 / TDesign 前端——能管 LLM Provider、热更新行为树 / 管线、看实时 Trace、查计费、翻审计、改表情包……

## 技术栈

别看我满脑子二次元，底子还是挺硬的：

| 层 | 技术 |
|---|---|
| 语言 | Go 1.26 + Vue 3 (TS / Vite / TDesign) |
| 消息引擎 | [github.com/zrurf/conduit](https://github.com/zrurf/conduit)（行为树 + 管线） |
| 网关 | lxzan/gws 反向 WebSocket，OneBot 12/11 |
| AI | CloudWeGo Eino（LLM / Embedding / ToolCalling） |
| 数据库 | PostgreSQL 18 + pgvector（HNSW 索引，GORM 参数化查询） |
| 缓存 | Redis 7（Conduit StateStore + 插件 KV） |
| 插件 | Extism + wazero（Wasm） |
| 权限 | Casbin RBAC |
| 管理 API | gofiber/fiber v3 |
| 对象存储 | RustFS（S3 兼容） |
| 日志 | zap + lumberjack（轮转） |
| 部署 | Docker Compose（Postgres / Redis / RustFS / ncm-api / Onebots / NapCat / 管理前端） |

## 快速开始

### 前置

Docker + Docker Compose 就行，别的我不挑。

### 一键启动（全栈）

```bash
cp .env.example .env   # 填 API Key 和管理面板凭据
make up                # 等价于 docker compose up -d --build
```

起来之后：

- 我的网关：`ws://<host>:8080/onebot/v12`（v11 也兼容：`/onebot/v11`，NapCat 加 `?platform=napcat`）
- 管理面板：`http://<host>:80`（nginx 托管前端并反代 `/api` 到后端 `:8090`）

常用命令（详见 `make help`）：

```bash
make logs     # 看我日志
make restart  # 重新叫醒我
make down     # 让我睡会儿
```

### 本地开发

本机要有 Go / Node.js，依赖容器：`docker compose up -d postgres redis rustfs ncm-api`。

```bash
make dev-server  # 后端（管理面板 :8090）
make dev-web     # 管理面板前端（vite :5173，/api 代理到 :8090）
```

## 配置

配置分两层：敏感信息一律走环境变量（前缀 `LANMEI_`），非敏感配置放在 `config/config.toml`。

- **环境变量**：数据库 / Redis 连接、网关监听与 token、LLM / Embedding API、管理面板凭据与加密密钥、知识库开关、网易云 API 地址等，模板见 [.env.example](.env.example)
- **config.toml**：我的昵称与超管、网关监听、流式回复节奏、插件与内置插件开关、管理面板参数、Prompt / Skill / 知识库路径与 `bases` 列表、编程答题题库目录（`[quiz] dir`）
- **skills.toml**：技能运行时启停

关键环境变量：

| 变量 | 说明 |
|---|---|
| `LANMEI_DATABASE_URL` | PostgreSQL 连接串（含 pgvector） |
| `LANMEI_REDIS_ADDR` | Redis 地址（Conduit StateStore） |
| `LANMEI_BOT_GATEWAY_LISTEN_ADDR` / `LANMEI_BOT_GATEWAY_ACCESS_TOKEN` | 反向 WS 网关监听地址 / 鉴权 token |
| `LANMEI_BOT_SUPER_USERS` | 超级用户，格式 `platform:userID`，逗号分隔 |
| `LANMEI_AI_LLM_API_KEY` / `LANMEI_AI_EMBEDDING_API_KEY` | LLM / Embedding API Key（填了角色扮演和 RAG 才自动启用） |
| `LANMEI_PLUGIN_NCM_URL` | 网易云点歌 API 地址 |
| `LANMEI_MANAGER_ADMIN_USERNAME` / `_PASSWORD` / `LANMEI_MANAGER_SECRET_KEY` | 管理面板超管凭据与 JWT 派生密钥（仅环境变量，不落盘） |
| `LANMEI_KNOWLEDGE_ENABLED` | 知识库总开关 |

## 管理面板

把 `config.toml` 的 `[manager] enabled = true` 打开、配好环境变量凭据，就能进后台「调教」我了（小声）。功能包括：

- **认证**：账号密码 + TOTP 两步验证 + WebAuthn passkey；超管 / 管理员角色、Step-Up 二次确认、会话管理、登录限流与锁定
- **LLM**：Provider 热切换与价格表、用量统计图表
- **Conduit 控制平面**：查看行为树 / 管线快照、热更新与回滚、消息实时 Trace（SSE）、流量统计
- **审计**：登录 / 敏感操作审计日志
- **内容管理**：群组配置、用户拉黑、知识库（同步 / 删除分块）、记忆、插件启停、技能启停、Prompt 片段编辑、表情库、命令列表
- **仪表盘**：全局统计

## 内置插件与命令

我平时会的活儿都在这了，`[plugin.builtins]` 控制启停（wasm 同名插件优先）：

| 插件 | 说明 |
|---|---|
| signin | 每日签到 + 积分排行榜（早起打卡有我） |
| welcome | 新人入群欢迎（事件类插件模范示例） |
| poke | 戳一戳回复（别老戳我！(˃ ⌑ ˂ഃ )） |
| three_g | 3G 关键词科普（懂的都懂，重邮 3G） |
| cat / balogo / github_card / ping | 猫图 / 蔚蓝档案 LOGO / GitHub 链接卡片 / 连通性测试 |
| music | 网易云点歌（语音 / 卡片 / 链接） |
| sticker | 表情库：`/添加表情`、`/删除表情`（管理员）、`/发表情`（无参随机）、`/表情列表`；LLM 语义触发 + 周期表情注入 |
| turtle_soup | 海龟汤文字游戏（异步出题 / 判定，独立 LLM 超时） |
| answer_question | 编程答题：`/答题 [语言] [难度]`，题库在 quizdata/，多语言热更新 |
| daily_quote | 每日一句：`/每日一句`（一言 + 出处作者） |

命令举例：`/签到`、`/帮助`（`/help` 也行）、`/答题 go python 困难`、`/每日一句`、`/删除表情 <标签>`、`/插件 安装 <url>`（远程装 Wasm 插件）。自然语言也能触发命令（意图分析，连 at 段都能保留）。

## 插件开发

想给我加新活儿？看这几个地方：

- 内置业务插件开发规范 + 事件类插件模范示例：见 [internal/bizplugin/README.md](internal/bizplugin/README.md)
- Wasm 插件开发（WIT 接口、权限、生命周期、ABI）：见 [PLUGIN_DEVELOPMENT.md](PLUGIN_DEVELOPMENT.md) 与 [schema/plugin/lanmei-plugin.wit](schema/plugin/lanmei-plugin.wit)
- 插件示例：`examples/plugin/signin/`

## 目录结构

我的身体结构大致长这样：

```
cmd/lanmei/          → 入口 main.go（组装全部模块）
internal/
  ai/                → ChatService、意图分析、LLM/Embedding 客户端、LOD 压缩、Prompt/Skill/Tool 系统
  bot/               → Conduit 引擎 + 行为树 + Pass 实现 + 事件分流
  command/           → 斜杠命令系统
  config/            → 配置加载（viper + toml + 环境变量）
  database/          → GORM 连接池、迁移、CRUD
  gateway/           → 反向 WS 服务端 + OneBot 12/11 协议适配
  infra/             → 基础设施统一管理（PG + Redis + pgvector + 对象存储 + Logger）
  kb/                → 知识库系统（Provider 抽象 + 多路召回 + local/feishu/sheet）
  manager/           → 管理面板（Fiber API + 认证 + Conduit 控制平面 + 审计 + 计费）
  media/             → 多媒体 MIME 与对象存储
  model/             → 数据模型
  plugin/            → 插件系统（注册表 + Wasm 运行时 + 权限控制 + KV 存储）
  topic/             → 群聊话题系统（语义/提及决策 + 归档）
  bizplugin/         → 内置业务插件
manager/             → 管理面板前端（Vue 3 + TDesign + Vite）
config/              → 运行时配置（config.toml / skills.toml / onebots / napcat）
prompts/             → 模块化提示词（system_persona / assembly_template 等）
skills/              → 技能目录（divination / word_guess 等）
quizdata/            → 编程答题题库（按语言分目录，JSON 外部化，热更新）
schema/plugin/       → Wasm 插件 WIT 接口定义
docs/                → 架构图与数据流文档
```

## 相关文档

- [架构图（drawio + mermaid）](docs/architecture.md) — 系统总览、消息流程、插件安全模型
- [数据流说明](docs/data-flow.md) — 消息 / 意图 / 工具调用 / 插件 / 记忆压缩全链路
- [插件开发规范](PLUGIN_DEVELOPMENT.md)
- [项目规则（AI 助手）](CLAUDE.md)

## 变更主线（近期）

- 2026-08（后续）：编程答题游戏（`/答题`，题库外部化至 quizdata/ 并支持多语言热更新）；每日一句插件（`/每日一句`）；表情库新增 `/删除表情`（管理员）；命令系统支持回传 at 段与禁止自动 @；海龟汤提示词调整
- 2026-08：海龟汤异步出题 / 判定与独立超时；意图分析独立超时；知识库 CSV 规则表摄入 + 火山方舟多模态向量化；工具调用循环注入调用者身份；表情库（`/表情列表`、随机表情、周期表情注入）；管理面板前后端
- 2026-07：知识库系统与多路召回；群聊话题系统；事件类插件（入群欢迎 / 戳一戳）；Prompt 模块化与技能系统；Eino 集成；跨平台 OneBot 网关重构；插件系统与 Wasm 支持

## License

内部项目，暂未指定开源协议——等哪天蓝妹愿意开源再说吧 (￣ω￣;)
