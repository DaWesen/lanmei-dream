package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zrurf/conduit"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"github.com/DaWesen/lanmei-dream/internal/ai/intent"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/config"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/DaWesen/lanmei-dream/internal/gateway"
	"github.com/DaWesen/lanmei-dream/internal/media"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/DaWesen/lanmei-dream/internal/topic"
)

// Bot 封装 Conduit 引擎 + 网关服务 + 意图分析器引用（用于动态更新命令/工具列表）
type Bot struct {
	engine        *conduit.Engine
	plugins       *pluginpkg.Registry
	gw            *gateway.Server
	analyzer      *intent.Analyzer               // 意图分析器引用，供插件加载后刷新命令/工具列表
	cmdSys        *command.System                // 命令系统引用
	toolReg       *tool.Registry                 // 工具注册表引用
	dedup         *Deduper                       // 消息去重（message_id SETNX）
	typingSpeedMS int                            // 打字速度（毫秒/字），0 禁用间隔
	minIntervalMS int                            // 最小发送间隔（毫秒）
	maxIntervalMS int                            // 最大发送间隔（毫秒），0 不限
	jitterPct     float64                        // 间隔抖动比例（0.0-1.0）
	superUsers    map[string]map[string]struct{} // 超管集合：平台 → 用户ID（静态配置 + 动态添加合并）
	adminMu       sync.RWMutex                   // 保护 superUsers 的并发读写
	sessionMu     sync.Mutex                     // 保护 sessions 的并发读写
	sessions      map[string]*sessionInfo        // 会话最近消息（供"回复前已有新消息→引用+at"判定）
	db            *database.DB                   // 数据库访问（动态管理员持久化；nil 时动态添加不可用）
	objectStore   *media.ObjectStore             // RustFS 对象存储（nil 时内网图片转 base64 发送不可用）
	logger        *zap.Logger

	// ── 管理面板控制平面 ──
	btMu           sync.RWMutex                     // 保护 btRoot 引用
	btRoot         conduit.BTNode                   // 行为树根节点引用（供面板快照/可视化）
	traceSink      TraceSink                        // 执行链路 Trace 落库回调（面板注入；nil 不采集）
	condMu         sync.RWMutex                     // 保护条件注册表
	condByName     map[string]conduit.ConditionFunc // 命名条件：名称 → 判断函数（供面板编辑行为树时引用）
	condByInstance map[*conduit.Condition]string    // 实例 → 名称（供面板快照时反查条件名）
}

// TraceMeta 消息级元数据，供 Trace 落库关联（面板审计日志）。
type TraceMeta struct {
	MessageID string
	UserID    string
	GroupID   string
	Platform  string
}

// TraceSink 执行链路 Trace 采集回调（由管理面板注入；nil 表示不采集）。
// 在消息处理回调中调用：ctx 内 trace 已完成（可经 conduit.GetTraceResult 读取），
// err 为管线处理错误（nil 表示成功），meta 为消息关联元数据。
type TraceSink func(ctx *conduit.MessageContext, err error, meta TraceMeta)

// SetTraceSink 注入 Trace 采集回调（面板启动时调用）。
func (b *Bot) SetTraceSink(sink TraceSink) {
	b.traceSink = sink
}

// setBehaviorTree 设置主行为树并记录根节点引用（供面板快照），并发安全。
func (b *Bot) setBehaviorTree(root conduit.BTNode) {
	b.btMu.Lock()
	b.btRoot = root
	b.btMu.Unlock()
	b.engine.SetBehaviorTree(root)
}

// BehaviorTree 返回当前行为树根节点（供管理面板快照/可视化）。
func (b *Bot) BehaviorTree() conduit.BTNode {
	b.btMu.RLock()
	defer b.btMu.RUnlock()
	return b.btRoot
}

// SetBehaviorTree 设置主行为树（管理面板应用编辑后调用；内部记录根节点引用）。
func (b *Bot) SetBehaviorTree(root conduit.BTNode) {
	b.setBehaviorTree(root)
}

// Engine 返回底层 Conduit 引擎（供管理面板控制平面使用）。
func (b *Bot) Engine() *conduit.Engine {
	return b.engine
}

// Plugins 返回插件注册表（供管理面板插件管理）。
func (b *Bot) Plugins() *pluginpkg.Registry {
	return b.plugins
}

// RegisterCondition 注册命名条件（供面板编辑行为树时按名引用；重名覆盖）。
// 核心条件（IsSegment/IsNotice/IsAdminCommand/IsCommand/IsMedia）在 New 中自动注册。
func (b *Bot) RegisterCondition(name string, fn conduit.ConditionFunc) {
	b.condMu.Lock()
	b.condByName[name] = fn
	b.condMu.Unlock()
}

// Condition 按名称解析条件函数；未注册返回 false。
func (b *Bot) Condition(name string) (conduit.ConditionFunc, bool) {
	b.condMu.RLock()
	fn, ok := b.condByName[name]
	b.condMu.RUnlock()
	return fn, ok
}

