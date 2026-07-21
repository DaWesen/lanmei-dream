package ai

import (
	"context"
	"fmt"
	"log"
)

// ChatService 编排 RAG 流程：向量化 → 记忆检索 → 提示构建 → LLM 调用
type ChatService struct {
	client   LLMClient
	embedder Embedder
	memory   MemoryStore
}

// NewChatService 创建对话服务
func NewChatService(client LLMClient, emb Embedder, mem MemoryStore) *ChatService {
	return &ChatService{
		client:   client,
		embedder: emb,
		memory:   mem,
	}
}

// Chat 执行一次完整对话：
//  1. 对用户最后一条消息做向量化
//  2. 从 MemoryStore 检索相关记忆
//  3. 拼装 system + RAG 上下文 + 历史消息
//  4. 调用 LLM
func (s *ChatService) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("chat: empty messages")
	}

	// 取最后一条用户消息用于检索
	lastMsg := req.Messages[len(req.Messages)-1]
	queryVec, err := s.embedder.Embed(ctx, lastMsg.Content)
	if err != nil {
		log.Printf("ai.Chat: embed failed: %v", err)
	} else if s.memory != nil {
		// RAG 检索
		memories, err := s.memory.Retrieve(ctx, queryVec, req.UserID, 5)
		if err != nil {
			log.Printf("ai.Chat: retrieve memory failed: %v", err)
		}

		// 拼装完整消息列表
		msgs := make([]Message, 0, len(req.Messages)+2)
		msgs = append(msgs, Message{Role: RoleSystem, Content: SystemPrompt})

		if ragCtx := BuildRAGContext(memories); ragCtx != "" {
			msgs = append(msgs, Message{Role: RoleSystem, Content: ragCtx})
		}
		msgs = append(msgs, req.Messages...)

		req.Messages = msgs
	}

	resp, err := s.client.Chat(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("chat: llm call: %w", err)
	}

	// 将本轮对话存入记忆（异步，不阻塞回复）
	if s.memory != nil && queryVec != nil {
		go func() {
			bgCtx := context.Background()
			err := s.memory.Store(bgCtx, &Memory{
				UserID:  req.UserID,
				Content: lastMsg.Content,
				Vector:  queryVec,
			})
			if err != nil {
				log.Printf("ai.Chat: store memory: %v", err)
			}
		}()
	}

	return resp, nil
}
