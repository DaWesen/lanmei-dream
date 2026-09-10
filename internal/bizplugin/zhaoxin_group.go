package bizplugin

import (
	"fmt"
	"strings"

	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// ============================================================
// ZhaoxinGroupPlugin 招新群导航插件
// ============================================================

// ZhaoxinGroupPlugin 实现招新群导航：用户在询问招新群号时，
// 回复蓝山工作室各部门招新交流群群号列表。
//
// 功能：
//   - 消息含招新相关关键词（如"招新群"、"招新交流群"）时直接触发
//   - 自然语言询问（如"招新群号是什么"）经意图分析路由到 /招新群 命令后触发
//   - 回复 [@对方 + 招新群群号列表] 一条消息（经出站段通道发送），at 与文本间保留一个空格
//   - 不做防刷限流
//
// 行为树：
//
//	subtree.zhaoxin_group → Selector [
//	  Sequence(ContainsZhaoxinKeyword, Action("pipeline.plugin.zhaoxin_group.main"))
//	  Sequence(IsZhaoxinCommand,      Action("pipeline.plugin.zhaoxin_group.main"))
//	]
//
// 管线（动态模式，支持运行时热替换）：
//
//	pipeline.plugin.zhaoxin_group.main → [zhaoxinGroupPass]
//
// 消息文本读取：插件不依赖 bot/gateway 包，直接读 MessageContext 的 RawMsg（原始文本）。
type ZhaoxinGroupPlugin struct {
	logger *zap.Logger
}

// NewZhaoxinGroupPlugin 创建招新群导航插件。
func NewZhaoxinGroupPlugin(logger *zap.Logger) *ZhaoxinGroupPlugin {
	return &ZhaoxinGroupPlugin{logger: logger}
}

// Info 返回招新群导航插件元信息。
// 声明 /招新群 命令供意图分析路由（"招新群号是什么" → command=招新群），
// 与关键词直触发两条路径最终汇入同一管线。
func (p *ZhaoxinGroupPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "zhaoxin_group",
		Name:        "招新群导航",
		Description: "用户询问招新群号时回复蓝山工作室各部门招新交流群群号",
		Version:     "1.0.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "招新群", Description: "查看蓝山工作室各部门招新交流群群号（产品/UI/Java/前端/GO/Python/运维/安全）", Order: 85},
		},
		SubtreeID: pluginpkg.SubtreeID("zhaoxin_group"),
	}
}

// OnInit 初始化招新群导航插件，注册 Pass、Pipeline 和 Subtree。
func (p *ZhaoxinGroupPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	// 注册 Pass（依赖直接注入 Pass 结构体）
	passID := pluginpkg.PassID("zhaoxin_group", "navigate")
	pass := &zhaoxinGroupPass{logger: p.logger}

	if err := ctx.Engine.RegisterPass(passID, pass); err != nil {
		return fmt.Errorf("register zhaoxin_group pass: %w", err)
	}

	// 跟踪 Pass，卸载时自动清理
	ctx.Registry.TrackPass("zhaoxin_group", passID)

	// 注册动态管线（通过 Pass ID 引用，支持运行时热替换）
	pipelineID := pluginpkg.PipelineID("zhaoxin_group", "main")
	pl := conduit.NewPipelineFromIDs(pipelineID, passID)
	if err := ctx.Engine.RegisterPipeline(pl); err != nil {
		return fmt.Errorf("register pipeline: %w", err)
	}

	// 跟踪 Pipeline，卸载时自动清理
	ctx.Registry.TrackPipeline("zhaoxin_group", pipelineID)

	// 注册行为树子树：关键词或 /招新群 命令双路触发
	subtree := conduit.NewSelector(
		conduit.NewSequence(
			conduit.NewCondition(containsZhaoxinKeyword),
			conduit.NewAction(pipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isZhaoxinCommand),
			conduit.NewAction(pipelineID),
		),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("zhaoxin_group"), subtree); err != nil {
		return fmt.Errorf("register subtree: %w", err)
	}

	return nil
}

// OnStart 招新群导航插件无需后台任务。
func (p *ZhaoxinGroupPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 招新群导航插件无需清理资源。
func (p *ZhaoxinGroupPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// ============================================================
// 条件判断
// ============================================================

// zhaoxinKeywords 招新相关触发关键词。
// 仅覆盖招新语境（"招新群"子串已含"招新群号"、"招新群是多少"等），
// 不匹配宽泛的"群号"等词，最大限度防误触发。
var zhaoxinKeywords = []string{"招新群", "招新交流群"}

// containsZhaoxinKeyword 判断消息原始文本是否包含招新相关关键词。
// 事件消息（notice）无文本内容，自然不满足条件；段落重入消息不作触发源
// （回复文案含"招新交流群"，否则蓝妹自回复会再次命中自己）。
func containsZhaoxinKeyword(ctx *conduit.MessageContext) bool {
	if isSegmentReentry(ctx) {
		return false
	}
	for _, kw := range zhaoxinKeywords {
		if strings.Contains(ctx.RawMsg, kw) {
			return true
		}
	}
	return false
}

// isZhaoxinCommand 判断消息是否为 /招新群 命令。
// 同时服务两条触发路径：用户显式输入，以及意图分析路由后的重入消息
// （LLM 判 command=招新群 → makeCommandHandler 构造 "/招新群" 重入）。
func isZhaoxinCommand(ctx *conduit.MessageContext) bool {
	return strings.TrimSpace(ctx.RawMsg) == "/招新群"
}

// ============================================================
// Pass 实现
// ============================================================

// zhaoxinGroupMessage 招新群群号列表（固定文案）
const zhaoxinGroupMessage = `✨ 蓝山工作室招新交流群 ✨
📦 产品及运营部：1103609889
🎨 UI 设计部：741857248
☕ Java组：1105629671
🌸 前端组：497827001
🐹 GO组：1103863004
🐍Python 组：1090172304
🛠️ 运维组：1102569474
🔒 安全组：1074152809`

// zhaoxinGroupPass 触发招新群导航回复：[@对方 + 招新群群号列表] 一条消息。
// 经出站段通道（conduit.Set "bot.send.segments"）交给 bot 回调按段发送；
// 事件缺 UserID（异常情况）时降级为纯文本文案。
type zhaoxinGroupPass struct {
	logger *zap.Logger
}

func (pass *zhaoxinGroupPass) Execute(ctx *conduit.MessageContext) error {
	pass.logger.Info("zhaoxin_group: 触发招新群导航",
		zap.String("user", ctx.UserID),
		zap.String("group", ctx.GroupID),
	)

	// 缺 UserID（异常情况）：降级纯文本文案，不 @ 任何人
	if ctx.UserID == "" {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: zhaoxinGroupMessage,
		})
		return nil
	}

	// 出站段：[@对方 + 群号列表]，at 与文本间保留一个空格（text 段开头空格实现），
	// 永远按 OneBot 12 语义组装（at 段用 user_id），协议差异由 bot 回调经 hub.SendSegments 收敛
	conduit.Set(ctx, sendSegmentsKey, []map[string]any{
		{"type": "at", "data": map[string]any{"user_id": ctx.UserID}},
		{"type": "text", "data": map[string]any{"text": " " + zhaoxinGroupMessage}},
	})
	return nil
}