// Conditions 返回全部已注册条件名（供面板下拉选择）。
func (b *Bot) Conditions() []string {
	b.condMu.RLock()
	defer b.condMu.RUnlock()
	names := make([]string, 0, len(b.condByName))
	for name := range b.condByName {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ConditionName 反查条件实例的名称（供面板快照还原语义）；未登记返回空串。
func (b *Bot) ConditionName(cond *conduit.Condition) string {
	b.condMu.RLock()
	defer b.condMu.RUnlock()
	return b.condByInstance[cond]
}

// newCondition 创建命名条件节点并登记映射（面板快照/编辑均依赖该映射）。
// 同一条件实例在行为树重建间复用，映射按实例指针稳定。
func (b *Bot) newCondition(name string, fn conduit.ConditionFunc) *conduit.Condition {
	cond := conduit.NewCondition(fn)
	b.condMu.Lock()
	b.condByName[name] = fn
	b.condByInstance[cond] = name
	b.condMu.Unlock()
	return cond
}

// emitTrace 在消息处理回调中采集执行链路 Trace（无 sink 时零开销）。
func (b *Bot) emitTrace(ctx *conduit.MessageContext, err error, msg *gateway.NormalizedMessage) {
	if b.traceSink == nil || ctx == nil {
		return
	}
	b.traceSink(ctx, err, TraceMeta{
		MessageID: msg.MessageID,
		UserID:    msg.UserID,
		GroupID:   msg.GroupID,
		Platform:  string(msg.Platform),
	})
}

// RefreshIntentAnalyzer 在插件注册新的命令或工具后同步更新意图分析器。
// 插件初始化完成后调用此方法，确保 LLM 意图分析能感知最新可用的命令和工具。
func (b *Bot) RefreshIntentAnalyzer() {
	cmdDefs := BuildIntentCommands(b.cmdSys)
	toolDefs := BuildIntentTools(b.toolReg)
	b.analyzer.UpdateCommands(cmdDefs)
	b.analyzer.UpdateTools(toolDefs)
	b.logger.Info("intent: 分析器已刷新",
		zap.Int("commands", len(cmdDefs)),
		zap.Int("tools", len(toolDefs)),
	)
}

// MediaDeps 多媒体处理依赖（可选注入，nil 时媒体管线降级工作）。
type MediaDeps struct {
	Store  *media.ObjectStore // RustFS 对象存储（nil 时跳过媒体缓存）
	Vision *ai.VisionService  // 视觉理解（nil 时图片仅缓存不描述）
}

// New 创建 Bot 实例，初始化 Conduit 引擎、行为树和插件系统。
//
// store 为 Conduit 状态存储（由 infra 包提供，RedisStore 用于生产，MemoryStore 用于测试）
// llmClient 为 LLM 客户端，用于意图分析（nil 时降级为纯聊天路由）
// pluginReg 为插件注册表（nil 时跳过插件初始化）
// gwServer 为网关服务端（反向 WS，由 gateway 包提供）
// mediaDeps 为多媒体处理依赖（nil 时媒体管线降级，仅记录）
// topicMgr 为群聊话题管理器（nil 时 topic 系统未启用，群聊退化为全量意图分析）
func New(cfg *config.BotConfig, cmdSys *command.System, chatSvc *ai.ChatService, db *database.DB, store conduit.StateStore, llmClient llm.LLMClient, pluginReg *pluginpkg.Registry, gwServer *gateway.Server, toolReg *tool.Registry, logger *zap.Logger, mediaDeps *MediaDeps, topicMgr *topic.Manager) *Bot {
	nick := cfg.NickName
	if nick == "" {
		nick = "蓝妹"
	}

	// ── Conduit 引擎 ──
	// 超时 20s：意图分析/命令等慢路径（如 LLM 慢响应）在超时前完成，
	// 避免"慢 LLM 调用超时被丢弃"导致群聊消息静默。
	// WithTracing(true)：启用执行链路追踪，供管理面板 Trace 审计采集。
	engine := conduit.New(store,
		conduit.WithWorkers(4),
		conduit.WithTimeout(20*time.Second),
		conduit.WithFallbackPipeline("pipeline.fallback"),
		conduit.WithTracing(true),
	)

	// ── 构建意图分析器（LLM 不可用时自动降级为 IntentChat）──
	// intentTimeout：意图分析独立短超时（默认 8s），LLM 故障时快速降级，
	// 避免吃满整条消息的 20s 预算触发"迷糊"兜底回复。
	intentTimeout := time.Duration(cfg.IntentTimeoutSeconds) * time.Second
	cmdDefs := BuildIntentCommands(cmdSys)
	toolDefs := BuildIntentTools(toolReg)
	analyzer := intent.NewAnalyzer(llmClient, cmdDefs, toolDefs, intentTimeout)

	// ── 注册 Pass 与管线 ──
	// 核心管线全部以"动态管线"（PassID 引用）注册：
	// 面板可可视化编辑管线 Pass 顺序（只替换 PassID 列表，Pass 实例复用），
	// Pass 替换时引擎自动失效对应管线解析缓存（热更新）。
	// 管理员命令管线：AdminGuardPass 校验超管身份，非超管拦截不执行命令
	engine.MustRegisterPass("pass.admin.guard", &AdminGuardPass{})
	engine.MustRegisterPass("pass.admin.command", &CommandPass{CmdSys: cmdSys})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.admin",
		"pass.admin.guard", "pass.admin.command",
	))

	engine.MustRegisterPass("pass.command.router", &CommandRouterPass{CmdSys: cmdSys})
	engine.MustRegisterPass("pass.command.execute", &ExecuteCommandPass{})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.command",
		"pass.command.router", "pass.command.execute",
	))

	// IntentAnalysisPass 是 RouterPass — Execute 执行分析，Route 动态路由
	engine.MustRegisterPass("pass.intent.analysis", &IntentAnalysisPass{
		Analyzer: analyzer,
		ChatSvc:  chatSvc, // nil 时 IntentChat/IntentTool 走 fallback
		logger:   logger,
	})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.intent_analysis",
		"pass.intent.analysis",
	))

	// 当 chatSvc 为 nil（LLM 未配置）时不注册对话管线
	if chatSvc != nil {
		engine.MustRegisterPass("pass.roleplay.stream", &RoleplayStreamPass{Chat: chatSvc, DB: db, Logger: logger, TopicMgr: topicMgr})
		engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.roleplay",
			"pass.roleplay.stream",
		))
	}

	// 流式段落交付管线（每个段落作为子消息重入引擎后走此管线）
	engine.MustRegisterPass("pass.roleplay.segment", &RoleplaySegmentPass{})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.roleplay_segment",
		"pass.roleplay.segment",
	))

	engine.MustRegisterPass("pass.intent.ignore", &IntentIgnorePass{DB: db})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.intent_ignore",
		"pass.intent.ignore",
	))

	engine.MustRegisterPass("pass.intent.command_exec", &IntentCommandExecPass{CmdSys: cmdSys})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.intent_command_exec",
		"pass.intent.command_exec",
	))

	// ── 互动事件预留节点（具体逻辑由插件子树实现）──
	engine.MustRegisterPass("pass.notice.gate", &NoticeGatePass{Logger: logger})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.notice",
		"pass.notice.gate",
	))

	// ── 多媒体处理管线（下载/缓存/理解 → RouterPass 路由）──
	var mediaStore *media.ObjectStore
	var visionSvc *ai.VisionService
	if mediaDeps != nil {
		mediaStore = mediaDeps.Store
		visionSvc = mediaDeps.Vision
	}
	engine.MustRegisterPass("pass.media.process", &MediaPass{Store: mediaStore, Vision: visionSvc, DB: db, Cfg: &cfg.Media, Logger: logger})
	engine.MustRegisterPass("pass.media.router", &MediaRouterPass{})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.media",
		"pass.media.process", "pass.media.router",
	))

	engine.MustRegisterPass("pass.fallback", &FallbackPass{})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.fallback",
		"pass.fallback",
	))

	// ── 群聊话题管线（选择性放行；topicMgr 为 nil 时 TopicGatePass 全放行）──
	engine.MustRegisterPass("pass.topic.gate", &TopicGatePass{Manager: topicMgr, Analyzer: analyzer, Logger: logger})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.topic_gate",
		"pass.topic.gate",
	))
	engine.MustRegisterPass("pass.topic.ignore", &TopicIgnorePass{DB: db})
	engine.MustRegisterPipeline(conduit.NewPipelineFromIDs("pipeline.topic_ignore",
		"pass.topic.ignore",
	))

	// ── 创建 Bot 实例 ──
	// 先于行为树构建：条件注册表（condByName/condByInstance）由 newCondition 填充，
	// 供管理面板快照还原条件语义、编辑行为树时按名引用。
	b := &Bot{
		engine:         engine,
		plugins:        pluginReg,
		gw:             gwServer,
		analyzer:       analyzer,
		cmdSys:         cmdSys,
		toolReg:        toolReg,
		dedup:          NewDeduper(store, 0),
		typingSpeedMS:  cfg.Stream.TypingSpeedMS,
		minIntervalMS:  cfg.Stream.MinIntervalMS,
		maxIntervalMS:  cfg.Stream.MaxIntervalMS,
		jitterPct:      cfg.Stream.JitterPct,
		superUsers:     parseSuperUsers(cfg.SuperUsers),
		sessions:       make(map[string]*sessionInfo),
		db:             db,
		objectStore:    mediaStore,
		logger:         logger,
		condByName:     make(map[string]conduit.ConditionFunc),
		condByInstance: make(map[*conduit.Condition]string),
	}

	// ── 行为树核心分支 ──
	// 段落分支优先级最高：流式段落重入消息直接走交付管线，不经过意图分析。
	// 条件节点统一使用命名条件（b.newCondition）：实例与名称双向登记，快照可还原。
	coreSegment := conduit.NewSequence(
		b.newCondition("IsSegment", IsSegment),
		conduit.NewAction("pipeline.roleplay_segment"),
	)
	// 互动事件分支：notice/request 事件路由到预留节点（插件子树可先行消费）
	coreNotice := conduit.NewSequence(
		b.newCondition("IsNotice", IsNotice),
		conduit.NewAction("pipeline.notice"),
	)
	coreAdmin := conduit.NewSequence(
		b.newCondition("IsAdminCommand", IsAdminCommand),
		conduit.NewAction("pipeline.admin"),
	)
	coreCommand := conduit.NewSequence(
		b.newCondition("IsCommand", IsCommand),
		conduit.NewAction("pipeline.command"),
	)
	// 多媒体分支：含图片/音频/视频/文件段的消息先经 pipeline.media 处理
	// （MediaRouterPass 内部再路由到 intent_analysis / intent_ignore）
	coreMedia := conduit.NewSequence(
		b.newCondition("IsMedia", IsMedia),
		conduit.NewAction("pipeline.media"),
	)

	// 意图感知路由核心（非命令消息）：
	// 群聊先经 pipeline.topic_gate 做话题决策（RouterPass 路由到 intent_analysis / topic_ignore），
	// 私聊由 TopicGatePass 直接放行到 intent_analysis。
	// 使用 RouterPass 简化行为树 —— Execute 执行分析，Route 动态路由到对应管线，
	// 不再需要 Condition 节点（避免 BT Tick 时分析结果尚未写入的时序问题）。
	coreIntent := conduit.NewAction("pipeline.topic_gate")

	// ── 插件系统 ──
	if pluginReg != nil {
		pluginReg.SetEngine(engine)
		pluginReg.SetRebuildBT(func() {
			b.setBehaviorTree(buildBehaviorTree(pluginReg, coreAdmin, coreCommand, coreIntent, coreSegment, coreNotice, coreMedia))
		})
	}

	b.setBehaviorTree(buildBehaviorTree(pluginReg, coreAdmin, coreCommand, coreIntent, coreSegment, coreNotice, coreMedia))

	// 合并持久化的动态管理员（重启不丢）并注册管理员命令
	b.loadDynamicAdmins()
	b.registerAdminCommands()

	return b
}

