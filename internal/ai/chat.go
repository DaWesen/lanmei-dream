package ai

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/memory"
	"github.com/DaWesen/lanmei-dream/internal/database"
)

// ChatService 编排 RAG 流程：LOD 上下文组装 → RAG 检索 → 提示构建 → LLM 调用 → 异步压缩
type ChatService struct {
	client     llm.LLMClient
	embedder   embedding.Embedder
	memory     memory.MemoryStore
	db         *database.DB
	compressor *Compressor
}

// NewChatService 创建对话服务
func NewChatService(client llm.LLMClient, emb embedding.Embedder, mem memory.MemoryStore, db *database.DB) *ChatService {
	svc := &ChatService{
		client:   client,
		embedder: emb,
		memory:   mem,
		db:       db,
	}
	// 压缩器依赖 ChatService 的各组件
	if client != nil {
		svc.compressor = NewCompressor(client, emb, mem, db)
	}
	return svc
}

// Compressor 暴露压缩器给外部调用
func (s *ChatService) Compressor() *Compressor {
	return s.compressor
}

// Chat 执行一次完整对话：
//  1. 按 LOD 多级上下文组装（L2→L1→L0，token 预算控制）
//  2. RAG 检索长期记忆
//  3. 拼装 system + LOD + RAG + 原始消息
//  4. 调用 LLM
func (s *ChatService) Chat(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("chat: empty messages")
	}

	// ── LOD 多级上下文组装 ──
	msgs := make([]llm.Message, 0, len(req.Messages)+4)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: SystemPrompt})

	if s.db != nil {
		lod, err := s.db.GetLODContext(ctx, req.UserID, 3000) // 3000 token 预算给上下文
		if err != nil {
			log.Printf("ai.Chat: lod context: %v", err)
		} else if lod != nil {
			if len(lod.TopicBriefs) > 0 {
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem,
					Content: "历史话题概览：\n" + strings.Join(lod.TopicBriefs, "\n")})
			}
			if len(lod.EpisodeDetails) > 0 {
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem,
					Content: "过往对话摘要：\n" + strings.Join(lod.EpisodeDetails, "\n")})
			} else if len(lod.EpisodeBriefs) > 0 {
				msgs = append(msgs, llm.Message{Role: llm.RoleSystem,
					Content: "过往对话概要：\n" + strings.Join(lod.EpisodeBriefs, "\n")})
			}

			// L0 原始对话（LOD 已经按预算筛选）
			for _, c := range lod.RawConversations {
				msgs = append(msgs, llm.Message{Role: llm.Role(c.Role), Content: c.Content})
			}
		}
	}

	// ── RAG 检索长期记忆 ──
	lastMsg := req.Messages[len(req.Messages)-1]
	queryVec, err := s.embedder.Embed(ctx, lastMsg.Content)
	if err != nil {
		log.Printf("ai.Chat: embed failed: %v", err)
	} else if s.memory != nil {
		memories, err := s.memory.Retrieve(ctx, queryVec, req.UserID, 5)
		if err != nil {
			log.Printf("ai.Chat: retrieve memory failed: %v", err)
		}
		if ragCtx := BuildRAGContext(memories); ragCtx != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: ragCtx})
		}
	}

	// 如果 LOD 没返回 L0 原始对话，用请求传入的
	if len(req.Messages) > 0 && (s.db == nil) {
		msgs = append(msgs, req.Messages...)
	} else if s.db != nil {
		// 只追加 LOD 没覆盖到的最新消息
		msgs = append(msgs, req.Messages...)
	}

	req.Messages = msgs

	resp, err := s.client.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("chat: llm call: %w", err)
	}

	// ── 异步：存记忆 + 触发压缩 ──
	if s.memory != nil && queryVec != nil {
		go func() {
			bgCtx := context.Background()
			_ = s.memory.Store(bgCtx, &memory.Memory{
				UserID:  req.UserID,
				Content: lastMsg.Content,
				Vector:  queryVec,
			})
		}()
	}
	if s.compressor != nil {
		go s.compressor.MaybeCompress(context.Background(), req.UserID)
	}

	return resp, nil
}
