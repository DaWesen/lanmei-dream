package plugin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/DaWesen/lanmei-dream/internal/ai/tool"
	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/database"
	"github.com/cloudwego/eino/schema"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// ============================================================
// Registry 插件注册表
// ============================================================

// Registry 管理所有插件的生命周期，是插件系统的核心。
//
// 职责：
//   - 维护已注册插件的列表
//   - 按序调用插件的 OnInit → OnStart 生命周期
//   - 反序调用插件的 OnStop 进行清理
//   - 动态重建主行为树，将插件子树挂载到决策流程中
//   - 跟踪每个插件注册的 Pass/Pipeline/Subtree/Command，卸载时自动清理
//
// 使用流程：
//
//	reg := plugin.NewRegistry(engine, store, db, cmdSys)
//	reg.Register(&SigninPlugin{})
//	reg.Register(&FestivalPlugin{})
//	reg.InitPlugins(ctx)  // 初始化所有插件
//	reg.StartPlugins(ctx) // 启动所有插件
//	// ... 运行 ...
//	reg.StopPlugins(ctx)  // 停止所有插件
type Registry struct {
	mu sync.RWMutex

	// engine Conduit 引擎，用于注册/注销 Pass、Pipeline、Subtree
	engine *conduit.Engine
	// store 全局状态存储
	store conduit.StateStore
	// kv 受限键值存储（PostgreSQL 后端，持久化；可为 nil）
	kv *database.PluginKVStore
	// db 数据库访问层
	db *database.DB
	// cmdSys 命令系统
	cmdSys *command.System
	// toolReg AI 工具注册表
	toolReg *tool.Registry
	// logger 日志记录器
	logger *zap.Logger

	// plugins 已注册的插件实例（按注册顺序）
	plugins []Plugin
	// pluginState 每个插件的运行状态
	pluginState map[string]pluginState

	// rebuildBT 行为树重建回调，由 Bot 层设置
	// 当插件注册/卸载后，Registry 调用此函数通知 Bot 重建主行为树
	rebuildBT func()
}

// pluginState 记录插件的运行状态和注册的资源
type pluginState struct {
	state        stateKind // 当前状态
	subtreeID    string    // 注册的子树 ID（空表示无子树）
	passIDs      []string  // 注册的 Pass ID 列表
	pipelineIDs  []string  // 注册的 Pipeline ID 列表
	commandNames []string  // 注册的命令名列表
	toolNames    []string  // 注册的工具名列表
}

type stateKind int

const (
	stateRegistered  stateKind = iota // 已注册但未初始化
	stateInitialized                  // 已初始化（OnInit 已调用）
	stateStarted                      // 已启动（OnStart 已调用）
	stateStopped                      // 已停止
)

// NewRegistry 创建插件注册表。
// engine 可以为 nil，稍后通过 SetEngine 设置（适用于引擎在 Bot.New 中创建的场景）。
func NewRegistry(engine *conduit.Engine, store conduit.StateStore, db *database.DB, cmdSys *command.System, toolReg *tool.Registry, logger *zap.Logger) *Registry {
	return &Registry{
		engine:      engine,
		store:       store,
		db:          db,
		cmdSys:      cmdSys,
		toolReg:     toolReg,
		logger:      logger,
		pluginState: make(map[string]pluginState),
	}
}

// SetEngine 设置 Conduit 引擎。
// 当引擎在 Registry 创建之后才初始化时（如 bot.New 中），通过此方法注入。
func (r *Registry) SetEngine(engine *conduit.Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine = engine
}

// SetKVStore 注入插件受限键值存储（PostgreSQL 持久化）。
// 在 InitPlugins 之前调用；不设置时插件的 PluginContext.KV 为 nil。
func (r *Registry) SetKVStore(kv *database.PluginKVStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kv = kv
}

// SetRebuildBT 设置行为树重建回调。
// Bot 层在创建 Registry 后调用此方法，传入行为树重建函数。
// 当插件注册/卸载导致子树变化时，Registry 通过此回调通知 Bot 重建主行为树。
func (r *Registry) SetRebuildBT(fn func()) {
	r.rebuildBT = fn
}

// ============================================================
// 注册与卸载
// ============================================================