// isSuperUser 判断 (platform, userID) 是否在超管集合中（静态配置 + 动态添加合并，并发安全）。
func (b *Bot) isSuperUser(platform, userID string) bool {
	b.adminMu.RLock()
	defer b.adminMu.RUnlock()
	if m, ok := b.superUsers[platform]; ok {
		_, ok := m[userID]
		return ok
	}
	return false
}

// AddSuperUser 向内存超管集合添加一个成员（并发安全）。
// 调用方需先完成持久化，保证内存与存储一致。
func (b *Bot) AddSuperUser(platform, userID string) {
	if platform == "" || userID == "" {
		return
	}
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.superUsers[platform] == nil {
		b.superUsers[platform] = map[string]struct{}{}
	}
	b.superUsers[platform][userID] = struct{}{}
}

// loadDynamicAdmins 启动时从 bot_admin 表加载动态管理员并入内存集合。
// DB 不可用时仅记录错误（静态配置超管不受影响），不阻断启动。
func (b *Bot) loadDynamicAdmins() {
	if b.db == nil {
		return
	}
	admins, err := b.db.ListAdmins(context.Background())
	if err != nil {
		b.logger.Error("bot: 加载动态管理员失败", zap.Error(err))
		return
	}
	for _, a := range admins {
		b.AddSuperUser(a.Platform, a.UserID)
	}
	if len(admins) > 0 {
		b.logger.Info("bot: 动态管理员已加载", zap.Int("count", len(admins)))
	}
}

// ── 管理员管理命令 ──

