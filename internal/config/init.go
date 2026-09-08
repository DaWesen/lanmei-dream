package config

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Init 初始化配置：命令行参数 → 配置文件 → 环境变量 → 默认值
func Init() (*Config, error) {
	initFlags()
	pflag.Parse()

	v := viper.New()
	setDefaults(v)

	// 配置文件（仅含非敏感配置）
	configFile, _ := pflag.CommandLine.GetString("config")
	v.SetConfigFile(configFile)

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Println("未找到配置文件，将使用默认值和环境变量")
		} else if os.IsNotExist(err) {
			log.Println("未找到配置文件，将使用默认值和环境变量")
		} else {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	// 环境变量优先（敏感信息）：LANMEI_DATABASE_URL、LANMEI_AI_LLM_API_KEY 等
	v.SetEnvPrefix("LANMEI")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// ── 关键修复：Viper.Sub() 不传播 AutomaticEnv ──
	// 当 Unmarshal 处理嵌套结构体（如 AIConfig）时，v.Sub("ai") 创建的子 Viper
	// 是全新的实例，不会继承父 Viper 的 AutomaticEnv 设置。
	// 因此只存在于环境变量而不在 config.toml 中的键（如 ai.llm_api_key）
	// 无法被子 Viper 解析到，导致 Unmarshal 后对应字段为空。
	//
	// 解决方案：在 Unmarshal 前将 LANMEI_ 前缀环境变量按正确映射写入 Viper，
	// 确保 Sub() 能通过 allKeys 找到它们。
	// 注意：不能简单地 ReplaceAll("_", ".") 反推，因为部分键名本身包含下划线
	//（如 llm_api_key, super_users），这个映射是 Viper 内部 (cfgKey → envVar)
	// 的单向变换，不可逆。
	envToCfg := map[string]string{
		"LANMEI_DATABASE_URL":                    "database.url",
		"LANMEI_REDIS_ADDR":                      "redis.addr",
		"LANMEI_BOT_NICKNAME":                    "bot.nickname",
		"LANMEI_BOT_SUPER_USERS":                 "bot.super_users",
		"LANMEI_BOT_GATEWAY_LISTEN_ADDR":         "bot.gateway.listen_addr",
		"LANMEI_BOT_GATEWAY_ACCESS_TOKEN":        "bot.gateway.access_token",
		"LANMEI_BOT_INTENT_TIMEOUT_SECONDS":      "bot.intent_timeout_seconds",
		"LANMEI_BOT_TURTLE_SOUP_TIMEOUT_SECONDS": "bot.turtle_soup_timeout_seconds",
		"LANMEI_AI_LLM_API_KEY":                  "ai.llm_api_key",
		"LANMEI_AI_LLM_BASE_URL":                 "ai.llm_base_url",
		"LANMEI_AI_LLM_MODEL":                    "ai.llm_model",
		"LANMEI_AI_LLM_MAX_TOKENS":               "ai.llm_max_tokens",
		"LANMEI_AI_LLM_TEMPERATURE":              "ai.llm_temperature",
		"LANMEI_AI_EMBEDDING_API_KEY":            "ai.embedding_api_key",
		"LANMEI_AI_EMBEDDING_BASE_URL":           "ai.embedding_base_url",
		"LANMEI_AI_EMBEDDING_MODEL":              "ai.embedding_model",
		"LANMEI_AI_EMBEDDING_DIM":                "ai.embedding_dim",
		"LANMEI_PLUGIN_ROOT_DIR":                 "plugin.root_dir",
		"LANMEI_PLUGIN_NCM_URL":                  "plugin.ncm_url",
		"LANMEI_PLUGIN_MUSIC_SEND_MODE":          "plugin.music_send_mode",
		"LANMEI_KNOWLEDGE_ENABLED":               "knowledge.enabled",
		"LANMEI_KNOWLEDGE_AUTO_RECALL_LIMIT":     "knowledge.auto_recall_limit",
		// 多媒体（RustFS）与机器人行为配置
		"LANMEI_MEDIA_ENDPOINT":   "bot.media.endpoint",
		"LANMEI_MEDIA_ACCESS_KEY": "bot.media.access_key",
		"LANMEI_MEDIA_SECRET_KEY": "bot.media.secret_key",
		"LANMEI_MEDIA_BUCKET":     "bot.media.bucket",
		"LANMEI_MEDIA_REGION":     "bot.media.region",
		"LANMEI_TOPIC_ENABLED":    "bot.topic.enabled",
		// 管理面板（docker-compose 经 nginx 托管前端时通过 env 启用）
		"LANMEI_MANAGER_ENABLED":       "manager.enabled",
		"LANMEI_MANAGER_LISTEN_ADDR":   "manager.listen_addr",
		"LANMEI_MANAGER_WEBAUTHN_RPID": "manager.webauthn_rpid",
	}
	for envKey, cfgKey := range envToCfg {
		if val := os.Getenv(envKey); val != "" {
			v.Set(cfgKey, val)
		}
	}

	// 命令行参数绑定
	pflag.CommandLine.VisitAll(func(f *pflag.Flag) {
		if f.Name != "config" {
			_ = v.BindPFlag(f.Name, f)
		}
	})

	// 解析为结构体
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	return &cfg, nil
}

