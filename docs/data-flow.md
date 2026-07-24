# 数据流说明

## 消息处理流

```
用户消息 → QQ/LLOneBot → WebSocket → bot.handleMessage
  │
  ├─ 转换为 conduit.InputMessage
  └─ engine.Process(input) → 同步拿到结果
       │
       ├─ 行为树决策
       │    ├─ /admin → pipeline.admin
       │    ├─ /xxx   → pipeline.command
       │    └─ 其余   → pipeline.chat
       │
       ├─ 执行管线 Pass
       │    ├─ CommandPass → command.System.Process → Handler
       │    ├─ RoleplayPass → ChatService.Chat → 回复
       │    └─ FallbackPass → 兜底回复
       │
       └─ OnResponse 回调（输出消息/记录错误）
```

## Conduit 引擎配置

```
Engine
  ├─ Workers: 4
  ├─ Timeout: 10s
  ├─ Fallback: pipeline.fallback
  ├─ StateStore: MemoryStore（内存）
  └─ OnResponse: 日志记录
```

## 记忆层

```
LOD 三级压缩架构：

L0 原始对话 (PostgreSQL conversations)
  ├── 每条消息原文保存
  └── 超过 40 条 → 触发压缩到 L1

L1 Episode Summary (PostgreSQL episode_summaries)
  ├── brief: 一句话总结（≤50字）
  ├── detailed: 详细摘要（≤200字），保留关键事实/情感/决策
  ├── facts: 结构化事实列表（JSONB），如 "用户喜欢猫"
  └── 超过 10 条 → 触发聚合到 L2

L2 Topic Cluster (PostgreSQL topic_clusters + Milvus)
  ├── topic: 主题名称（LLM 生成，如"宠物话题"）
  ├── brief: 一句话主题总结
  ├── detailed: 详细主题描述
  ├── facts: 聚合后的关键事实
  └── 向量化存入 Milvus，支持语义检索
```

## 压缩流程

```
用户发消息 → RoleplayPass → ChatService.Chat
                                    │
                                    ├── 1. LOD 上下文组装（3000 token 预算）
                                    │     L2 主题概览 → L1 摘要 → L0 原文
                                    │     按 token 预算优先级填充
                                    │
                                    ├── 2. RAG 检索（Milvus 向量相似度）
                                    │
                                    ├── 3. 拼提示 → LLM → 回复
                                    │
                                    ├── 4. 异步存记忆（Milvus）
                                    │
                                    └── 5. 异步压缩（Compressor.MaybeCompress）
                                           ├── L0≥40 → LLM压缩 → L1 EpisodeSummary → 删原文
                                           └── L1≥10 → LLM聚合 → L2 TopicCluster → 向量化 → 删旧摘要
```

## 上下文组装示例

```
System: 蓝妹系统提示词
System: 历史话题概览：
        宠物话题: 用户养了一只布偶猫叫小雪
        健康咨询: 用户提到贫血，看了医生
System: 过往对话摘要：
        用户问了布偶猫的饮食注意事项，推荐了皇家猫粮
        用户去医院检查，医生说轻度贫血建议补铁
User:   小雪最近不太爱吃东西
Assist: 可能是换季影响了食欲，试试...
User:   她今天连罐头都不吃了
```

## 管线一览

| 管线 ID | Pass 链 | 触发条件 |
|---------|---------|----------|
| pipeline.admin | CommandPass | `/admin` 开头 |
| pipeline.command | CommandPass | `/` 开头 |
| pipeline.chat | RoleplayPass | 其余（自然语言） |
| pipeline.fallback | FallbackPass | 超时降级 |
