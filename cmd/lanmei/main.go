package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/bot"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/plugin/roleplay"
	"github.com/DaWesen/lanmei-dream/internal/plugin/signin"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── PostgreSQL ──
	pgURL := env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/lanmei?sslmode=disable")
	db, err := database.Connect(ctx, pgURL)
	if err != nil {
		log.Fatalf("连接 PostgreSQL 失败: %v", err)
	}
	defer db.Close()
	log.Println("PostgreSQL 已连接")

	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	log.Println("数据库迁移完成")

	// ── Milvus（RAG 向量存储）──
	milvusAddr := env("MILVUS_ADDR", "localhost:19530")
	milvusColl := env("MILVUS_COLLECTION", "lanmei_memories")
	embeddingDim := envInt("EMBEDDING_DIM", 1024)

	memStore, err := ai.NewMilvusMemoryStore(ctx, milvusAddr, milvusColl, embeddingDim)
	if err != nil {
		log.Printf("⚠ Milvus 连接失败，RAG 将不可用: %v", err)
	}
	if memStore != nil {
		defer memStore.Close()
		log.Println("Milvus 已连接")
	}

	// ── AI 对话服务 ──
	// TODO: 等用户指定 LLM 和 Embedding 提供方后，在此初始化具体实现
	//   var llmClient ai.LLMClient = xxx.NewClient(...)
	//   var embedder  ai.Embedder  = xxx.NewEmbedder(...)
	//
	// 目前 LLMClient / Embedder 接口已定义但无实现，
	// 角色扮演功能暂时不可用，命令系统正常工作。
	var (
		llmClient ai.LLMClient // = nil，待注入
		embedder  ai.Embedder  // = nil，待注入
	)

	var rp *roleplay.Plugin
	if llmClient != nil && embedder != nil {
		chatSvc := ai.NewChatService(llmClient, embedder, memStore)
		rp = roleplay.New(chatSvc, db)
		log.Println("AI 对话服务就绪")
	} else {
		log.Println("⚠ LLM/Embedding 未配置，角色扮演不可用（命令系统正常）")
	}

	// ── 命令系统 ──
	cmdSys := command.New()
	signinPlugin := signin.New(db)
	cmdSys.Register(command.Command{
		Name:        "签到",
		Description: "每日签到",
		Handler:     signinPlugin.HandleSignin,
	})
	cmdSys.Register(command.Command{
		Name:        "帮助",
		Description: "显示可用命令",
		Handler:     cmdSys.HelpHandler,
	})

	// ── ZeroBot ──
	wsURL := env("WS_URL", "ws://127.0.0.1:3001")
	accessToken := os.Getenv("ACCESS_TOKEN")
	nick := env("BOT_NICKNAME", "蓝妹")
	superUsers := parseSuperUsers(os.Getenv("SUPER_USERS"))

	b := bot.New(&bot.BotConfig{
		WebSocketURL: wsURL,
		AccessToken:  accessToken,
		NickName:     nick,
		SuperUsers:   superUsers,
	}, cmdSys, rp)

	// 优雅退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("正在关闭...")
		cancel()
		os.Exit(0)
	}()

	log.Printf("蓝妹启动，WebSocket → %s", wsURL)
	b.Run()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func parseSuperUsers(s string) []int64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var users []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			users = append(users, id)
		}
	}
	return users
}
