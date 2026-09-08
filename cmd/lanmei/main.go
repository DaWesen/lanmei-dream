package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/prompt"
	"github.com/DaWesen/lanmei-dream/internal/ai/skill"
	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/bizplugin"
	"github.com/DaWesen/lanmei-dream/internal/bot"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
	"github.com/DaWesen/lanmei-dream/internal/infra"
	kbpkg "github.com/DaWesen/lanmei-dream/internal/kb"
	"github.com/DaWesen/lanmei-dream/internal/kb/provider/feishu"
	"github.com/DaWesen/lanmei-dream/internal/kb/provider/local"
	"github.com/DaWesen/lanmei-dream/internal/kb/provider/sheet"
	"github.com/DaWesen/lanmei-dream/internal/manager"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/DaWesen/lanmei-dream/internal/topic"
	"go.uber.org/zap"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 配置初始化 ──
	cfg, err := config.Init()
	if err != nil {
		zap.L().Fatal("配置初始化失败", zap.Error(err))
	}

	// ── 日志初始化 ──
	logger := infra.InitLogger(&cfg.Log)
	defer logger.Sync()

	// ── 基础设施（PostgreSQL+pgvector + Redis + RustFS 对象存储）──
	// embeddingDim 透传给数据库迁移，保证 knowledge_chunks 向量列维度与模型一致
	inf, err := infra.Setup(ctx, &cfg.Database, &cfg.Redis, &cfg.Bot.Media, cfg.AI.EmbeddingDim, logger)
	if err != nil {
		logger.Fatal("基础设施初始化失败", zap.Error(err))
	}
	defer inf.Close()

	authorizer, err := pluginpkg.NewService(inf.DB.Orm)
	if err != nil {
		logger.Fatal("插件授权服务初始化失败", zap.Error(err))
	}
	superUserPrincipals := pluginpkg.ParseSuperUsers(cfg.Bot.ParseSuperUsers())
	if err := authorizer.InitBuiltinPolicies(superUserPrincipals); err != nil {
		logger.Fatal("插件内置权限初始化失败", zap.Error(err))
	}

	var (
		llmClient llm.LLMClient
		embedder  embedding.Embedder
		llmMgr    *llm.ProviderManager // 管理面板启用时承载热切换；禁用时为 nil
	)

	// ── LLM 客户端（eino，支持 OpenAI/DeepSeek/Qwen/Moonshot/Ark/Ollama）──
	if cfg.AI.LLMAPIKey != "" {
		einoLLM, err := llm.NewEinoClient(ctx, &llm.EinoOptions{
			BaseURL:     cfg.AI.LLMBaseURL,
			APIKey:      cfg.AI.LLMAPIKey,
			Model:       cfg.AI.LLMModel,
			MaxTokens:   cfg.AI.LLMMaxTokens,
			Temperature: cfg.AI.LLMTemperature,
		})
		if err != nil {
			logger.Fatal("LLM 初始化失败", zap.Error(err))
		}
		if cfg.Manager.Enabled {
			// 管理面板启用：注册 config.toml 配置的模型为默认 Provider，
			// 面板可在数据库 Provider 为空时继续使用；DB 有配置后由面板接管。
			llmMgr = llm.NewProviderManager(logger)
			if _, err := llmMgr.SetProviders([]*llm.Provider{{
				Name:        "default",
				BaseURL:     cfg.AI.LLMBaseURL,
				APIKey:      cfg.AI.LLMAPIKey,
				Model:       cfg.AI.LLMModel,
				MaxTokens:   cfg.AI.LLMMaxTokens,
				Temperature: cfg.AI.LLMTemperature,
				Enabled:     true,
				Priority:    0,
			}}); err != nil {
				logger.Warn("默认 LLM Provider 注册失败", zap.Error(err))
			}
			llmClient = llmMgr
		} else {
			llmClient = einoLLM
		}
		logger.Info("LLM 就绪", zap.String("base_url", cfg.AI.LLMBaseURL), zap.String("model", cfg.AI.LLMModel))
	} else {
		logger.Warn("LLM API Key 未配置，角色扮演不可用")
	}

	// ── Embedder 客户端 ──
	if cfg.AI.EmbeddingAPIKey != "" {
		embOpts := &embedding.EinoOptions{
			BaseURL:   cfg.AI.EmbeddingBaseURL,
			APIKey:    cfg.AI.EmbeddingAPIKey,
			Model:     cfg.AI.EmbeddingModel,
			Dimension: cfg.AI.EmbeddingDim,
		}
		// 火山方舟 doubao-embedding-vision 系列：标准 OpenAI /embeddings 端点不支持其
		// 多模态格式，走专用实现（POST /embeddings/multimodal，input=[{type,text}]）。
		if strings.Contains(cfg.AI.EmbeddingBaseURL, "volces.com") || strings.HasPrefix(cfg.AI.EmbeddingModel, "doubao-embedding") {
			volcEmb, err := embedding.NewVolcEmbedder(embOpts)
			if err != nil {
				logger.Fatal("Embedder 初始化失败", zap.Error(err))
			}
			embedder = volcEmb
			logger.Info("VolcEngine Embedder 就绪",
				zap.String("base_url", cfg.AI.EmbeddingBaseURL),
				zap.String("model", cfg.AI.EmbeddingModel),
				zap.Int("dim", cfg.AI.EmbeddingDim))
		} else {
			einoEmb, err := embedding.NewEinoEmbedder(ctx, embOpts)
			if err != nil {
				logger.Fatal("Embedder 初始化失败", zap.Error(err))
			}
			embedder = einoEmb
			logger.Info("Embedder 就绪", zap.String("base_url", cfg.AI.EmbeddingBaseURL), zap.String("model", cfg.AI.EmbeddingModel), zap.Int("dim", cfg.AI.EmbeddingDim))
		}
	} else {
		logger.Warn("Embedding API Key 未配置，RAG 检索不可用")
	}

	// ── Skill 系统 ──
	skillMgr := skill.NewManager(cfg.Skills.Dir, cfg.Skills.Config)
	if err := skillMgr.LoadAll(); err != nil {
		logger.Warn("技能加载不完整", zap.Error(err))
	} else {
		logger.Info("Skill 系统就绪", zap.Int("count", len(skillMgr.List())), zap.String("dir", cfg.Skills.Dir))
	}

	// ── Prompt 系统 ──
	promptMgr := prompt.NewManager(cfg.Prompts.Dir, cfg.Prompts.Config)
	promptMgr.SetSkills(skillMgr)
	if err := promptMgr.Load(cfg.Prompts.Config); err != nil {
		logger.Fatal("Prompt 系统加载失败", zap.Error(err))
	}
	logger.Info("Prompt 系统就绪", zap.String("dir", cfg.Prompts.Dir))

	var (
		chatSvc *ai.ChatService
		toolReg *tool.Registry
		kbSvc   *kbpkg.Service
	)
	if llmClient != nil {
		toolReg = tool.NewRegistry()
		chatSvc = ai.NewChatService(llmClient, embedder, inf.MemStore, inf.DB, toolReg, logger)
		chatSvc.SetPromptManager(promptMgr)

		// ── 知识库系统（provider 工厂注册 + 服务构建 + 工具注册 + 隐式召回注入）──
		if cfg.Knowledge.Enabled {
			// 注册 provider 工厂（未来新增 provider 在此追加注册即可）
			if err := kbpkg.RegisterProvider("local", local.New); err != nil {
				logger.Fatal("注册 local provider 失败", zap.Error(err))
			}
			if err := kbpkg.RegisterProvider("feishu", feishu.New); err != nil {
				logger.Fatal("注册 feishu provider 失败", zap.Error(err))
			}
			if err := kbpkg.RegisterProvider("sheet", sheet.New); err != nil {
				logger.Fatal("注册 sheet provider 失败", zap.Error(err))
			}
			kbSvc, err = kbpkg.NewService(ctx, &cfg.Knowledge, inf.DB.Orm, embedder, logger)
			if err != nil {
				logger.Fatal("知识库初始化失败", zap.Error(err))
			}
			if kbSvc != nil {
				if err := kbSvc.RegisterTools(toolReg); err != nil {
					logger.Fatal("注册知识库工具失败", zap.Error(err))
				}
				chatSvc.SetKnowledge(kbSvc)
				logger.Info("知识库系统就绪", zap.Int("bases", len(kbSvc.List())))
			}
		}
		logger.Info("AI 对话服务就绪")

		// ── 记忆维护器：后台周期清理膨胀的对话表与超龄向量记忆 ──
		// （群聊 L0 不参与压缩只增不减，memory_vectors 持续写入；按保留上限+时间衰减淘汰）
		maintainer := ai.NewMemoryMaintainer(inf.DB, logger)
		maintainer.Start(ctx)
	} else {
		logger.Warn("LLM 未配置，角色扮演不可用")
	}

	// ── 群聊话题（Topic）系统：决策管理器 + 冷却归档器 ──
	// 启用时替换群聊全量回复为"提及/话题制"选择性回复；未启用时传入 nil 退化为原行为。
	var topicMgr *topic.Manager
	if cfg.Bot.Topic.Enabled {
		archiver := topic.NewArchiver(llmClient, embedder, inf.MemStore, inf.DB, logger)
		topicMgr = topic.NewManager(&cfg.Bot.Topic, inf.StateStore, embedder, llmClient, archiver, botNicknames(&cfg.Bot), logger)
		topicMgr.Start(ctx)
		logger.Info("群聊话题系统就绪",
			zap.Bool("semantic", cfg.Bot.Topic.SemanticThreshold > 0 && embedder != nil),
			zap.Float64("mention_strong_threshold", topicMgr.StrongThreshold()),
			zap.Float64("mention_weak_threshold", topicMgr.WeakThreshold()))
	} else {
		logger.Info("群聊话题系统未启用（群聊退化为全量回复）")
	}

	// ── 命令系统 ──
	cmdSys := command.New()
	if err := cmdSys.Register(command.Command{
		Name:        "帮助",
		Description: "显示可用命令",
		Handler:     cmdSys.HelpHandler,
		Order:       51, // 紧跟 /help（50）之后
	}); err != nil {
		logger.Fatal("注册帮助命令失败", zap.Error(err))
	}
	// /help 别名：与 /帮助 同义，降低新用户使用门槛
	if err := cmdSys.Register(command.Command{
		Name:        "help",
		Description: "显示可用命令",
		Handler:     cmdSys.HelpHandler,
		Order:       50,
	}); err != nil {
		logger.Fatal("注册 help 命令失败", zap.Error(err))
	}

	// ── 视觉理解服务（多模态图片描述，可选）──
	var visionSvc *ai.VisionService
	if cfg.Bot.Media.VisionEnabled && llmClient != nil {
		visionModel := cfg.Bot.Media.VisionModel
		if visionModel == "" {
			visionModel = cfg.AI.LLMModel
		}
		visionLLM, err := llm.NewEinoClient(ctx, &llm.EinoOptions{
			BaseURL:     cfg.AI.LLMBaseURL,
			APIKey:      cfg.AI.LLMAPIKey,
			Model:       visionModel,
			MaxTokens:   min(cfg.AI.LLMMaxTokens, 1024),
			Temperature: 0.2, // 描述任务用低温，更客观
		})
		if err != nil {
			logger.Fatal("视觉理解模型初始化失败", zap.Error(err))
		}
		visionSvc = ai.NewVisionService(visionLLM.BaseModel(), logger)
		logger.Info("视觉理解服务就绪", zap.String("model", visionModel))
	} else {
		logger.Info("视觉理解未启用（图片仅缓存或占位）")
	}

	// ── 插件系统 ──
	pluginReg := pluginpkg.NewRegistry(nil, inf.StateStore, inf.DB, cmdSys, toolReg, logger)
	// 注入插件受限 KV 存储（PostgreSQL 持久化，类似前端 IndexedDB）：
	// 插件私有业务数据（如签到积分）通过 PluginContext.KV 读写，重启不丢失。
	pluginReg.SetKVStore(database.NewPluginKVStore(inf.DB.Orm))

	// ── 网关 ──
	gwServer := gateway.NewServer(&gateway.ListenConfig{
		ListenAddr:  cfg.Bot.Gateway.ListenAddr,
		AccessToken: cfg.Bot.Gateway.AccessToken,
	}, logger, nil)

	b := bot.New(&cfg.Bot, cmdSys, chatSvc, inf.DB, inf.StateStore, llmClient, pluginReg, gwServer, toolReg, logger, &bot.MediaDeps{
		Store:  inf.ObjectStore,
		Vision: visionSvc,
	}, topicMgr)

	gwServer.SetHandler(b)

	wasmManager, err := pluginpkg.NewWasmManager(&cfg.Plugin, inf.DB, pluginReg, authorizer, logger, nil)
	if err != nil {
		logger.Fatal("Wasm 插件管理器初始化失败", zap.Error(err))
	}
	if err := cmdSys.Register(pluginpkg.NewWasmInstallCommand(ctx, wasmManager)); err != nil {
		logger.Fatal("注册插件管理命令失败", zap.Error(err))
	}
	if err := wasmManager.LoadEnabled(ctx, pluginpkg.SystemPrincipal("startup")); err != nil {
		logger.Fatal("恢复已启用 Wasm 插件失败", zap.Error(err))
	}

	// ── 内置业务插件：配置驱动注册（[plugin.builtins]）──
	// 替代在 main.go 硬编码 if 块逐个注册的写法：启停由配置文件控制；
	// 内置插件与 Wasm 插件同走一个注册表，同名插件已由 Wasm 加载时自动跳过，避免 ID 冲突。
	bizReg := bizplugin.NewBusinessRegistry(&cfg.Plugin.Builtins, pluginReg, logger)
	bizReg.SetNCMURL(cfg.Plugin.NCMURL)
	bizReg.SetMusicSendMode(cfg.Plugin.MusicSendMode)
	bizReg.SetObjectStore(inf.ObjectStore)
	bizReg.SetVisionService(visionSvc)
	bizReg.SetLLMClient(llmClient)
	bizReg.SetQuizDir(cfg.Quiz.Dir)
	bizReg.SetRandomBeautyConfig(cfg.Plugin.RandomBeauty)
	// 海龟汤出题/判定 LLM 独立超时（默认 15s）：LLM 慢时快速降级回"汤煮糊了"，不耗尽消息预算
	bizReg.SetTurtleSoupTimeout(time.Duration(cfg.Bot.TurtleSoupTimeoutSeconds) * time.Second)
	if err := bizReg.RegisterBuiltins(); err != nil {
		logger.Fatal("内置业务插件注册失败", zap.Error(err))
	}

	// 表情情绪窗口（按群/私聊统计表情发送历史，注入提示词控制发表情节奏）
	if chatSvc != nil {
		moodWindow := ai.NewMoodWindow()
		chatSvc.SetMoodWindow(moodWindow)
	}

	if err := pluginReg.InitPlugins(ctx); err != nil {
		logger.Fatal("插件初始化失败", zap.Error(err))
	}
	if err := pluginReg.StartPlugins(ctx); err != nil {
		logger.Fatal("插件启动失败", zap.Error(err))
	}

	// 插件可能注册了新的命令/工具，刷新意图分析器使其感知
	b.RefreshIntentAnalyzer()

	// ── 管理面板（内嵌，独立端口；config.manager.enabled=false 时跳过）──
	var mgr *manager.Manager
	if cfg.Manager.Enabled {
		mgr, err = manager.New(&cfg.Manager, manager.Deps{
			DB:        inf.DB,
			Bot:       b,
			LLMMgr:    llmMgr,
			Logger:    logger,
			Skills:    skillMgr,
			Prompts:   promptMgr,
			Knowledge: kbSvc,
			Wasm:      wasmManager,
			Commands:  cmdSys,
		})
		if err != nil {
			logger.Fatal("管理面板初始化失败", zap.Error(err))
		}
		// 从数据库加载 Provider 到运行时（DB 为空时保留 config.toml 兜底）
		if err := mgr.LoadProviders(ctx); err != nil {
			logger.Warn("管理面板 LLM Provider 加载失败", zap.Error(err))
		}
		// 用量上报：ProviderManager（直连/流式）与 ChatService（工具/流式循环）统一接计费
		hook := mgr.BillingHook()
		if llmMgr != nil {
			llmMgr.SetUsageHook(hook)
		}
		if chatSvc != nil {
			chatSvc.SetUsageHook(hook)
		}
		if err := mgr.Start(ctx); err != nil {
			logger.Fatal("管理面板启动失败", zap.Error(err))
		}
		logger.Info("管理面板就绪", zap.String("listen", cfg.Manager.ListenAddr))
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("正在关闭...")
		pluginReg.StopPlugins(ctx)
		if kbSvc != nil {
			kbSvc.Close()
		}
		gwServer.Shutdown()
		if mgr != nil {
			_ = mgr.Stop()
		}
		cancel()
	}()

	logger.Info("蓝妹启动", zap.String("listen_addr", cfg.Bot.Gateway.ListenAddr))
	b.Run()
}

// botNicknames 汇总 Bot 的名字与别名（提及检测用）。
// 优先级：bot.topic.nicknames 配置 > bot.nickname（默认"蓝妹"）> 内置外号"蓝莓"，并去重。
func botNicknames(cfg *config.BotConfig) []string {
	seen := make(map[string]struct{}, 8)
	nicknames := make([]string, 0, 8)
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		nicknames = append(nicknames, n)
	}
	for _, n := range cfg.Topic.Nicknames {
		add(n)
	}
	if cfg.NickName != "" {
		add(cfg.NickName)
	}
	add("蓝莓") // 内置外号
	if len(nicknames) == 0 {
		nicknames = append(nicknames, "蓝妹")
	}
	return nicknames
}