// registerAdminCommands 注册管理员管理命令（/admin 帮助入口 + /添加管理员）。
func (b *Bot) registerAdminCommands() {
	// /admin 作为管理员管线（pipeline.admin）入口：IsAdminCommand 匹配 /admin 前缀后
	// 由 CommandPass 执行，必须注册对应命令，否则报 unknown command: admin。
	_ = b.cmdSys.Register(command.Command{
		Name:        "admin",
		Description: "管理员帮助（仅超管），列出可用管理员命令",
		Handler:     b.handleAdminHelp,
		Order:       140,
	})
	_ = b.cmdSys.Register(command.Command{
		Name:        "添加管理员",
		Description: "添加管理员（仅超管），格式：/添加管理员 [平台:]用户ID，如 /添加管理员 qq:123456",
		Handler:     b.handleAddAdmin,
		Order:       160,
	})
}

// handleAdminHelp 处理 /admin：列出当前可用的管理员命令。
func (b *Bot) handleAdminHelp(cmdCtx *command.Context) error {
	if !cmdCtx.IsSuperUser {
		cmdCtx.Reply("只有管理员才能查看管理员命令哦~")
		return nil
	}
	cmdCtx.Reply("📋 管理员命令：\n" +
		"  /添加管理员 [平台:]用户ID — 添加管理员（如 /添加管理员 qq:123456）")
	return nil
}

// handleAddAdmin 处理 /添加管理员：
// 超管校验 → 解析 [平台:]用户ID → 幂等写库 → 更新内存集合。
// 顺序保证一致性：先持久化成功再更新内存，DB 失败则内存不变。
func (b *Bot) handleAddAdmin(cmdCtx *command.Context) error {
	if !cmdCtx.IsSuperUser {
		cmdCtx.Reply("只有管理员才能添加管理员哦~")
		return nil
	}
	if b.db == nil {
		cmdCtx.Reply("数据库不可用，无法添加管理员")
		return nil
	}

	platform, userID, err := parseAdminArg(cmdCtx)
	if err != nil {
		cmdCtx.Reply(err.Error())
		return nil
	}

	existed, err := b.db.AddAdmin(context.Background(), platform, userID,
		cmdCtx.Platform+":"+cmdCtx.PlatformUserID)
	if err != nil {
		b.logger.Error("bot: 添加管理员写库失败",
			zap.String("platform", platform), zap.String("user", userID), zap.Error(err))
		cmdCtx.Reply("添加管理员失败，请稍后重试")
		return nil
	}
	// 写库成功（或已存在）后再更新内存集合，保证内存与存储一致
	b.AddSuperUser(platform, userID)
	if existed {
		cmdCtx.Reply(fmt.Sprintf("%s:%s 已经是管理员啦", platform, userID))
		return nil
	}
	cmdCtx.Reply(fmt.Sprintf("已添加管理员 ✅ %s:%s", platform, userID))
	return nil
}

// knownPlatforms 合法平台标识（与 gateway.Platform 常量保持一致）。
var knownPlatforms = map[string]struct{}{"qq": {}, "napcat": {}, "wechat": {}, "telegram": {}}

// parseAdminArg 解析 /添加管理员 参数：[平台:]用户ID。
//   - 带冒号（如 "qq:123456"）：校验平台合法性；
//   - 裸用户ID（如 "123456"）：默认取当前消息平台；当前平台未知（unknown）时要求显式带平台前缀。
func parseAdminArg(cmdCtx *command.Context) (platform, userID string, err error) {
	raw := strings.TrimSpace(strings.Join(cmdCtx.CommandArgs, " "))
	if raw == "" {
		return "", "", errors.New("格式错误！用法：/添加管理员 [平台:]用户ID，如 /添加管理员 qq:123456")
	}
	if strings.Contains(raw, ":") {
		parts := strings.SplitN(raw, ":", 2)
		platform = strings.TrimSpace(parts[0])
		userID = strings.TrimSpace(parts[1])
		if _, ok := knownPlatforms[platform]; !ok {
			return "", "", fmt.Errorf("未知平台「%s」，支持 qq/napcat/wechat/telegram", platform)
		}
	} else {
		platform = cmdCtx.Platform
		userID = strings.TrimSpace(raw)
		if _, ok := knownPlatforms[platform]; !ok {
			return "", "", fmt.Errorf("无法确定平台，请用 /添加管理员 [平台:]用户ID 指定，如 qq:%s", userID)
		}
	}
	if userID == "" {
		return "", "", errors.New("用户 ID 不能为空")
	}
	return platform, userID, nil
}

// buildBehaviorTree 构建主行为树。
//
// 结构：
//
//	Selector [
//	  SubtreeRef("plugin.signin.subtree")    // 插件子树（优先级最高）
//	  SubtreeRef("plugin.festival.subtree")  // ...
//	  SubtreeRef("plugin.minigame.subtree")  // ...
//	  Sequence [IsSegment, Action(segment)]  // 流式段落重入（优先于命令/意图）
//	  Sequence [IsNotice, Action(notice)]    // 互动事件（预留节点）
//	  Sequence [IsAdminCommand, Action(admin)]  // 管理员命令
//	  Sequence [IsCommand, Action(command)]  // 斜杠命令
//	  Sequence [IsMedia, Action(media)]      // 多媒体（内部路由到意图分析/忽略）
//	  Action(intent)                         // 意图分析（兜底）
//	]
func buildBehaviorTree(pluginReg *pluginpkg.Registry, coreAdmin, coreCommand, coreIntent, coreSegment, coreNotice, coreMedia conduit.BTNode) conduit.BTNode {
	var branches []conduit.BTNode

	// 插件子树优先（最先匹配，最高优先级）
	if pluginReg != nil {
		for _, ref := range pluginReg.SubtreeRefs() {
			branches = append(branches, ref)
		}
	}

	// 段落分支优先于命令/意图（流式段落直接交付，不走分析）
	branches = append(branches, coreSegment)

	// 核心分支：事件 → 管理员命令 → 斜杠命令 → 媒体 → 意图分析（兜底）
	branches = append(branches, coreNotice, coreAdmin, coreCommand, coreMedia, coreIntent)

	return conduit.NewBehaviorTree(branches...)
}

