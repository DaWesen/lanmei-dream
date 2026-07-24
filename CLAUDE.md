# CLAUDE.md — 项目规则（AI 助手必读）

## 项目概况

蓝妹（lanmei-dream）是一个基于 ZeroBot 框架的 QQ 聊天机器人，通过 LLOneBot 的 WebSocket 协议与 QQ 通信。消息路由使用 Conduit（行为树 + 管线架构）。

## 技术栈

- **语言**: Go
- **Bot 框架**: github.com/wdvxdr1123/ZeroBot
- **消息引擎**: github.com/zrurf/conduit（行为树 + 管线）
- **数据库**: PostgreSQL（GORM ORM + pgx 驱动，参数化查询防注入）
- **向量数据库**: Milvus（RAG 长期记忆）
- **通信**: LLOneBot WebSocket（driver.NewWebSocketClient）
- **LLM / Embedding**: 待定（接口已定义在 internal/ai/llm/ 和 internal/ai/embedding/，具体实现等用户指定）

## 目录结构

```
cmd/lanmei/          → 入口 main.go
internal/
  ai/                → ChatService、Compressor、提示词
    llm/             → LLMClient 接口 + ChatRequest/ChatResponse/Role/Message
    embedding/       → Embedder 接口
    memory/          → MemoryStore 接口 + MilvusMemoryStore 实现
  bot/               → ZeroBot 初始化 + Conduit 引擎 + 行为树 + Pass 实现
  command/           → 斜杠命令系统（注册/解析/分发）
  database/          → GORM 连接池、迁移、CRUD（user/conversation/lod），所有查询参数化
  model/             → 数据模型（User/Conversation/Memory/EpisodeSummary/TopicCluster）
  signin/            → 签到插件
docs/                → 架构图（drawio + markdown）
```

## 约定

- **语言**: 所有代码注释、对话、commit message 用中文
- **数据库表名/字段**: 英文蛇形（snake_case）
- **Go 导出符号**: 大驼峰
- **错误处理**: fmt.Errorf 带 %w 包装，log.Printf 记录，不吞错
- **SQL 安全**: GORM 参数化查询，绝不拼接用户输入到 SQL 字符串
- **依赖注入**: 构造函数注入（NewXxx），不用全局变量
- **消息路由**: 行为树决策 → 管线执行，不写 if-else 面条
- **新增功能**: 加 Pass 实现 conduit.Pass 接口，注册到管线
- **不要擅自添加基础设施**（Redis、消息队列等），等用户明确指示再加

## 关键设计决策

- 消息路由由 Conduit 引擎驱动：行为树决策走哪条管线，管线内 Pass 链式执行
- 行为树优先级：管理员命令 > 普通命令 > 角色扮演（兜底）
- 四条管线：pipeline.admin / pipeline.command / pipeline.chat / pipeline.fallback
- 角色扮演流程：LOD 多级上下文组装 → RAG 检索记忆 → 拼提示 → LLM → 存对话+记忆 → 异步压缩
- LOD 记忆压缩：L0（原始对话）→ L1（EpisodeSummary: brief+detailed+facts）→ L2（TopicCluster: brief+detailed+facts+Milvus向量）
- 压缩由 LLM 驱动：读旧对话/摘要 → 生成 brief+detailed+结构化事实 → 替换原文
- 压缩阈值：L0≥40条触发L0→L1，L1≥10条触发L1→L2
- 上下文组装按 token 预算分配：L2→L1→L0 优先级填充
- LLMClient / Embedder 是接口，无具体实现，角色扮演因此暂不可用
- 命令系统计划支持 Function Calling 自然语言意图识别（等 LLM provider 确定后实现）
- 记忆层：LOD 三级压缩（L0 原文→L1 摘要→L2 主题），PostgreSQL + Milvus 协同

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| DATABASE_URL | postgres://postgres:postgres@localhost:5432/lanmei?sslmode=disable | PostgreSQL 连接串 |
| MILVUS_ADDR | localhost:19530 | Milvus 地址 |
| MILVUS_COLLECTION | lanmei_memories | Milvus 集合名 |
| EMBEDDING_DIM | 1024 | 向量维度 |
| WS_URL | ws://127.0.0.1:3001 | LLOneBot WebSocket 地址 |
| ACCESS_TOKEN | （空） | 鉴权 token |
| BOT_NICKNAME | 蓝妹 | 机器人昵称 |
| SUPER_USERS | （空） | 超级管理员 QQ 号，逗号分隔 |

> **本文档状态：当前有效** — 架构变更时须同步更新此文件。

## 变更记录

- 2026-07-22：数据库层从 pgxpool 迁移到 GORM，参数化查询防注入，AutoMigrate 替代原始 SQL
- 2026-07-22：拆包重构：ai/ → ai/ + ai/llm/ + ai/embedding/ + ai/memory/；database/ 拆分为 user/conversation/lod；plugin/signin → signin/；删除空 plugin/ 目录
- 2026-07-22：引入 LOD 记忆压缩系统（L0→L1→L2），LLM 驱动 brief/detailed/facts 双粒度压缩，token 预算上下文组装
- 2026-07-22：引入 Conduit 消息引擎（行为树 + 管线），替代 if-else 路由；roleplay 逻辑合并进 RoleplayPass
- 2026-07-22：初始版本，PostgreSQL + Milvus + ZeroBot，LLM/Embedding 待定
