package config

import (
	"strings"
)

// Config 应用总配置，按模块拆分为子结构体
type Config struct {
	Database  DatabaseConfig  `mapstructure:"database"`
	Redis     RedisConfig     `mapstructure:"redis"`
	AI        AIConfig        `mapstructure:"ai"`
	Bot       BotConfig       `mapstructure:"bot"`
	Plugin    PluginConfig    `mapstructure:"plugin"`
	Log       LogConfig       `mapstructure:"log"`
	Prompts   PromptsConfig   `mapstructure:"prompts"`
	Skills    SkillsConfig    `mapstructure:"skills"`
	Quiz      QuizConfig      `mapstructure:"quiz"`
	Knowledge KnowledgeConfig `mapstructure:"knowledge"`
	Manager   ManagerConfig   `mapstructure:"manager"`
}

// ManagerConfig 管理面板服务配置。
// 注意：认证相关的敏感配置（超级管理员账号/密码、加密主密钥）不在 toml 中，
// 一律从环境变量读取（LANMEI_MANAGER_ADMIN_USERNAME / LANMEI_MANAGER_ADMIN_PASSWORD / LANMEI_MANAGER_SECRET_KEY），
// 避免明文密钥落盘。
type ManagerConfig struct {
	// Enabled 管理面板总开关，默认 false
	Enabled bool `mapstructure:"enabled"`
	// ListenAddr 管理面板监听地址，如 "0.0.0.0:8090"
	ListenAddr string `mapstructure:"listen_addr"`
	// TraceRetentionDays conduit_trace 保留天数，默认 7
	TraceRetentionDays int `mapstructure:"trace_retention_days"`
	// AccessTokenTTLMinutes 短期 Access Token 有效期（分钟），默认 15
	AccessTokenTTLMinutes int `mapstructure:"access_token_ttl_minutes"`
	// RefreshTokenTTLHours 长期 Refresh Token 有效期（小时），默认 168（7 天）
	RefreshTokenTTLHours int `mapstructure:"refresh_token_ttl_hours"`
	// EnableWebAuthn WebAuthn（passkey）总开关，默认 true。
	// 部署在 IP / 非 HTTPS 环境时浏览器不可用 passkey，登录自动回退密码，服务端不受影响。
	EnableWebAuthn bool `mapstructure:"enable_webauthn"`
	// WebAuthnRPID WebAuthn Relying Party ID（必须为域名，不含端口与协议）。
	// 留空时服务端根据请求 Host 推断（IP 部署时会自动禁用 passkey 相关注册接口）。
	WebAuthnRPID string `mapstructure:"webauthn_rpid"`
	// WebAuthnDisplayName WebAuthn 展示名（默认 "Lanmei Manager"）
	WebAuthnDisplayName string `mapstructure:"webauthn_display_name"`
	// WebAuthnOrigins 允许的 WebAuthn 来源（完整 origin，如 https://lanmei.example.com）。
	// 留空时使用 RPID 推断。
	WebAuthnOrigins []string `mapstructure:"webauthn_origins"`
	// RateLimitPerMinute 登录类接口每 IP 每分钟最大请求数，默认 20
	RateLimitPerMinute int `mapstructure:"rate_limit_per_minute"`
	// SessionMaxPerUser 单账号最大活跃会话数（超出后最旧会话被吊销），默认 10
	SessionMaxPerUser int `mapstructure:"session_max_per_user"`
	// MaxLoginFails 连续登录失败锁定阈值，默认 5
	MaxLoginFails int `mapstructure:"max_login_fails"`
	// LoginLockMinutes 登录锁定分钟数，默认 15
	LoginLockMinutes int `mapstructure:"login_lock_minutes"`
	// TrustedProxies 信任的反向代理网段（获取真实客户端 IP），空则不信任任何代理
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// PluginConfig 插件系统配置
type PluginConfig struct {
	// RootDir Wasm 插件根目录
	RootDir string `mapstructure:"root_dir"`

	// NCMURL 网易云音乐 API 服务地址（网易云点歌插件使用）。
	// 为空时点歌插件不可用；格式如 http://ncm-api:3000
	NCMURL string `mapstructure:"ncm_url"`

	// MusicSendMode 点歌结果的发送方式（适配不同反向代理工具）：
	//   - auto（默认）：QQ/NapCat 平台发 OB11 music 段（音乐卡片），其余平台发文字链接
	//   - card：强制发 music 段（要求 OneBot 实现支持 music 段，如 llonebot）
	//   - link：强制发文字链接（最通用，任何工具都能展示）
	MusicSendMode string `mapstructure:"music_send_mode"`

	// Builtins 内置业务插件开关（配置驱动注册，替代 main.go 硬编码注册）
	Builtins PluginBuiltinsConfig `mapstructure:"builtins"`
}

// PluginBuiltinsConfig 内置业务插件开关。
// 每个开关对应一个内置插件；false 时不注册该插件。
// 若同名插件已由 Wasm 动态加载，注册表会自动跳过内置注册，避免重复。
type PluginBuiltinsConfig struct {
	// Signin 签到插件
	Signin bool `mapstructure:"signin"`
	// Welcome 入群欢迎插件
	Welcome bool `mapstructure:"welcome"`
	// Poke 戳一戳回复插件
	Poke bool `mapstructure:"poke"`
	// ThreeG 3G 关键词科普插件
	ThreeG bool `mapstructure:"three_g"`
	// Rank 签到积分排行榜插件
	Rank bool `mapstructure:"rank"`
	// Cat 猫猫图片插件
	Cat bool `mapstructure:"cat"`
	// BaLogo 蔚蓝档案 LOGO 插件
	BaLogo bool `mapstructure:"balogo"`
	// Ping 连通性测试插件
	Ping bool `mapstructure:"ping"`
	// GitHubCard GitHub 链接卡片插件
	GitHubCard bool `mapstructure:"github_card"`
	// Music 网易云点歌插件
	Music bool `mapstructure:"music"`
	// Sticker 自定义表情库插件
	Sticker bool `mapstructure:"sticker"`
	// TurtleSoup 海龟汤文字游戏插件
	TurtleSoup bool `mapstructure:"turtle_soup"`
	// AnswerQuestion 编程答题游戏插件
	AnswerQuestion bool `mapstructure:"answer_question"`
	// DailyQuote 每日一句插件
	DailyQuote bool `mapstructure:"daily_quote"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	URL string `mapstructure:"url"`
}

// RedisConfig Redis 配置
type RedisConfig struct {
	Addr string `mapstructure:"addr"`
}

// AIConfig AI 相关配置
type AIConfig struct {
	// LLM 配置（OpenAI 兼容 API）
	LLMBaseURL     string  `mapstructure:"llm_base_url"`
	LLMAPIKey      string  `mapstructure:"llm_api_key"`
	LLMModel       string  `mapstructure:"llm_model"`
	LLMMaxTokens   int     `mapstructure:"llm_max_tokens"`
	LLMTemperature float64 `mapstructure:"llm_temperature"`

	// Embedding 配置（OpenAI 兼容 API）
	EmbeddingBaseURL string `mapstructure:"embedding_base_url"`
	EmbeddingAPIKey  string `mapstructure:"embedding_api_key"`
	EmbeddingModel   string `mapstructure:"embedding_model"`
	EmbeddingDim     int    `mapstructure:"embedding_dim"`
}

// BotConfig 机器人配置
type BotConfig struct {
	NickName   string        `mapstructure:"nickname"`
	SuperUsers string        `mapstructure:"super_users"` // 格式：platform:userID,... 如 qq:123456,wechat:wxid_xxx
	Gateway    GatewayConfig `mapstructure:"gateway"`
	Stream     StreamConfig  `mapstructure:"stream"`
	Media      MediaConfig   `mapstructure:"media"`
	Topic      TopicConfig   `mapstructure:"topic"`
	// IntentTimeoutSeconds 意图分析 LLM 调用的独立超时（秒）。
	// 意图分析只是消息路由前置步骤，不应吃满整条消息的处理预算；
	// LLM 故障时快速降级，避免后续对话管线失去剩余时间、触发"迷糊"兜底。
	// 0 表示不设独立超时（沿用消息级超时，默认 20s）。
	IntentTimeoutSeconds int `mapstructure:"intent_timeout_seconds"`
	// TurtleSoupTimeoutSeconds 海龟汤出题/判定的异步任务最长等待（秒）。
	// 命令到达后立即回复提示并挂起管线，LLM 在后台 goroutine 以独立 context 执行，
	// 不受消息级超时约束；本值是其上限（默认 120s），<=0 时用固定 120s。
	TurtleSoupTimeoutSeconds int `mapstructure:"turtle_soup_timeout_seconds"`
}

// MediaConfig 多媒体处理配置（RustFS 对象存储 + 视觉理解）
type MediaConfig struct {
	// Endpoint RustFS 服务端点（S3 兼容），如 http://localhost:9000
	Endpoint string `mapstructure:"endpoint"`
	// AccessKey / SecretKey S3 凭据（敏感值建议用环境变量注入）
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	// Bucket 媒体对象存储桶名
	Bucket string `mapstructure:"bucket"`
	// Region S3 区域标识（RustFS 通常任意值即可）
	Region string `mapstructure:"region"`
	// MaxDownloadBytes 单张多媒体下载大小上限
	MaxDownloadBytes int64 `mapstructure:"max_download_bytes"`
	// VisionEnabled 视觉理解开关（需配置支持多模态的模型）
	VisionEnabled bool `mapstructure:"vision_enabled"`
	// VisionModel 视觉模型名（空则复用主对话模型）
	VisionModel string `mapstructure:"vision_model"`
}

// TopicConfig 群聊话题（Topic）系统配置
type TopicConfig struct {
	// Enabled topic 系统总开关（false 时回退为现行为：群聊全回复）
	Enabled bool `mapstructure:"enabled"`
	// Nicknames Bot 名字与别名（为空时使用 bot.nickname，内置"蓝莓"外号）
	Nicknames []string `mapstructure:"nicknames"`
	// TopicWindowMsgs 活跃窗口：最近 N 条群消息内提及/回复即保持 Active
	TopicWindowMsgs int `mapstructure:"topic_window_msgs"`
	// CoolingTimeoutMinutes 冷却超时（分钟）后归档
	CoolingTimeoutMinutes int `mapstructure:"cooling_timeout_minutes"`
	// SemanticThreshold 语义相关阈值（0~1），0 表示关闭语义判定（纯成员制）
	SemanticThreshold float64 `mapstructure:"semantic_threshold"`
	// CreditEnabled 回复配额（防刷屏）
	CreditEnabled bool `mapstructure:"credit_enabled"`
	// LinguisticStrongThreshold 提及判断"强提及"的置信度下限（0~1），默认 0.7。
	// 达到该值视为确定在跟 Bot 说话 → 直接回复（等同 at 强信号）。
	LinguisticStrongThreshold float64 `mapstructure:"linguistic_strong_threshold"`
	// LinguisticWeakThreshold 提及判断"弱提及"的置信度下限（0~1），默认 0.4。
	// 达到该值视为疑似提及 → 拉入话题（非成员静默，成员按配额续聊）。
	LinguisticWeakThreshold float64 `mapstructure:"linguistic_weak_threshold"`
	// ArchiveIntervalSeconds 归档扫描间隔（秒）
	ArchiveIntervalSeconds int `mapstructure:"archive_interval_seconds"`
}

// StreamConfig 流式回复配置
type StreamConfig struct {
	// TypingSpeedMS 打字速度（毫秒/字）。
	// 段落发送间隔 = 下一段字数 × 打字速度，模拟真人打字耗时。
	// 设为 0 则禁用间隔机制（段落间不延迟）。
	TypingSpeedMS int `mapstructure:"typing_speed_ms"`

	// MinIntervalMS 最小发送间隔（毫秒）。
	// 无论字数多短，间隔不小于此值，保证平台不乱序。
	MinIntervalMS int `mapstructure:"min_interval_ms"`

	// MaxIntervalMS 最大发送间隔（毫秒）。
	// 无论字数多长，间隔不大于此值，避免过长等待。
	// 设为 0 表示不设上限。
	MaxIntervalMS int `mapstructure:"max_interval_ms"`

	// JitterPct 间隔抖动比例（0.0-1.0）。
	// 实际间隔 = 基础间隔 × (1 ± JitterPct)，增加真实感。
	// 0 表示无抖动。
	JitterPct float64 `mapstructure:"jitter_pct"`
}

// GatewayConfig 网关（反向 WebSocket 服务端）配置
type GatewayConfig struct {
	ListenAddr  string `mapstructure:"listen_addr"`  // 监听地址，如 "0.0.0.0:8080"
	AccessToken string `mapstructure:"access_token"` // 鉴权 token（空则不鉴权）
}

// PromptsConfig Prompt 系统路径配置
type PromptsConfig struct {
	Dir    string `mapstructure:"dir"`
	Config string `mapstructure:"config"`
}

// SkillsConfig Skill 系统路径配置
type SkillsConfig struct {
	Dir    string `mapstructure:"dir"`
	Config string `mapstructure:"config"`
}

// QuizConfig 编程答题题库路径配置
type QuizConfig struct {
	Dir string `mapstructure:"dir"`
}

// KnowledgeConfig 知识库系统配置
type KnowledgeConfig struct {
	// Enabled 知识库总开关，默认 false
	Enabled bool `mapstructure:"enabled"`

	// DefaultModes 隐式召回与 kb_search 工具的默认召回模式，可选值：vector/fuzzy/time
	// 空数组表示使用各 provider 的全部能力
	DefaultModes []string `mapstructure:"default_recall_modes"`

	// AutoRecallLimit 每轮对话隐式召回注入上下文的条数，默认 3
	AutoRecallLimit int `mapstructure:"auto_recall_limit"`

	// Weights 多路召回合并权重
	Weights RecallWeightsConfig `mapstructure:"weights"`

	// Bases 知识库列表
	Bases []KnowledgeBaseConfig `mapstructure:"bases"`
}

// RecallWeightsConfig 各召回模式的合并权重（多路命中时按权重累加排名分）
type RecallWeightsConfig struct {
	Vector float64 `mapstructure:"vector"`
	Fuzzy  float64 `mapstructure:"fuzzy"`
	Time   float64 `mapstructure:"time"`
}

// KnowledgeBaseConfig 单个知识库的配置。
// Config 为 provider 私有配置（如 local 的 docs_dir、feishu 的 app_id 等），
// 由对应 provider 工厂解析；敏感值支持 "env:VAR_NAME" 语法从环境变量读取。
type KnowledgeBaseConfig struct {
	ID          string         `mapstructure:"id"`
	Name        string         `mapstructure:"name"`
	Description string         `mapstructure:"description"`
	Provider    string         `mapstructure:"provider"` // local / feishu
	Enabled     bool           `mapstructure:"enabled"`
	RecallLimit int            `mapstructure:"recall_limit"` // 单模式召回上限，默认 5
	Config      map[string]any `mapstructure:"config"`
}

// LogConfig 日志相关配置
type LogConfig struct {
	Level       string `mapstructure:"level"`       // 日志级别：debug/info/warn/error，默认 info
	Persistent  bool   `mapstructure:"persistent"`  // 是否持久化到文件，默认 false
	Path        string `mapstructure:"path"`        // 日志文件路径（Persistent=true 时），默认 ./logs/lanmei.log
	Compression bool   `mapstructure:"compression"` // 是否压缩旧日志文件，默认 true
	MaxSize     int    `mapstructure:"max_size"`    // 单个日志文件最大大小（MB），默认 100
	MaxAge      int    `mapstructure:"max_age"`     // 日志文件最大保留天数，默认 30
	MaxBackups  int    `mapstructure:"max_backups"` // 最多保留的旧日志文件数，默认 10
}

// ParseSuperUsers 返回原始超级用户字符串，供 plugin.ParseSuperUsers 使用
func (c *BotConfig) ParseSuperUsers() string {
	return strings.TrimSpace(c.SuperUsers)
}