// Run 启动 Conduit 引擎 + 网关服务，阻塞运行
func (b *Bot) Run() {
	b.engine.Start()
	defer func() { _ = b.engine.Stop() }()

	if err := b.gw.Run(); err != nil {
		b.logger.Fatal("gateway: 服务运行失败", zap.Error(err))
	}
}

// OnMessage 实现 gateway.EventHandler 接口，将网关消息转为 Conduit 输入。
// 使用异步 Submit + ResponseCallback，避免阻塞网关事件处理。
//
// 入口职责：
//  1. 空文本消息过滤（但含媒体段或为事件的消息放行）
//  2. message_id 去重（Deduper）
//  3. 将完整事件上下文（含多模态段 / notice 信息）注入 InputMessage.Extra
//
// 通知事件（notice/request）无文本内容，与普通消息分流：
// 事件信息经 Extra 写入黑板供插件消费；事件不产生回复，出错也保持静默。
func (b *Bot) OnMessage(msg *gateway.NormalizedMessage) {
	if msg == nil {
		return
	}
	// 普通消息必须非空（纯图片/事件消息无文本时放行，交给媒体/事件管线处理）
	if msg.MessageType == gateway.MessageTypeMessage && msg.Content == "" && len(msg.Segments) == 0 {
		return
	}
	// ── 消息去重：重复 message_id 直接丢弃（存储故障时放行）──
	if b.dedup != nil && !b.dedup.Accept(msg) {
		b.logger.Debug("bot: 重复消息已丢弃",
			zap.String("conn", msg.ConnID), zap.String("message_id", msg.MessageID))
		return
	}

	// ── 用户封禁拦截：被封禁用户的全部消息静默丢弃（不进入行为树）──
	if b.db != nil {
		banned, err := b.db.IsUserBanned(context.Background(), string(msg.Platform), msg.UserID)
		if err != nil {
			b.logger.Warn("bot: 查询封禁状态失败，放行消息",
				zap.String("user", msg.UserID), zap.Error(err))
		} else if banned {
			b.logger.Debug("bot: 用户已被封禁，消息丢弃",
				zap.String("user", msg.UserID), zap.String("group", msg.GroupID))
			return
		}
	}

	// 记录会话最近消息（供"回复前会话已有新消息 → 引用并 at"判定）
	b.recordSession(msg)

	// 超管身份需计算后注入（Extra 其余字段均为 msg 直取）
	isSuperUser := b.isSuperUser(string(msg.Platform), msg.UserID)

	input := &conduit.InputMessage{
		UserID:  msg.UserID,
		GroupID: msg.GroupID,
		Content: msg.Content,
		IsGroup: msg.IsGroup,
		Extra: map[string]any{
			KeyPlatform:       string(msg.Platform),
			KeyPlatformUserID: msg.UserID,
			KeyNickname:       msg.SenderName,
			KeyMessageID:      msg.MessageID,
			KeyConnID:         msg.ConnID,
			KeySelfID:         msg.SelfID,
			KeyIsSuperUser:    isSuperUser,
			KeyImageURLs:      msg.ImageURLs,
			// ── 事件输入（只读 Extra）──
			KeyMessageType:  msg.MessageType,
			KeySegments:     msg.Segments,
			KeyMimeTypes:    msg.MimeTypes,
			KeyAtTargets:    msg.AtTargets,
			KeyEventType:    msg.EventType,
			KeyEventSubType: msg.EventSubType,
			KeyEventData:    msg.EventData,
		},
	}
	// 事件消息（notice/request）：出错也绝不向群里发消息（事件落空发"迷糊话术"是 bug）
	isEvent := msg.MessageType == gateway.MessageTypeNotice || msg.MessageType == gateway.MessageTypeRequest
	if isEvent {
		input.ResponseCallback = b.makeEventCallback(msg)
	} else {
		input.ResponseCallback = b.makeResponseCallback(msg)
	}

	if err := b.engine.Submit(input); err != nil {
		b.logger.Error("conduit: submit failed", zap.Error(err))
		if !isEvent {
			b.reply(msg, "蓝妹现在有点迷糊，稍后再试~")
		}
	}
}

// makeEventCallback 构造通知事件专用回调：事件处理出错时只记日志、绝不回复；
// 正常完成时发送管线输出（出站段优先，纯文本兜底，供事件消费插件使用）。
// 无论成败均采集执行链路 Trace（审计面板可查看事件处理轨迹）。
func (b *Bot) makeEventCallback(msg *gateway.NormalizedMessage) func(*conduit.MessageContext, error) {
	return func(ctx *conduit.MessageContext, err error) {
		b.emitTrace(ctx, err, msg)
		if err != nil {
			b.logger.Error("bot: event process failed",
				zap.String("event", msg.EventType),
				zap.String("group", msg.GroupID),
				zap.Error(err),
			)
			return
		}
		b.flushOutput(ctx, msg)
	}
}

// makeResponseCallback 构造消息级回调，处理两种情况：
//   - 正常完成（未 yield）：发送管线输出（出站段优先，纯文本兜底）
//   - 流式挂起（yield）：启动 goroutine 消费段落通道，逐条重入引擎投递
//
// 无论成败均采集执行链路 Trace（管理面板实时查看节点状态/耗时/错误）。
func (b *Bot) makeResponseCallback(msg *gateway.NormalizedMessage) func(*conduit.MessageContext, error) {
	return func(ctx *conduit.MessageContext, err error) {
		if err != nil {
			b.emitTrace(ctx, err, msg)
			b.logger.Error("conduit: process failed", zap.Error(err))
			b.reply(msg, "蓝妹现在有点迷糊，稍后再试~")
			return
		}
		if conduit.IsYielded(ctx) {
			// 流式回复：Trace 在 yield 时已 finish，先采集再启动 goroutine 投递段落
			b.emitTrace(ctx, nil, msg)
			go b.streamSegments(ctx, msg)
			return
		}
		b.emitTrace(ctx, nil, msg)
		// 正常回复：发送所有输出消息
		b.flushOutput(ctx, msg)
	}
}

