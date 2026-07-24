# 蓝妹架构图

## 系统总览

```mermaid
graph TB
    subgraph 外部
        QQ[QQ 客户端]
        LLOneBot[LLOneBot]
    end

    subgraph 蓝妹服务
        Main[main.go 入口]
        Bot[bot.Bot]
        Engine[Conduit Engine]
        BT[行为树]
        CmdSys[command.System]
    end

    subgraph 管线 Pass
        CmdPass[CommandPass]
        RPPass[RoleplayPass]
        FBPass[FallbackPass]
    end

    subgraph AI 层
        ChatSvc[ai.ChatService]
        Comp[ai.Compressor<br/>LOD 压缩]
        LLM[ai.LLMClient<br/>⚠ 待实现]
        Emb[ai.Embedder<br/>⚠ 待实现]
        MemStore[ai.MilvusMemoryStore]
    end

    subgraph 存储层
        PG[(PostgreSQL)]
        MV[(Milvus)]
    end

    QQ -->|消息| LLOneBot
    LLOneBot -->|WebSocket| Bot
    Bot -->|Process| Engine
    Engine -->|决策| BT
    BT -->|/admin| CmdPass
    BT -->|/命令| CmdPass
    BT -->|其余| RPPass
    BT -->|超时/兜底| FBPass
    CmdPass --> CmdSys
    RPPass --> ChatSvc
    ChatSvc --> LLM
    ChatSvc --> Emb
    ChatSvc --> MemStore
    ChatSvc --> Comp
    Comp --> LLM
    Comp --> Emb
    Comp --> MemStore
    RPPass --> PG
    CmdPass --> PG
    MemStore --> MV
    ChatSvc -.->|存对话| PG
```

## 消息处理流程

```mermaid
sequenceDiagram
    participant U as 用户
    participant Q as QQ/LLOneBot
    participant B as bot.Bot
    participant E as Conduit Engine
    participant BT as 行为树
    participant CP as CommandPass
    participant RP as RoleplayPass
    participant FB as FallbackPass
    participant A as ai.ChatService
    participant P as PostgreSQL
    participant M as Milvus

    U->>Q: 发消息
    Q->>B: WebSocket 推送
    B->>E: Process(InputMessage)

    E->>BT: Tick(MessageContext)

    alt /admin 开头
        BT-->>E: pipeline.admin
        E->>CP: Execute
        CP->>P: 操作数据库
        CP-->>U: 命令结果
    else / 开头（普通命令）
        BT-->>E: pipeline.command
        E->>CP: Execute
        CP-->>U: 命令结果
    else 自然语言
        BT-->>E: pipeline.chat
        E->>RP: Execute
        RP->>P: GetOrCreateUser
        RP->>A: Chat(当前消息, userID)
        A->>A: LOD组装(L2→L1→L0) + RAG检索
        A->>A: Embed → Milvus检索 → 拼提示 → LLM
        A-->>RP: response
        RP->>P: SaveConversation (L0)
        A-.->Comp: MaybeCompress (异步)
        Comp-.->LLM: L0→L1 / L1→L2 压缩
        RP-->>U: 回复内容
    else 超时/异常
        BT-->>E: pipeline.fallback
        E->>FB: Execute
        FB-->>U: 兜底回复
    end
```

## 行为树结构

```mermaid
graph TD
    Root[Selector 根节点]
    Root --> S1[Sequence: 管理员命令]
    Root --> S2[Sequence: 普通命令]
    Root --> A1[Action: pipeline.chat]

    S1 --> C1[Condition: IsAdminCommand]
    S1 --> A2[Action: pipeline.admin]

    S2 --> C2[Condition: IsCommand]
    S2 --> A3[Action: pipeline.command]
```

优先级从上到下：管理员命令 > 普通命令 > 角色扮演

## 存储层设计

```mermaid
erDiagram
    users {
        bigserial id PK
        bigint qq_id UK
        varchar nickname
        timestamptz created_at
        timestamptz updated_at
    }
    conversations {
        bigserial id PK
        bigint user_id FK
        varchar role
        text content
        timestamptz created_at
    }
    episode_summaries {
        bigserial id PK
        bigint user_id FK
        text brief "一句话总结"
        text detailed "详细摘要"
        jsonb facts "结构化事实"
        int covered_count "压缩的原文条数"
        bigint first_convo_id
        bigint last_convo_id
        timestamptz created_at
    }
    topic_clusters {
        bigserial id PK
        bigint user_id FK
        varchar topic "主题名称"
        text brief "一句话主题总结"
        text detailed "详细主题描述"
        jsonb facts "聚合关键事实"
        int covered_count "聚合的episode条数"
        timestamptz created_at
        timestamptz updated_at
    }
    memories {
        bigserial id PK
        bigint user_id FK
        text content
        jsonb metadata
        timestamptz created_at
    }
    users ||--o{ conversations : "L0 原文"
    users ||--o{ episode_summaries : "L1 摘要"
    users ||--o{ topic_clusters : "L2 主题"
    users ||--o{ memories : "has"

    Milvus_lanmei_memories {
        int64 id PK "自增"
        int64 user_id "用户ID"
        varchar content "记忆文本"
        float_vector embedding "向量"
    }
```

## 待实现 / 规划中

- [ ] LLMClient 具体实现（等用户指定 provider）
- [ ] Embedder 具体实现（等用户指定 provider）
- [ ] Function Calling 自然语言命令路由（IntentPass）
- [ ] 多路召回（向量 + 关键词 + 时间）
- [x] LOD 记忆压缩（L0 原文 → L1 摘要 → L2 主题）
- [ ] 签到记录表
- [ ] 状态面板前端