func initFlags() {
	pflag.String("config", "./config/config.toml", "配置文件路径")
	pflag.String("bot.nickname", "", "机器人昵称")
	pflag.String("bot.super_users", "", "超级用户列表（格式：platform:userID,逗号分隔）")
	pflag.String("log.level", "", "日志级别：debug/info/warn/error")
	pflag.Bool("log.persistent", false, "是否持久化日志到文件")
	pflag.String("log.path", "", "日志文件路径")
	pflag.Bool("log.compression", false, "是否压缩旧日志文件")
	pflag.Int("log.max_size", 0, "单个日志文件最大大小（MB）")
	pflag.Int("log.max_age", 0, "日志文件最大保留天数")
	pflag.Int("log.max_backups", 0, "最多保留的旧日志文件数")
	pflag.String("plugin.root_dir", "", "Wasm 插件根目录")
	pflag.String("plugin.ncm_url", "", "网易云音乐 API 服务地址（点歌插件）")
	// Prompts 路径
	pflag.String("prompts.dir", "", "Prompt 系统目录")
	pflag.String("prompts.config", "", "Prompt 配置文件路径")
	// Skills 路径
	pflag.String("skills.dir", "", "Skill 目录")
	pflag.String("skills.config", "", "Skill 配置文件路径")
	// Quiz 题库路径
	pflag.String("quiz.dir", "", "编程答题题库目录")
}