// flushOutput 发送管线产生的输出：出站段优先、纯文本兜底。
//
//   - 插件经 conduit.Set 写入出站段键（KeySendSegments）→ 按段列表发送（at/text/image 组合）
//   - 否则遍历 ctx.Output 逐条纯文本回复（历史行为，老插件零影响）
//
// 纯文本回复在群聊中若为明确指向性回复（命令/工具/at/话题提及），
// 会自动 at 请求者，防止"这回复给谁的"歧义（签到等插件回复即受益于此）。
func (b *Bot) flushOutput(ctx *conduit.MessageContext, msg *gateway.NormalizedMessage) {
	if b.trySendSegments(ctx, msg) {
		return
	}
	suppressRequesterAt, _ := conduit.Get[bool](ctx, KeySuppressRequesterAt)
	directed := !suppressRequesterAt && b.isDirected(ctx, msg)
	// 引用/at 只建立在首条文本消息上（anchorDone 标记），
	// 避免多输出回复（如签到插件输出两条）重复引用同一条消息、重复 at 同一人；
	// 图片消息无法携带引用段，不建立锚点，后续文本再补。
	anchorDone := false
	for _, out := range ctx.Output {
		if extractImageURL(out.Content) != "" {
			b.sendReply(msg, out.Content, replyOpts{})
			continue
		}
		b.sendReply(msg, out.Content, replyOpts{quoteIfStale: !anchorDone, atRequester: !anchorDone && directed})
		anchorDone = true
	}
}

// trySendSegments 尝试按插件写入的出站段列表发送。
//
// 插件永远按 OneBot 12 语义组装段（at 段用 user_id），段形状与 MessageSegmentV12 等价，
// 经 JSON 转换后复用 gateway.ParseSegmentsV12 转标准段，再经 hub.SendSegments 发出
// （v11 连接由 ToMessageSegmentV11 自动处理 at 段 user_id→qq 与协议动作）。
//
// 返回 true 表示段路径已接管输出（发送失败也只记日志，与 reply 一致）；
// 段键缺失 / 类型不符 / 段列表为空时返回 false，由调用方走纯文本兜底。
func (b *Bot) trySendSegments(ctx *conduit.MessageContext, msg *gateway.NormalizedMessage) bool {
	raw, ok := conduit.Get[[]map[string]any](ctx, KeySendSegments)
	if !ok || len(raw) == 0 {
		return false
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		b.logger.Warn("bot: 出站段序列化失败，降级纯文本", zap.Error(err))
		return false
	}
	var segs []gateway.MessageSegmentV12
	if err := json.Unmarshal(buf, &segs); err != nil {
		b.logger.Warn("bot: 出站段形状非法，降级纯文本", zap.Error(err))
		return false
	}
	if err := b.gw.Hub().SendSegments(msg.ConnID, msg, gateway.ParseSegmentsV12(segs)); err != nil {
		b.logger.Error("bot: 段消息发送失败", zap.String("conn", msg.ConnID), zap.Error(err))
	}
	return true
}

// streamSegments 消费段落通道，逐条创建子消息重入引擎。
//
// 每个段落通过 NewChildInput 派生子消息，标记 KeyIsSegment 后 Submit 到引擎。
// 段落顺序投递：前一段的回调完成后才提交下一段（<-done），保证天然时序。
// 段落间发送间隔由 calcSegmentInterval 按下一段字数动态计算，模拟真人打字节奏，
// 避免 QQ 等平台短时间内快速发送消息导致乱序。
// 流式 goroutine 关闭通道后，range 循环自然退出。
//
// 打字时间算法（v2）：间隔约束的是「距离上一次实际发送的时间」，而非
// "入队后必须等待这么长时间"。LLM 流式生成期间已经消耗了大量时间（用户感知为
// "正在打字"），因此后续段落到达时若距上次发送已超过打字间隔，则立即发送、
// 不再额外等待；仅当剩余等待为正时才 Sleep。首段在 LLM 生成期间已"打完字"，
// 直接发送（lastSentAt 以首段为基准启动计时）。
func (b *Bot) streamSegments(ctx *conduit.MessageContext, msg *gateway.NormalizedMessage) {
	segCh, ok := conduit.Get[chan string](ctx, KeyStreamChannel)
	if !ok {
		b.logger.Error("bot: stream channel not found in context")
		b.reply(msg, "蓝妹现在有点迷糊，稍后再试~")
		return
	}

	first := true
	// 明确指向性回复（at/话题提及/命令等）在群聊中首段 at 请求者（任务4）；
	// 计算一次复用，后续段落不再 at。
	directed := b.isDirected(ctx, msg)
	// lastSentAt 记录上一次段落实际发送完成的时间（在 <-done 后读取，线程安全）；
	// 首段发送后初始化，后续段落按 "lastSentAt + 打字间隔" 计算最早可发送时间。
	var lastSentAt time.Time
	for segment := range segCh {
		// 首段无延迟（LLM 生成期间已"打字"完毕）；后续段落等待「距上次发送」的
		// 打字间隔：若 LLM 生成耗时已超过打字间隔则直接发送，仅剩余时间为正才 Sleep。
		if !first && !lastSentAt.IsZero() {
			if d := b.calcSegmentInterval(segment); d > 0 {
				target := lastSentAt.Add(d)
				if wait := time.Until(target); wait > 0 {
					time.Sleep(wait)
				}
			}
		}
		// 快照当前段是否为首段（回调异步执行，不能引用会被后续迭代修改的 first）
		quoteThis := first
		first = false

		child := ctx.NewChildInput(segment)
		child.Extra[KeyIsSegment] = true

		// 段落回调：发送该段输出，完成后关闭 done 通知主循环。
		// 仅首段参与"会话已有新消息 → 引用+at"判定（引用只需建立一次锚点）与
		// 明确指向性 at；后续段落直接发送，不重复引用/at。
		done := make(chan struct{})
		child.ResponseCallback = func(childCtx *conduit.MessageContext, childErr error) {
			defer close(done)
			if childErr != nil {
				b.logger.Error("bot: segment delivery failed", zap.Error(childErr))
				return
			}
			for _, out := range childCtx.Output {
				b.sendReply(msg, out.Content, replyOpts{
					quoteIfStale: quoteThis,
					atRequester:  quoteThis && directed,
				})
			}
		}

		if err := b.engine.Submit(child); err != nil {
			b.logger.Error("bot: submit segment failed", zap.Error(err))
			close(done) // 不阻塞，继续下一段
			if errors.Is(err, conduit.ErrEngineStopped) {
				return // 引擎已停止，无需继续
			}
		}

		// 顺序等待：前一段发送完成后才投递下一段
		<-done
		// 上一段已发送完成，以此刻作为"距上次发送"的计时基准
		lastSentAt = time.Now()
	}
}