// Register 注册一个插件到注册表。
// 插件 ID 不可重复，否则返回错误。
// 注册后需调用 InitPlugins 进行初始化。
func (r *Registry) Register(p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	info := p.Info()
	if info.ID == "" {
		return fmt.Errorf("plugin: ID must not be empty")
	}
	if _, exists := r.pluginState[info.ID]; exists {
		return fmt.Errorf("plugin: %q already registered", info.ID)
	}

	r.plugins = append(r.plugins, p)
	r.pluginState[info.ID] = pluginState{state: stateRegistered}
	r.logger.Info("plugin registered", zap.String("id", info.ID), zap.String("name", info.Name), zap.String("version", info.Version))
	return nil
}

// Unregister 卸载一个插件。
// 如果插件已启动，会先调用 OnStop 停止，然后清理所有注册的资源。
func (r *Registry) Unregister(pluginID string) error {
	// 在锁内快照插件信息
	r.mu.Lock()

	idx := -1
	for i, p := range r.plugins {
		if p.Info().ID == pluginID {
			idx = i
			break
		}
	}
	if idx == -1 {
		r.mu.Unlock()
		return fmt.Errorf("plugin: %q not found", pluginID)
	}

	p := r.plugins[idx]
	st := r.pluginState[pluginID]
	wasStarted := st.state == stateStarted

	// 先标记为已停止，防止并发操作
	st.state = stateStopped
	r.pluginState[pluginID] = st

	r.mu.Unlock()

	// 如果已启动，先停止（在锁外调用，避免 OnStop 内部访问 Registry 导致死锁）
	if wasStarted {
		pCtx := r.newPluginContext()
		if err := p.OnStop(pCtx); err != nil {
			r.logger.Error("plugin OnStop failed", zap.String("id", pluginID), zap.Error(err))
		}
	}

	// 清理注册的资源（engine 方法不需要 Registry 锁）
	r.cleanupResources(pluginID, st)

	// 重新获取锁，从列表移除
	r.mu.Lock()
	// 重新查找索引（可能在释放锁期间发生变化）
	idx = -1
	for i, pp := range r.plugins {
		if pp.Info().ID == pluginID {
			idx = i
			break
		}
	}
	if idx != -1 {
		r.plugins = slices.Delete(r.plugins, idx, idx+1)
	}
	delete(r.pluginState, pluginID)
	r.mu.Unlock()

	r.logger.Info("plugin unregistered", zap.String("id", pluginID))

	// 通知重建行为树（在锁外调用，避免 rebuildBT → SubtreeRefs → r.mu.RLock 死锁）
	if r.rebuildBT != nil {
		r.rebuildBT()
	}

	return nil
}

// ============================================================
// 生命周期
// ============================================================