func setDefaults(v *viper.Viper) {
	// Bot 默认值
	v.SetDefault("bot.nickname", "蓝妹")
	v.SetDefault("bot.super_users", "")
	v.SetDefault("bot.gateway.listen_addr", "0.0.0.0:8080")
	v.SetDefault("bot.intent_timeout_seconds", 8)        // 意图分析独立超时 8s，LLM 故障时快速降级，避免吃满消息预算
	v.SetDefault("bot.turtle_soup_timeout_seconds", 120) // 海龟汤异步出题/判定最长等待 120s（独立 context，不占消息预算）
	v.SetDefault("bot.stream.typing_speed_ms", 150)      // 150ms/字，模拟打字速度
	v.SetDefault("bot.stream.min_interval_ms", 1000)     // 最小 1 秒
	v.SetDefault("bot.stream.max_interval_ms", 5000)     // 最大 5 秒（长消息段间隔上限，避免过久等待）
	v.SetDefault("bot.stream.jitter_pct", 0.25)          // ±25% 抖动

	// 多媒体（RustFS）默认值
	v.SetDefault("bot.media.endpoint", "http://localhost:9000")
	v.SetDefault("bot.media.bucket", "lanmei-media")
	v.SetDefault("bot.media.region", "us-east-1")
	v.SetDefault("bot.media.max_download_bytes", 10485760) // 10MB
	v.SetDefault("bot.media.vision_enabled", false)
	v.SetDefault("bot.media.vision_model", "")

	// 内置业务插件默认开启（配置驱动注册）
	v.SetDefault("plugin.builtins.signin", true)
	v.SetDefault("plugin.builtins.welcome", true)
	v.SetDefault("plugin.builtins.poke", true)
	v.SetDefault("plugin.builtins.three_g", true)
	v.SetDefault("plugin.builtins.turtle_soup", true)
	v.SetDefault("plugin.builtins.answer_question", true)
	v.SetDefault("plugin.builtins.daily_quote", true)
	v.SetDefault("plugin.builtins.random_beauty", true)
	v.SetDefault("plugin.random_beauty.api_base_url", "https://i.mukyu.ru")
	v.SetDefault("plugin.random_beauty.timeout_seconds", 18)
	v.SetDefault("plugin.random_beauty.max_attempts", 2)
	v.SetDefault("plugin.random_beauty.cooldown_seconds", 30)
	v.SetDefault("plugin.random_beauty.max_image_bytes", 10*1024*1024)
	v.SetDefault("plugin.random_beauty.min_width", 720)
	v.SetDefault("plugin.random_beauty.min_height", 720)
	v.SetDefault("plugin.random_beauty.min_bookmarks", 100)
	v.SetDefault("plugin.random_beauty.safe_confidence", 0.9)
	v.SetDefault("plugin.random_beauty.moderation_timeout_seconds", 8)

	// 群聊 topic 系统默认值
	v.SetDefault("bot.topic.enabled", true)
	v.SetDefault("bot.topic.nicknames", []string{})
	v.SetDefault("bot.topic.topic_window_msgs", 20)
	v.SetDefault("bot.topic.cooling_timeout_minutes", 30)
	v.SetDefault("bot.topic.semantic_threshold", 0.5)
	v.SetDefault("bot.topic.credit_enabled", true)
	v.SetDefault("bot.topic.linguistic_strong_threshold", 0.7)
	v.SetDefault("bot.topic.linguistic_weak_threshold", 0.4)
	v.SetDefault("bot.topic.archive_interval_seconds", 60)

	// Log 默认值
	v.SetDefault("log.level", "info")
	v.SetDefault("log.persistent", false)
	v.SetDefault("log.path", "./logs/lanmei.log")
	v.SetDefault("log.compression", true)
	v.SetDefault("log.max_size", 100)
	v.SetDefault("log.max_age", 30)
	v.SetDefault("log.max_backups", 10)

	// Plugin 默认值
	v.SetDefault("plugin.root_dir", "./data/plugins")
	v.SetDefault("plugin.ncm_url", "")
	v.SetDefault("plugin.music_send_mode", "auto") // auto/card/link，见 PluginConfig.MusicSendMode

	// Prompts 默认路径
	v.SetDefault("prompts.dir", "./prompts")
	v.SetDefault("prompts.config", "./prompts/prompts.toml")

	// Skills 默认路径
	v.SetDefault("skills.dir", "./skills")
	v.SetDefault("skills.config", "./config/skills.toml")

	// Quiz 默认题库路径
	v.SetDefault("quiz.dir", "./quizdata")

	// Knowledge 默认值（知识库默认关闭，需在配置中显式启用）
	v.SetDefault("knowledge.enabled", false)
	v.SetDefault("knowledge.default_recall_modes", []string{"vector", "fuzzy"})
	v.SetDefault("knowledge.auto_recall_limit", 3)
	v.SetDefault("knowledge.weights.vector", 1.0)
	v.SetDefault("knowledge.weights.fuzzy", 0.8)
	v.SetDefault("knowledge.weights.time", 0.5)

	// LLM 非敏感默认值（API Key 仅从环境变量读取）
	v.SetDefault("ai.llm_base_url", "https://api.openai.com/v1")
	v.SetDefault("ai.llm_model", "gpt-4o-mini")
	v.SetDefault("ai.llm_max_tokens", 4096)
	v.SetDefault("ai.llm_temperature", 0.7)

	// Embedding 非敏感默认值（API Key 仅从环境变量读取）
	v.SetDefault("ai.embedding_base_url", "https://api.openai.com/v1")
	v.SetDefault("ai.embedding_model", "text-embedding-3-small")
	v.SetDefault("ai.embedding_dim", 1536)

	// 数据库连接字符串从环境变量 LANMEI_DATABASE_URL 获取，此处仅保留默认
	v.SetDefault("database.url", "postgres://postgres:postgres@localhost:5432/lanmei?sslmode=disable")

	// Redis 地址仅作为非敏感默认，密码通过环境变量覆盖
	v.SetDefault("redis.addr", "localhost:6379")
}