// calcSegmentInterval 根据段落文本长度计算发送间隔。
//
// 算法：基础间隔 = 字数 × typingSpeedMS，
// 先叠加 ±jitterPct 的随机抖动（模拟真人打字的不均匀节奏），
// 再 clamp 到 [minIntervalMS, maxIntervalMS]，
// 保证抖动后仍不超过上限（长消息不会被无限拖慢）。
//
// 返回 0 表示不延迟（typingSpeedMS 为 0 时禁用整个间隔机制）。
func (b *Bot) calcSegmentInterval(text string) time.Duration {
	if b.typingSpeedMS <= 0 {
		return 0
	}

	charCount := utf8.RuneCountInString(text)
	baseMS := charCount * b.typingSpeedMS

	// 叠加抖动：实际间隔 = base × (1 + uniform[-jitter, +jitter])
	if b.jitterPct > 0 {
		jitter := (rand.Float64()*2 - 1) * b.jitterPct // [-jitterPct, +jitterPct]
		baseMS = int(float64(baseMS) * (1 + jitter))
	}

	// clamp 到最小值
	if b.minIntervalMS > 0 && baseMS < b.minIntervalMS {
		baseMS = b.minIntervalMS
	}
	// clamp 到最大值（0 表示不设上限）
	if b.maxIntervalMS > 0 && baseMS > b.maxIntervalMS {
		baseMS = b.maxIntervalMS
	}

	return time.Duration(baseMS) * time.Millisecond
}

// reply 通过网关回复消息（通用入口）。
// 回复内容为纯 http(s) URL 时按图片消息发送（富媒体段），供 cat/balogo/github_card 等图片类插件使用。
// 附加行为：会话内已有新消息时引用原消息（quoteIfStale=true，群聊附带 at 原发送者）；
// 不主动 at 请求者（at 由调用方按明确指向性回复显式指定）。
func (b *Bot) reply(msg *gateway.NormalizedMessage, text string) {
	b.sendReply(msg, text, replyOpts{quoteIfStale: true})
}

// replyOpts 回复发送选项。
type replyOpts struct {
	// quoteIfStale 会话内已有新消息时，引用被回复的原消息（群聊附带 at 原发送者）。
	// 用于 LLM 长耗时回复期间会话出现插话的场景，防止"这条到底回复谁/哪条"的困惑。
	quoteIfStale bool
	// atRequester 群聊中 at 当前请求者（命令/插件/at/话题提及等明确指向性回复）。
	atRequester bool
}

// sendReply 发送文本回复，支持按需附加引用/at 前缀段（协议自适应）。
//
// 段组合顺序：reply(引用原消息) → at(原发送者，群聊) → at(请求者，明确指向性) → text。
// 引用与 at 均会去重：同一消息只会出现一次 at。
func (b *Bot) sendReply(msg *gateway.NormalizedMessage, text string, opts replyOpts) {
	// 剔除 markdown 代码块围栏（``` 边界行）：QQ 等平台不支持 markdown 渲染，
	// 直接发送代码原文，避免把 ``` 符号原样发给用户。
	text = stripCodeFences(text)

	if file := extractImageURL(text); file != "" {
		// 纯 URL → 图片消息（走上游富媒体发送体系）
		// 内网 rustfs 预签名 URL：外部 IM 客户端无法解析容器内网主机名，
		// 由 bot 下载对象内容转 base64 后发送（失败时按原 URL 降级）。
		if b.objectStore != nil {
			if uri, err := b.objectStore.ImageBase64FromURL(context.Background(), file); err != nil {
				b.logger.Warn("bot: 内网图片转 base64 失败，按 URL 降级发送", zap.String("url", file), zap.Error(err))
			} else if uri != "" {
				file = uri
			}
		}
		if err := b.gw.Hub().SendSegments(msg.ConnID, msg, []gateway.NormalizedSegment{{
			Type: "image",
			Data: map[string]string{"file": file},
		}}); err != nil {
			b.logger.Error("bot: 图片回复失败", zap.String("conn", msg.ConnID), zap.Error(err))
		}
		return
	}

	segs := make([]gateway.NormalizedSegment, 0, 3)
	atAdded := false

	// 会话内已有新消息：引用被回复的原消息（群聊附带 at 原发送者），
	// 明确告诉群友这条回复是对谁、对哪条消息的回应。
	if opts.quoteIfStale && b.hasNewerMessages(msg) {
		if msg.MessageID != "" {
			segs = append(segs, b.replyQuoteSegment(msg))
		}
		if msg.IsGroup && msg.UserID != "" {
			segs = append(segs, gateway.NormalizedSegment{Type: "at", Data: map[string]string{"user_id": msg.UserID}})
			atAdded = true
		}
	}

	// 明确指向性回复（命令/插件/at/话题提及）：群聊中 at 请求者，防止"这回复给谁的"歧义。
	// 已因引用而 at 过原发送者时不再重复 at。
	if opts.atRequester && msg.IsGroup && msg.UserID != "" && !atAdded {
		segs = append(segs, gateway.NormalizedSegment{Type: "at", Data: map[string]string{"user_id": msg.UserID}})
	}

	segs = append(segs, gateway.NormalizedSegment{Type: "text", Text: text})
	if err := b.gw.Hub().SendSegments(msg.ConnID, msg, segs); err != nil {
		b.logger.Error("bot: 回复失败", zap.String("conn", msg.ConnID), zap.Error(err))
	}
}

// replyQuoteSegment 构造协议自适应的引用（reply）消息段：
// OneBot 11（NapCat 方言）引用段用 id 字段；OneBot 12 规范用 message_id 字段。
func (b *Bot) replyQuoteSegment(msg *gateway.NormalizedMessage) gateway.NormalizedSegment {
	data := map[string]string{}
	if msg.Protocol == gateway.ProtocolV12 {
		data["message_id"] = msg.MessageID
	} else {
		data["id"] = msg.MessageID
	}
	return gateway.NormalizedSegment{Type: "reply", Data: data}
}

