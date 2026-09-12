package bizplugin

import (
	"fmt"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	randombeauty "github.com/DaWesen/lanmei-dream/internal/bizplugin/random_beauty"
	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/media"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"go.uber.org/zap"
)

// ============================================================
// BusinessRegistry 内置业务插件注册表
// ============================================================

// BusinessRegistry 负责按配置文件开关动态注册内置业务插件，
// 替代在 main.go 中硬编码 if 块逐个注册的写法。
//
// 设计要点：
//   - 插件启用与否完全由配置（[plugin.builtins]）决定，改配置即可启停，无需改代码；
//   - 注册前先查询注册表：若同名插件已由 Wasm 动态加载（如用户安装了 wasm 版签到/欢迎），
//     则跳过内置注册，避免 ID 冲突，保证"注册表优先、Wasm 优先"的动态性；
//   - 内置插件与 Wasm 插件走同一注册表，后续生命周期（Init/Start/Stop）统一由 Registry 管理；
//   - 插件私有业务数据通过受限 KV 存储（PluginContext.KV，PostgreSQL 持久化）读写，
//     内置插件不直接持有裸数据库。
type BusinessRegistry struct {
	cfg                *config.PluginBuiltinsConfig // 内置插件开关配置
	registry           *pluginpkg.Registry          // 插件注册表
	ncmURL             string                       // 网易云音乐 API 地址（点歌插件使用）
	musicSendMode      string                       // 点歌发送方式：auto/card/link
	store              *media.ObjectStore           // RustFS 对象存储（表情库/入群欢迎插件使用，未配置时为 nil）
	vision             *ai.VisionService            // 视觉理解服务（表情库自动打标使用，未配置时为 nil）
	llmClient          llm.LLMClient                // LLM 客户端（海龟汤插件出题/判定使用，未配置时为 nil）
	turtleSoupTimeout  time.Duration                // 海龟汤出题/判定 LLM 调用独立超时（<=0 不限制）
	quizDir            string                       // 编程答题题库根目录
	randomBeautyConfig config.RandomBeautyConfig    // 随机美图插件配置
	logger             *zap.Logger
}

// NewBusinessRegistry 创建内置业务插件注册表。
func NewBusinessRegistry(cfg *config.PluginBuiltinsConfig, registry *pluginpkg.Registry, logger *zap.Logger) *BusinessRegistry {
	return &BusinessRegistry{
		cfg:      cfg,
		registry: registry,
		logger:   logger,
	}
}

// SetNCMURL 设置网易云音乐 API 地址（点歌插件依赖）。
func (r *BusinessRegistry) SetNCMURL(url string) {
	r.ncmURL = url
}

// SetMusicSendMode 设置点歌发送方式（auto/card/link，适配不同反向代理工具）。
func (r *BusinessRegistry) SetMusicSendMode(mode string) {
	r.musicSendMode = mode
}

// SetObjectStore 设置 RustFS 对象存储（表情库/入群欢迎插件依赖；未配置时收藏与欢迎图不可用）。
func (r *BusinessRegistry) SetObjectStore(store *media.ObjectStore) {
	r.store = store
}

// SetVisionService 设置视觉理解服务（表情库自动打标依赖；未配置时收藏表情不可用）。
func (r *BusinessRegistry) SetVisionService(v *ai.VisionService) {
	r.vision = v
}

// SetLLMClient 设置 LLM 客户端（海龟汤插件依赖；未配置时该插件命令提示不可用）。
func (r *BusinessRegistry) SetLLMClient(client llm.LLMClient) {
	r.llmClient = client
}

// SetTurtleSoupTimeout 设置海龟汤出题/判定 LLM 调用的独立超时（<=0 不限制）。
func (r *BusinessRegistry) SetTurtleSoupTimeout(timeout time.Duration) {
	r.turtleSoupTimeout = timeout
}

// SetQuizDir 设置编程答题题库根目录。
func (r *BusinessRegistry) SetQuizDir(dir string) {
	r.quizDir = dir
}

// SetRandomBeautyConfig 设置随机美图插件的上游与安全阈值配置。
func (r *BusinessRegistry) SetRandomBeautyConfig(cfg config.RandomBeautyConfig) {
	r.randomBeautyConfig = cfg
}

