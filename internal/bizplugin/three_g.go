package bizplugin

import (
	"fmt"
	"strings"

	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// ============================================================
// ThreeGPlugin 3G 关键词科普插件
// ============================================================

// ThreeGPlugin 实现关键词触发科普：用户消息含 "3G"/"3g" 时，
// at 对方并发送重邮 3G 科普文本。
//
// 功能：
//   - 普通消息（含私聊）含关键词即触发，大小写两种形态都覆盖
//   - 回复 [@对方 + 固定科普文本] 一条消息（经出站段通道发送），at 与文本间保留一个空格
//   - 不做防刷限流
//
// 行为树：
//
//	subtree.three_g → Sequence(ContainsThreeG, Action("pipeline.plugin.three_g.main"))
//
// 管线（动态模式，支持运行时热替换）：
//
//	pipeline.plugin.three_g.main → [threeGPass]
//
// 消息文本读取：插件不依赖 bot/gateway 包，直接读 MessageContext 的 RawMsg（原始文本）。
type ThreeGPlugin struct {
	logger *zap.Logger
}

// NewThreeGPlugin 创建 3G 关键词科普插件。
func NewThreeGPlugin(logger *zap.Logger) *ThreeGPlugin {
	return &ThreeGPlugin{logger: logger}
}

// Info 返回 3G 关键词科普插件元信息。
func (p *ThreeGPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "three_g",
		Name:        "3G 科普",
		Description: "消息含 3G/3g 时科普重邮 3G 历史",
		Version:     "1.0.0",
		SubtreeID:   pluginpkg.SubtreeID("three_g"),
	}
}

// OnInit 初始化 3G 关键词科普插件，注册 Pass、Pipeline 和 Subtree。
func (p *ThreeGPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	// 注册 Pass（依赖直接注入 Pass 结构体）
	passID := pluginpkg.PassID("three_g", "three_g")
	pass := &threeGPass{logger: p.logger}

	if err := ctx.Engine.RegisterPass(passID, pass); err != nil {
		return fmt.Errorf("register three_g pass: %w", err)
	}

	// 跟踪 Pass，卸载时自动清理
	ctx.Registry.TrackPass("three_g", passID)

	// 注册动态管线（通过 Pass ID 引用，支持运行时热替换）
	pipelineID := pluginpkg.PipelineID("three_g", "main")
	pl := conduit.NewPipelineFromIDs(pipelineID, passID)
	if err := ctx.Engine.RegisterPipeline(pl); err != nil {
		return fmt.Errorf("register pipeline: %w", err)
	}

	// 跟踪 Pipeline，卸载时自动清理
	ctx.Registry.TrackPipeline("three_g", pipelineID)

	// 注册行为树子树：关键词触发路由
	subtree := conduit.NewSequence(
		conduit.NewCondition(containsThreeG),
		conduit.NewAction(pipelineID),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("three_g"), subtree); err != nil {
		return fmt.Errorf("register subtree: %w", err)
	}

	return nil
}

// OnStart 3G 关键词科普插件无需后台任务。
func (p *ThreeGPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 3G 关键词科普插件无需清理资源。
func (p *ThreeGPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// ============================================================
// 条件判断
// ============================================================

// keyIsSegment 段落重入标记，与 bot 包 KeyIsSegment 同值（插件"不导包"，用字面量）。
const keyIsSegment = "bot.is_segment"

// isSegmentReentry 判断消息是否为蓝妹流式回复的段落重入消息。
// 段落不作关键词触发源：否则插件抢占段落交付分支且出站段无人消费，回复静默丢失
// （如介绍蒋建华必含 "23Go"，子串命中 "3G"）。
func isSegmentReentry(ctx *conduit.MessageContext) bool {
	if raw, ok := ctx.Extra[keyIsSegment]; ok {
		if b, ok := raw.(bool); ok {
			return b
		}
	}
	return false
}

// containsThreeG 判断消息原始文本是否包含关键词 "3G" 或 "3g"。
// 事件消息（notice）无文本内容，自然不满足条件；段落重入消息不作触发源。
func containsThreeG(ctx *conduit.MessageContext) bool {
	if isSegmentReentry(ctx) {
		return false
	}
	return strings.Contains(ctx.RawMsg, "3G") || strings.Contains(ctx.RawMsg, "3g")
}

// ============================================================
// Pass 实现
// ============================================================

// threeGMessage 固定科普文案（at 与文本间的空格由 text 段开头空格实现）
const threeGMessage = `检测到您的语句中含有"3G"，触发关键词，我将为您科普重邮3G:
2005年10月9日下午2:30，重庆市人民政府新闻办公室在重庆市新闻发布中心举行了新闻发布会。会议郑重发布了"世界第一颗0.13微米工艺的TD-SCDMA 3G手机核心芯片在重庆诞生"这一令国人自豪和骄傲的重大喜讯。它是世界上第一颗采用0.13微米工艺的TD-SCDMA手机基带芯片，功耗低，内核尺寸小，成本低，标志着中国3G通信核心芯片的关键技术达到了世界领先水平。重邮信科"通芯一号"芯片是符合3GPP TD-SCDMA标准自主研发的手机芯片，它具有优良的总体构架和实现算法，经过了充分的仿真和验证，具有极高的性能和稳定性，可完成TD-SCDMA手机物理层、协议栈和应用软件所有处理工作。"通芯一号"芯片的开发成功，是邮电学院从1998年开始参与大唐电信为首组织的TD-SCDMA标准研究，并在2003年采用通用芯片独立开发出世界上第一部TD-SCDMA（TSM）手机后在TD-SCDMA自主创新上的又一重大突破，是重邮信科对TD-SCDMA产业化的重大贡献，标志着重邮信科在TD-SCDMA终端产业链上已经确立了重要的基础地位。`

// threeGPass 触发关键词回复：[@对方 + 固定科普文本] 一条消息。
// 经出站段通道（conduit.Set "bot.send.segments"）交给 bot 回调按段发送；
// 事件缺 UserID（异常情况）时降级为纯文本文案。
type threeGPass struct {
	logger *zap.Logger
}

func (pass *threeGPass) Execute(ctx *conduit.MessageContext) error {
	pass.logger.Info("three_g: 触发 3G 关键词",
		zap.String("user", ctx.UserID),
		zap.String("group", ctx.GroupID),
	)

	// 缺 UserID（异常情况）：降级纯文本文案，不 @ 任何人
	if ctx.UserID == "" {
		conduit.AppendOutput(ctx, &conduit.Message{
			UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
			Content: threeGMessage,
		})
		return nil
	}

	// 出站段：[@对方 + 科普文本]，at 与文本间保留一个空格（text 段开头空格实现），
	// 永远按 OneBot 12 语义组装（at 段用 user_id），协议差异由 bot 回调经 hub.SendSegments 收敛
	conduit.Set(ctx, sendSegmentsKey, []map[string]any{
		{"type": "at", "data": map[string]any{"user_id": ctx.UserID}},
		{"type": "text", "data": map[string]any{"text": " " + threeGMessage}},
	})
	return nil
}