// ── 会话最近消息追踪（供"回复前会话已有新消息 → 引用并 at"判定）──

// sessionInfo 记录某会话最近一条入站消息，用于判定回复时是否已出现新消息。
type sessionInfo struct {
	LastMsgID string // 最近一条消息的 ID
	LastMsgAt time.Time
	IsGroup   bool
}

// sessionKey 生成会话键：群聊按 (平台, 群ID)，私聊按 (平台, 用户ID)。
func sessionKey(msg *gateway.NormalizedMessage) string {
	if msg == nil {
		return ""
	}
	if msg.IsGroup {
		return "g:" + string(msg.Platform) + ":" + msg.GroupID
	}
	return "p:" + string(msg.Platform) + ":" + msg.UserID
}

// recordSession 更新会话最近消息记录（仅普通消息；通知事件不参与）。
// 网关事件按序到达，但为防止乱序事件覆盖新消息，按到达时间取新。
// 机器人自身消息（网关若回显自己发出的回复）不参与追踪：
// 否则 bot 的回复会被记为"会话最新消息"，导致后续回复误判为"已有新消息"而全部引用+at。
func (b *Bot) recordSession(msg *gateway.NormalizedMessage) {
	if msg == nil || msg.MessageType != gateway.MessageTypeMessage {
		return
	}
	if msg.UserID != "" && msg.UserID == msg.SelfID {
		return // 机器人自身消息（回显），跳过
	}
	key := sessionKey(msg)
	if key == "" {
		return
	}
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if prev, ok := b.sessions[key]; ok {
		if !prev.LastMsgAt.IsZero() && !msg.ReceivedAt.IsZero() && msg.ReceivedAt.Before(prev.LastMsgAt) {
			return // 乱序的旧消息，不覆盖
		}
	}
	b.sessions[key] = &sessionInfo{
		LastMsgID: msg.MessageID,
		LastMsgAt: msg.ReceivedAt,
		IsGroup:   msg.IsGroup,
	}
}

// hasNewerMessages 判断触发回复的消息之后，会话中是否又出现了新消息。
// 优先用 message_id 精确比较；ID 缺失（平台未提供）时退化为到达时间比较。
func (b *Bot) hasNewerMessages(msg *gateway.NormalizedMessage) bool {
	if msg == nil || msg.MessageType != gateway.MessageTypeMessage {
		return false
	}
	key := sessionKey(msg)
	if key == "" {
		return false
	}
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	info, ok := b.sessions[key]
	if !ok {
		return false
	}
	if msg.MessageID != "" && info.LastMsgID != "" {
		return info.LastMsgID != msg.MessageID
	}
	if !msg.ReceivedAt.IsZero() && !info.LastMsgAt.IsZero() {
		return info.LastMsgAt.After(msg.ReceivedAt)
	}
	return false
}

// isDirected 判断本次回复是否为"明确指向某成员"的回复（群聊中需要 at 请求者）。
// 判定依据（任一命中即视为明确指向）：
//   - 消息 at 了机器人；
//   - 斜杠命令（如 /签到）；
//   - 意图为命令或工具（自然语言指令，如"帮我签到"）；
//   - 话题提及（at / 语言学提及）。
func (b *Bot) isDirected(ctx *conduit.MessageContext, msg *gateway.NormalizedMessage) bool {
	if ctx == nil || msg == nil || !msg.IsGroup {
		return false
	}
	if msg.SelfID != "" {
		for _, t := range msg.AtTargets {
			if t == msg.SelfID {
				return true
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(msg.Content), "/") {
		return true
	}
	if result, ok := conduit.Get[*intent.Result](ctx, intentResultKey); ok && result != nil {
		if result.Intent == intent.IntentCommand || result.Intent == intent.IntentTool {
			return true
		}
	}
	if mode, ok := conduit.Get[topic.MentionMode](ctx, KeyMentionMode); ok && mode != topic.MentionNone {
		return true
	}
	return false
}

// codeFenceRe 匹配 markdown 代码块围栏（``` 起始行 → ``` 结束行）。
// 兼容：开围栏可选语言标注；闭围栏可缺失（LLM 漏写结尾时从开围栏处剔除到末尾）。
var codeFenceRe = regexp.MustCompile("```[^\n]*\n?([\\s\\S]*?)\n?(?:```|\\z)")

// stripCodeFences 剔除文本中的 markdown 代码块围栏行，仅保留代码内容。
// 例如 "```go\nfmt.Println(\"hi\")\n```" → "fmt.Println(\"hi\")"。
// 代码块与后续文字之间的换行保留；未闭合代码块（LLM 漏写结尾 ```）原样保留内容。
func stripCodeFences(text string) string {
	return codeFenceRe.ReplaceAllStringFunc(text, func(m string) string {
		idx := codeFenceRe.FindStringSubmatchIndex(m)
		if idx == nil || idx[2] < 0 {
			return m
		}
		content := m[idx[2]:idx[3]]
		// 闭合代码块：去掉闭围栏前的换行；未闭合：保持原样
		if strings.HasSuffix(m, "```") {
			return strings.TrimRight(content, "\n")
		}
		return content
	})
}

// isImageURL 判断文本是否为纯图片 URL。
// 规则：去除首尾空白后是完整的 http(s) URL（不含内部空格/换行）。
func isImageURL(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
		return false
	}
	return !strings.ContainsAny(t, " \t\n\r")
}

// extractImageURL 从回复文本中提取图片 URL。
// LLM 常把工具返回的 URL 用 markdown 反引号包裹（inline code，如 `http://...`），
// 直接按纯文本发送会破坏图片段（NapCat 收不到图片）；此处剥离反引号与首尾空白后，
// 若为纯 http(s) URL 则返回清洗后的 URL，否则返回空串。
func extractImageURL(text string) string {
	t := strings.TrimSpace(text)
	// 剥离 markdown inline code 包裹（开头的 ` 与结尾的 `）
	t = strings.Trim(t, "`")
	t = strings.TrimSpace(t)
	if !isImageURL(t) {
		return ""
	}
	return t
}