// RegisterBuiltins 按配置注册所有内置业务插件。
//
// 每个内置插件按以下规则处理：
//  1. 配置未启用 → 跳过并记录日志；
//  2. 注册表中已存在同名插件（Wasm 动态加载）→ 跳过并记录日志；
//  3. 否则注册内置实现。
//
// 任一插件注册失败立即返回错误（由调用方决定终止启动）。
func (r *BusinessRegistry) RegisterBuiltins() error {
	if r.registry == nil {
		return fmt.Errorf("bizplugin: 插件注册表为空，无法注册内置插件")
	}
	logger := r.logger
	if logger == nil {
		logger = zap.NewNop()
	}

	// builtins 开关（配置为空时按默认开启处理）
	builtins := r.cfg
	if builtins == nil {
		builtins = &config.PluginBuiltinsConfig{}
	}

	// 注册一个内置插件（启用开关 + Wasm 去重 + 注册）
	register := func(sw bool, pluginID string, plugin pluginpkg.Plugin) error {
		if !sw {
			logger.Info("bizplugin: 插件已由配置关闭，跳过注册", zap.String("plugin", pluginID))
			return nil
		}
		if _, loaded := r.registry.Get(pluginID); loaded {
			logger.Info("bizplugin: 插件已由 Wasm 动态加载，跳过内置注册", zap.String("plugin", pluginID))
			return nil
		}
		if err := r.registry.Register(plugin); err != nil {
			return fmt.Errorf("bizplugin: 注册 %s 插件失败: %w", pluginID, err)
		}
		return nil
	}

	// ── 签到插件 ──
	if err := register(builtins.Signin, "signin", NewSigninPlugin(logger)); err != nil {
		return err
	}

	// ── 入群欢迎插件 ──
	if err := register(builtins.Welcome, "welcome", NewWelcomePlugin(r.store, logger)); err != nil {
		return err
	}

	// ── 戳一戳回复插件 ──
	if err := register(builtins.Poke, "poke", NewPokePlugin(logger)); err != nil {
		return err
	}

	// ── 3G 关键词科普插件 ──
	if err := register(builtins.ThreeG, "three_g", NewThreeGPlugin(logger)); err != nil {
		return err
	}

	// ── 招新群导航插件 ──
	if err := register(builtins.ZhaoxinGroup, "zhaoxin_group", NewZhaoxinGroupPlugin(logger)); err != nil {
		return err
	}

	// ── 签到积分排行榜插件 ──
	if err := register(builtins.Rank, "signin_rank", NewRankPlugin(logger)); err != nil {
		return err
	}

	// ── 猫猫图片插件 ──
	if err := register(builtins.Cat, "cat", NewCatPlugin()); err != nil {
		return err
	}

	// ── 蔚蓝档案 LOGO 插件 ──
	if err := register(builtins.BaLogo, "balogo", NewBaLogoPlugin()); err != nil {
		return err
	}

	// ── Ping 连通性测试插件 ──
	if err := register(builtins.Ping, "ping", NewPingPlugin()); err != nil {
		return err
	}

	// ── GitHub 链接卡片插件 ──
	if err := register(builtins.GitHubCard, "github_card", NewGitHubCardPlugin()); err != nil {
		return err
	}

	// ── 网易云点歌插件 ──
	if err := register(builtins.Music, "music", NewMusicPlugin(r.ncmURL, r.musicSendMode, logger)); err != nil {
		return err
	}

	// ── 自定义表情库插件 ──
	if err := register(builtins.Sticker, "sticker", NewStickerPlugin(r.store, r.vision, logger)); err != nil {
		return err
	}

	// ── 海龟汤文字游戏插件 ──
	if err := register(builtins.TurtleSoup, "turtle_soup", NewTurtleSoupPlugin(r.llmClient, logger, r.turtleSoupTimeout)); err != nil {
		return err
	}

	// ── 编程答题游戏插件──
	if err := register(builtins.AnswerQuestion, "answer_question", NewAnswerQuestionPlugin(r.quizDir)); err != nil {
		return err
	}

	// ── 每日一句插件 ──
	if err := register(builtins.DailyQuote, "daily_quote", NewDailyQuotePlugin(logger)); err != nil {
		return err
	}

	// ── 严格安全审核的 Pixiv 随机美图插件 ──
	if !builtins.RandomBeauty {
		if err := register(false, "random_beauty", nil); err != nil {
			return err
		}
	} else if _, loaded := r.registry.Get("random_beauty"); loaded {
		logger.Info("bizplugin: 插件已由 Wasm 动态加载，跳过内置注册", zap.String("plugin", "random_beauty"))
	} else {
		p, err := randombeauty.New(r.randomBeautyConfig, r.vision, logger)
		if err != nil {
			return fmt.Errorf("bizplugin: 创建 random_beauty 插件失败: %w", err)
		}
		if err := register(true, "random_beauty", p); err != nil {
			return err
		}
	}

	return nil
}