// InitPlugin 初始化单个已注册插件。
func (r *Registry) InitPlugin(ctx context.Context, pluginID string) error {
	r.mu.RLock()
	var p Plugin
	for _, candidate := range r.plugins {
		if candidate.Info().ID == pluginID {
			p = candidate
			break
		}
	}
	st, exists := r.pluginState[pluginID]
	r.mu.RUnlock()
	if p == nil || !exists {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	if st.state != stateRegistered {
		return nil
	}

	info := p.Info()
	if err := p.OnInit(r.newPluginContextWith(ctx)); err != nil {
		r.mu.RLock()
		failedState := r.pluginState[pluginID]
		r.mu.RUnlock()
		failedState.subtreeID = info.SubtreeID
		r.cleanupResources(pluginID, failedState)
		return fmt.Errorf("plugin %q OnInit failed: %w", pluginID, err)
	}

	cmdNames := make([]string, 0, len(info.Commands))
	for _, cmd := range info.Commands {
		if err := r.cmdSys.Register(command.Command{
			Name:        cmd.Name,
			Description: cmd.Description,
			Handler:     r.makeCommandHandler(p, cmd),
			Order:       cmd.Order,
		}); err != nil {
			r.mu.RLock()
			failedState := r.pluginState[pluginID]
			r.mu.RUnlock()
			failedState.subtreeID = info.SubtreeID
			failedState.commandNames = cmdNames
			r.cleanupResources(pluginID, failedState)
			return fmt.Errorf("plugin %q register command %q failed: %w", pluginID, cmd.Name, err)
		}
		cmdNames = append(cmdNames, cmd.Name)
	}

	// Register tools if PluginInfo has them
	toolNames := make([]string, 0, len(info.Tools))
	if len(info.Tools) > 0 && r.toolReg != nil {
		for _, td := range info.Tools {
			toolInfo := &schema.ToolInfo{
				Name:        td.Name,
				Desc:        td.Description,
				ParamsOneOf: td.Parameters,
			}
			t := &tool.Tool{Info: toolInfo, Handler: td.Handler}
			if err := r.toolReg.Register(t); err != nil {
				r.mu.RLock()
				failedState := r.pluginState[pluginID]
				r.mu.RUnlock()
				failedState.subtreeID = info.SubtreeID
				failedState.commandNames = cmdNames
				failedState.toolNames = toolNames
				r.cleanupResources(pluginID, failedState)
				r.logger.Warn("插件工具注册失败", zap.String("plugin", pluginID), zap.String("tool", td.Name), zap.Error(err))
				continue
			}
			toolNames = append(toolNames, td.Name)
		}
	}

	r.mu.Lock()
	st = r.pluginState[pluginID]
	st.state = stateInitialized
	st.subtreeID = info.SubtreeID
	st.commandNames = cmdNames
	st.toolNames = toolNames
	r.pluginState[pluginID] = st
	r.mu.Unlock()

	r.logger.Info("plugin initialized", zap.String("id", pluginID), zap.Strings("commands", cmdNames), zap.Strings("tools", toolNames))
	if r.rebuildBT != nil {
		r.rebuildBT()
	}
	return nil
}

// StartPlugin 启动单个已初始化插件。
func (r *Registry) StartPlugin(ctx context.Context, pluginID string) error {
	r.mu.RLock()
	var p Plugin
	for _, candidate := range r.plugins {
		if candidate.Info().ID == pluginID {
			p = candidate
			break
		}
	}
	st, exists := r.pluginState[pluginID]
	r.mu.RUnlock()
	if p == nil || !exists {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	if st.state == stateStarted {
		return nil
	}
	if st.state != stateInitialized {
		return fmt.Errorf("plugin %q 未初始化", pluginID)
	}
	if err := p.OnStart(r.newPluginContextWith(ctx)); err != nil {
		return fmt.Errorf("plugin %q OnStart failed: %w", pluginID, err)
	}
	r.mu.Lock()
	if current, ok := r.pluginState[pluginID]; ok && current.state == stateInitialized {
		current.state = stateStarted
		r.pluginState[pluginID] = current
	}
	r.mu.Unlock()
	r.logger.Info("plugin started", zap.String("id", pluginID))
	return nil
}

// StopPlugin 停止单个已启动插件，不注销资源。
func (r *Registry) StopPlugin(ctx context.Context, pluginID string) error {
	r.mu.RLock()
	var p Plugin
	for _, candidate := range r.plugins {
		if candidate.Info().ID == pluginID {
			p = candidate
			break
		}
	}
	st, exists := r.pluginState[pluginID]
	r.mu.RUnlock()
	if p == nil || !exists {
		return fmt.Errorf("plugin: %q not found", pluginID)
	}
	if st.state != stateStarted {
		return nil
	}
	if err := p.OnStop(r.newPluginContextWith(ctx)); err != nil {
		return fmt.Errorf("plugin %q OnStop failed: %w", pluginID, err)
	}
	r.mu.Lock()
	if current, ok := r.pluginState[pluginID]; ok && current.state == stateStarted {
		current.state = stateStopped
		r.pluginState[pluginID] = current
	}
	r.mu.Unlock()
	r.logger.Info("plugin stopped", zap.String("id", pluginID))
	return nil
}

// InitPlugins 初始化所有已注册插件。
func (r *Registry) InitPlugins(ctx context.Context) error {
	r.mu.RLock()
	ids := make([]string, 0, len(r.plugins))
	for _, p := range r.plugins {
		ids = append(ids, p.Info().ID)
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if err := r.InitPlugin(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// StartPlugins 启动所有已初始化插件。
func (r *Registry) StartPlugins(ctx context.Context) error {
	r.mu.RLock()
	ids := make([]string, 0, len(r.plugins))
	for _, p := range r.plugins {
		ids = append(ids, p.Info().ID)
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if err := r.StartPlugin(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// StopPlugins 按注册逆序停止所有已启动插件。
func (r *Registry) StopPlugins(ctx context.Context) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.plugins))
	for i := len(r.plugins) - 1; i >= 0; i-- {
		ids = append(ids, r.plugins[i].Info().ID)
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if err := r.StopPlugin(ctx, id); err != nil {
			r.logger.Error("plugin OnStop failed", zap.String("id", id), zap.Error(err))
		}
	}
}

// ============================================================
// 行为树管理
// ============================================================

// SubtreeRefs 返回所有已初始化插件的 SubtreeRef 节点列表。
// Bot 层在构建主行为树时，将这些 SubtreeRef 插入到核心分支之前，
// 使插件的路由优先级高于核心逻辑。
//
// 调用时机：Bot 构建或重建主行为树时。
func (r *Registry) SubtreeRefs() []*conduit.SubtreeRef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var refs []*conduit.SubtreeRef
	for _, p := range r.plugins {
		info := p.Info()
		st := r.pluginState[info.ID]
		if st.state < stateInitialized {
			continue
		}
		if st.subtreeID != "" {
			refs = append(refs, r.engine.NewSubtreeRef(st.subtreeID))
		}
	}
	return refs
}

// ============================================================
// 查询
// ============================================================

// List 返回所有已注册插件的信息列表。
func (r *Registry) List() []PluginInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]PluginInfo, 0, len(r.plugins))
	for _, p := range r.plugins {
		infos = append(infos, p.Info())
	}
	return infos
}

// Get 根据插件 ID 获取插件实例。
func (r *Registry) Get(pluginID string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, p := range r.plugins {
		if p.Info().ID == pluginID {
			return p, true
		}
	}
	return nil, false
}

// State 返回插件的当前状态。
func (r *Registry) State(pluginID string) (stateKind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	st, ok := r.pluginState[pluginID]
	if !ok {
		return stateRegistered, false
	}
	return st.state, true
}

// StateName 返回插件的可读状态名（供管理面板展示）。
func (r *Registry) StateName(pluginID string) string {
	st, ok := r.State(pluginID)
	if !ok {
		return "not_loaded"
	}
	switch st {
	case stateRegistered:
		return "registered"
	case stateInitialized:
		return "initialized"
	case stateStarted:
		return "started"
	case stateStopped:
		return "stopped"
	}
	return "unknown"
}

// ============================================================
// 内部方法
// ============================================================

// newPluginContext 创建不带超时的 PluginContext（用于生命周期调用）。
func (r *Registry) newPluginContext() *PluginContext {
	return &PluginContext{
		Engine:   r.engine,
		Store:    r.store,
		KV:       r.kv,
		DB:       r.db,
		CmdSys:   r.cmdSys,
		Registry: r,
		ToolReg:  r.toolReg,
		Logger:   r.logger,
		Ctx:      context.Background(),
	}
}

// newPluginContextWith 使用指定 context 创建 PluginContext。
func (r *Registry) newPluginContextWith(ctx context.Context) *PluginContext {
	return &PluginContext{
		Engine:   r.engine,
		Store:    r.store,
		KV:       r.kv,
		DB:       r.db,
		CmdSys:   r.cmdSys,
		Registry: r,
		ToolReg:  r.toolReg,
		Logger:   r.logger,
		Ctx:      ctx,
	}
}

// makeCommandHandler 将插件的命令处理包装为 command.Handler。
//
// 插件命令的实际执行由行为树子树路由到插件管线完成，
// 此 handler 仅作为 command.System 中的注册占位（供帮助列表和 Lookup 查询使用）。
// 当通过意图分析路由到插件命令时，handler 会通过插件管线执行命令逻辑。
func (r *Registry) makeCommandHandler(p Plugin, cmd CommandDef) func(ctx *command.Context) error {
	return func(cmdCtx *command.Context) error {
		pluginID := p.Info().ID

		// 防重入死循环：当插件子树未匹配（如命令带插件不支持的参数）时，
		// 重入消息会再次落入"命令分支 → 本 handler → 重入"的无限递归。
		// 重入消息由下方 Extra 中的重入标记标识，第二次进入直接报错退出。
		if cmdCtx.CommandReentry {
			return fmt.Errorf("plugin %q command %q reentry loop detected", pluginID, cmd.Name)
		}

		// 还原消息上下文键（键名与 bot 包 Key* 常量保持一致），
		// 保证重入后的消息在插件子树 / 记忆层 / 话题层能拿到正确的
		// 平台、机器人自身 ID（平台 ID）与 at 目标（平台 ID 列表）。
		extra := map[string]any{
			"plugin_id":    pluginID,
			"command_name": cmd.Name,
			// 重入标记：插件子树消费后不再进入命令分支
			"bot.command.reentry": true,
			// ── 消息上下文（bot 包 KeyPlatform/KeyPlatformUserID/...）──
			"platform":         cmdCtx.Platform,
			"platform_user_id": cmdCtx.PlatformUserID,
			"nickname":         cmdCtx.Nickname,
			"message_id":       cmdCtx.MessageID,
			"conn_id":          cmdCtx.ConnID,
			"self_id":          cmdCtx.SelfID,
			// bot.message_type = "message"（gateway.MessageTypeMessage）：命令必然为普通消息
			"bot.message_type": "message",
			"bot.at_targets":   cmdCtx.AtTargets,
		}
		if wasmPlugin, ok := p.(*WasmPlugin); ok {
			extra["installation_id"] = wasmPlugin.InstallationID()
		}

		// 构建输入内容：
		// - 斜杠命令（如 /签到）：直接使用原始消息，插件子树可匹配
		// - 自然语言触发（如 "我要签到"，通过意图分析路由至此）：
		//   构造斜杠命令形式 "/签到"，确保插件子树能正确匹配，
		//   避免以原始内容重新进入引擎导致再次落入意图分析形成死循环。
		content := cmdCtx.Message
		if !strings.HasPrefix(content, "/") {
			content = "/" + cmd.Name
			if len(cmdCtx.CommandArgs) > 0 {
				content += " " + strings.Join(cmdCtx.CommandArgs, " ")
			}
		}

		input := &conduit.InputMessage{
			UserID:  cmdCtx.PlatformUserID,
			GroupID: cmdCtx.GroupID,
			Content: content,
			IsGroup: cmdCtx.IsGroup,
			Extra:   extra,
		}
		result, err := r.engine.Process(input)
		if err != nil {
			return fmt.Errorf("plugin %q command %q process failed: %w", pluginID, cmd.Name, err)
		}
		// 同步 Process 不触发消息级 ResponseCallback，因此插件写入子上下文的
		// OneBot 原生段必须显式回传。否则自然语言触发的 at 等段会丢失。
		if segments, ok := conduit.Get[[]map[string]any](result, "bot.send.segments"); ok && len(segments) > 0 && cmdCtx.ReplySegments != nil {
			cmdCtx.ReplySegments(segments)
		}
		if suppressAt, ok := conduit.Get[bool](result, "bot.reply.suppress_requester_at"); ok && suppressAt && cmdCtx.SuppressRequesterAt != nil {
			cmdCtx.SuppressRequesterAt()
		}
		for _, msg := range result.Output {
			cmdCtx.Reply(msg.Content)
		}
		return nil
	}
}

// cleanupResources 清理插件注册的所有资源。
func (r *Registry) cleanupResources(pluginID string, st pluginState) {
	if st.subtreeID != "" {
		if err := r.engine.UnregisterSubtree(st.subtreeID); err != nil {
			r.logger.Warn("failed to unregister subtree", zap.String("id", st.subtreeID), zap.Error(err))
		}
	}
	for _, id := range st.pipelineIDs {
		if err := r.engine.UnregisterPipeline(id); err != nil {
			r.logger.Warn("failed to unregister pipeline", zap.String("id", id), zap.Error(err))
		}
	}
	for _, id := range st.passIDs {
		if err := r.engine.UnregisterPass(id); err != nil {
			r.logger.Warn("failed to unregister pass", zap.String("id", id), zap.Error(err))
		}
	}
	for _, name := range st.commandNames {
		r.cmdSys.Unregister(name)
	}
	if r.toolReg != nil {
		for _, toolName := range st.toolNames {
			r.toolReg.Unregister(toolName)
		}
	}
}

// TrackPass 记录插件注册的 Pass ID。
func (r *Registry) TrackPass(pluginID, passID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pluginState[pluginID]
	st.passIDs = append(st.passIDs, passID)
	r.pluginState[pluginID] = st
}

// TrackPipeline 记录插件注册的 Pipeline ID。
func (r *Registry) TrackPipeline(pluginID, pipelineID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pluginState[pluginID]
	st.pipelineIDs = append(st.pipelineIDs, pipelineID)
	r.pluginState[pluginID] = st
}

// TrackTool 记录插件注册的工具名。
func (r *Registry) TrackTool(pluginID, toolName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.pluginState[pluginID]
	st.toolNames = append(st.toolNames, toolName)
	r.pluginState[pluginID] = st
}
