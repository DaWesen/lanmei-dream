package bizplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/database"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// ============================================================
// TurtleSoupPlugin 海龟汤插件
// ============================================================

// TurtleSoupPlugin 实现海龟汤（情境推理）文字游戏：
//   - /开汤（或 /海龟汤）：蓝妹用 LLM 生成一局谜题（汤面公开、汤底隐藏）
//   - /问 <问题>：向汤面提问，蓝妹只回答 是/否/无关，不作任何补充
//   - /猜 <答案>：尝试猜出汤底，命中则揭晓结算；未命中时仅按大方向给出
//     固定的"方向正确/猜错了"整体判断，不透露汤底任何细节
//   - /认输（或 /看汤底）：放弃并揭晓汤底
//
// 防混乱约束：**同一个群（或同一私聊）同时只能开一局**，未结束前 /开汤 会被拒绝；
// 提问只在有进行中的局时生效；局超过 12 小时自动作废。
//
// 局状态存插件受限 KV 存储（PostgreSQL 持久化，重启不丢），按群/私聊隔离。
//
// 行为树：
//
//	subtree.turtle_soup → Selector(
//	  Sequence(isOpenSoupCommand,  Action("pipeline.plugin.turtle_soup.main")),
//	  Sequence(isAskSoupCommand,   Action("pipeline.plugin.turtle_soup.main")),
//	  Sequence(isGuessSoupCommand, Action("pipeline.plugin.turtle_soup.main")),
//	  Sequence(isGiveUpSoupCommand,Action("pipeline.plugin.turtle_soup.main")),
//	)
//
// 管线：
//
//	pipeline.plugin.turtle_soup.main → [turtleSoupPass]
type TurtleSoupPlugin struct {
	llmClient llm.LLMClient
	kv        *database.PluginKVStore
	logger    *zap.Logger
	// timeout 出题/判定 LLM 调用的独立超时（<=0 不设独立超时，沿用消息级预算）。
	// 命令管线受消息级超时约束，给独立上限可避免 LLM 慢时耗尽预算触发"迷糊"兜底。
	timeout time.Duration
}

// NewTurtleSoupPlugin 创建海龟汤插件。llmClient 为 nil 时出题/判定不可用，
// 命令会提示配置缺失；timeout 为出题/判定 LLM 调用的独立超时（<=0 不限制）。
func NewTurtleSoupPlugin(llmClient llm.LLMClient, logger *zap.Logger, timeout time.Duration) *TurtleSoupPlugin {
	return &TurtleSoupPlugin{llmClient: llmClient, logger: logger, timeout: timeout}
}

// Info 返回海龟汤插件元信息。
func (p *TurtleSoupPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "turtle_soup",
		Name:        "海龟汤",
		Description: "海龟汤情境推理文字游戏（/开汤 开局，/问 提问，/猜 猜答案，/认输 揭晓）",
		Version:     "1.0.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "开汤", Description: "开始一局海龟汤（一个群同时只能开一局）", Order: 100},
			{Name: "问", Description: "向汤面提问，蓝妹回答 是/否/无关，格式：/问 问题", Order: 110},
			{Name: "猜", Description: "猜汤底，格式：/猜 答案", Order: 120},
			{Name: "认输", Description: "放弃本局并揭晓汤底", Order: 130},
		},
		SubtreeID: pluginpkg.SubtreeID("turtle_soup"),
	}
}

// OnInit 初始化海龟汤插件，注册 Pass、Pipeline 和 Subtree。
func (p *TurtleSoupPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.kv = ctx.KV

	passID := pluginpkg.PassID("turtle_soup", "main")
	pass := &turtleSoupPass{llmClient: p.llmClient, kv: p.kv, logger: p.logger, timeout: p.timeout, inFlight: map[string]bool{}}
	if err := ctx.Engine.RegisterPass(passID, pass); err != nil {
		return fmt.Errorf("register turtle_soup pass: %w", err)
	}
	ctx.Registry.TrackPass("turtle_soup", passID)

	pipelineID := pluginpkg.PipelineID("turtle_soup", "main")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(pipelineID, passID)); err != nil {
		return fmt.Errorf("register turtle_soup pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("turtle_soup", pipelineID)

	// 四个命令路由到同一个管线，Pass 内按命令前缀分发
	subtree := conduit.NewSelector(
		conduit.NewSequence(
			conduit.NewCondition(isOpenSoupCommand),
			conduit.NewAction(pipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isAskSoupCommand),
			conduit.NewAction(pipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isGuessSoupCommand),
			conduit.NewAction(pipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(isGiveUpSoupCommand),
			conduit.NewAction(pipelineID),
		),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("turtle_soup"), subtree); err != nil {
		return fmt.Errorf("register turtle_soup subtree: %w", err)
	}

	return nil
}

// OnStart 海龟汤插件无需后台任务。
func (p *TurtleSoupPlugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }

// OnStop 海龟汤插件无需清理资源。
func (p *TurtleSoupPlugin) OnStop(_ *pluginpkg.PluginContext) error { return nil }

// ============================================================
// 条件判断
// ============================================================

// isOpenSoupCommand 判断消息是否为开汤命令。
func isOpenSoupCommand(ctx *conduit.MessageContext) bool {
	msg := strings.TrimSpace(ctx.RawMsg)
	return msg == "/开汤" || msg == "/海龟汤"
}

// isAskSoupCommand 判断消息是否为提问命令（/问 前缀）。
func isAskSoupCommand(ctx *conduit.MessageContext) bool {
	return strings.HasPrefix(strings.TrimSpace(ctx.RawMsg), "/问 ")
}

// isGuessSoupCommand 判断消息是否为猜答案命令（/猜 前缀）。
func isGuessSoupCommand(ctx *conduit.MessageContext) bool {
	return strings.HasPrefix(strings.TrimSpace(ctx.RawMsg), "/猜 ")
}

// isGiveUpSoupCommand 判断消息是否为认输/揭晓命令。
func isGiveUpSoupCommand(ctx *conduit.MessageContext) bool {
	msg := strings.TrimSpace(ctx.RawMsg)
	return msg == "/认输" || msg == "/放弃" || msg == "/看汤底" || msg == "/汤底"
}

// ============================================================
// 局状态
// ============================================================

const (
	// turtleSoupPluginID 受限 KV 存储命名空间
	turtleSoupPluginID = "turtle_soup"
	// turtleSoupMaxAge 局最长存活时间（12 小时，超时自动作废）
	turtleSoupMaxAge = 12 * time.Hour
	// turtleSoupMaxQuestions 单局最大提问数（防止无限刷）
	turtleSoupMaxQuestions = 30
)

// turtleQA 一条历史问答
type turtleQA struct {
	Q string `json:"q"`
	A string `json:"a"`
}

// turtleGame 一局海龟汤状态
type turtleGame struct {
	SoupFace      string     `json:"soup_face"`      // 汤面（公开谜面）
	SoupBase      string     `json:"soup_base"`      // 汤底（答案，隐藏）
	Hints         []string   `json:"hints"`          // 判定要点（内部参考，不公开）
	QuestionCount int        `json:"question_count"` // 已提问次数
	Creator       string     `json:"creator"`        // 开局人
	CreatedAt     int64      `json:"created_at"`     // 开局时间戳
	QAPairs       []turtleQA `json:"qa_pairs"`       // 历史问答（判定上下文）
}

// soupKey 生成局的 KV 键：群聊按群 ID 隔离，私聊按用户 ID 隔离。
func soupKey(groupID, userID string) string {
	if groupID != "" {
		return "state:group:" + groupID
	}
	return "state:dm:" + userID
}

// loadGame 读取当前局的进行状态；无局或已过期返回 nil。
func loadGame(kv *database.PluginKVStore, ctx context.Context, groupID, userID string) *turtleGame {
	if kv == nil {
		return nil
	}
	key := soupKey(groupID, userID)
	raw, err := kv.Get(ctx, turtleSoupPluginID, key)
	if err != nil || raw == "" {
		return nil
	}
	var g turtleGame
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return nil
	}
	// 过期作废并清理
	if time.Since(time.Unix(g.CreatedAt, 0)) > turtleSoupMaxAge {
		_ = kv.Set(ctx, turtleSoupPluginID, key, "")
		return nil
	}
	return &g
}

// saveGame 保存局状态（g 为 nil 时删除键，结算/揭晓后本局即结束）。
func saveGame(kv *database.PluginKVStore, ctx context.Context, groupID, userID string, g *turtleGame) {
	if kv == nil {
		return
	}
	key := soupKey(groupID, userID)
	if g == nil {
		_ = kv.Delete(ctx, turtleSoupPluginID, key)
		return
	}
	data, _ := json.Marshal(g)
	_ = kv.Set(ctx, turtleSoupPluginID, key, string(data))
}

// ============================================================
// LLM 出题 / 判定
// ============================================================

// turtleGeneration LLM 出题结果
type turtleGeneration struct {
	SoupFace string   `json:"soup_face"`
	SoupBase string   `json:"soup_base"`
	Hints    []string `json:"hints"` // 判定要点列表（模型输出数组）
}

// turtleJudgement LLM 判定结果（提问）
type turtleJudgement struct {
	Answer string `json:"answer"` // "是" / "否" / "无关"
}

// turtleGuessResult LLM 猜答案判定结果
type turtleGuessResult struct {
	Correct bool `json:"correct"`
	// Direction 大方向是否正确（correct=false 时仍可 direction=true）。
	// 仅用于代码拼接固定的"方向正确"肯定语，LLM 不输出任何文字提示，
	// 保证不会泄露汤底的具体信息、线索或暗示。
	Direction bool `json:"direction"`
}

// intPtr 返回 int 的指针（ChatRequest.MaxTokens 为 *int，nil 表示沿用全局配置）。
func intPtr(v int) *int { return &v }

// boolPtr 返回 bool 的指针（ChatRequest.DisableThinking 为 *bool）。
func boolPtr(v bool) *bool { return &v }

// chatJSON 调用 LLM 并解析 JSON 响应（容错：剥离 markdown 代码块围栏）。
// timeout > 0 时为 LLM 调用设置独立超时：LLM 慢/故障时快速降级返回，
// 避免耗尽消息级预算（20s）触发"迷糊"兜底；<=0 则沿用传入 ctx。
func chatJSON(ctx context.Context, client llm.LLMClient, system, user string, out any, timeout time.Duration) error {
	if client == nil {
		return fmt.Errorf("LLM 不可用")
	}
	llmCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		llmCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	resp, err := client.Chat(llmCtx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		},
		MaxTokens:       intPtr(turtleSoupMaxTokens),
		DisableThinking: boolPtr(true),
	})
	if err != nil {
		return fmt.Errorf("LLM 调用失败: %w", err)
	}
	raw := strings.TrimSpace(resp.Content)
	if start := strings.Index(raw, "{"); start != -1 {
		if end := strings.LastIndex(raw, "}"); end > start {
			raw = raw[start : end+1]
		}
	}
	return json.Unmarshal([]byte(raw), out)
}

// generateSoup 让 LLM 生成一局海龟汤。timeout 为 LLM 调用独立超时（<=0 不限制）。
func generateSoup(ctx context.Context, client llm.LLMClient, timeout time.Duration) (*turtleGame, error) {
	const system = `你是一个海龟汤（情境推理谜题）出题人。请生成一个经典风格的海龟汤谜题：
- 汤面（soup_face）：简短、离奇、让人困惑的情境描述（1-3 句话），只陈述现象，不含任何解释
- 汤底（soup_base）：完整合理的真相（1-3 句话），能自洽解释汤面
- 判定要点（hints）：供主持人回答"是/否/无关"时参考的关键事实列表（2-4 条，严禁出现完整答案）
要求：情境不得涉及血腥、暴力、色情、违法或不适内容，适合校园群聊；谜题要有趣且逻辑自洽。
仅输出 JSON，不要其他内容：{"soup_face":"...","soup_base":"...","hints":["...","..."]}`
	resp := &turtleGeneration{}
	if err := chatJSON(ctx, client, system, "请生成一局海龟汤。", resp, timeout); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.SoupFace) == "" || strings.TrimSpace(resp.SoupBase) == "" {
		return nil, fmt.Errorf("LLM 返回的谜题不完整")
	}
	return &turtleGame{
		SoupFace:  strings.TrimSpace(resp.SoupFace),
		SoupBase:  strings.TrimSpace(resp.SoupBase),
		Hints:     resp.Hints,
		CreatedAt: time.Now().Unix(),
	}, nil
}

// judgeQuestion 让 LLM 判定提问的答案是 是/否/无关。timeout 为 LLM 调用独立超时。
func judgeQuestion(ctx context.Context, client llm.LLMClient, g *turtleGame, question string, timeout time.Duration) (string, error) {
	var sb strings.Builder
	sb.WriteString("你正在主持一个海龟汤（情境推理）游戏，只回答 是/否/无关。\n\n")
	sb.WriteString("汤面：" + g.SoupFace + "\n")
	sb.WriteString("汤底：" + g.SoupBase + "\n")
	sb.WriteString("判定要点：" + strings.Join(g.Hints, "；") + "\n")
	if len(g.QAPairs) > 0 {
		sb.WriteString("\n玩家已提问（历史）：\n")
		for i, qa := range g.QAPairs {
			sb.WriteString(fmt.Sprintf("%d. 问：%s → 答：%s\n", i+1, qa.Q, qa.A))
		}
	}
	sb.WriteString("\n玩家最新提问：" + question + "\n\n")
	sb.WriteString(`判定规则：
- "是"：提问符合汤底描述
- "否"：提问与汤底矛盾
- "无关"：提问无法从汤底判断或与游戏无关
严格只回答 是 / 否 / 无关 三者之一，严禁附加任何解释、提示或多余文字。
仅输出 JSON：{"answer":"是"}`)
	resp := &turtleJudgement{}
	if err := chatJSON(ctx, client, "你是海龟汤主持人，回答必须严格基于汤底事实。", sb.String(), resp, timeout); err != nil {
		return "", err
	}
	switch resp.Answer {
	case "是", "否":
	case "无关":
		resp.Answer = "无关"
	default:
		resp.Answer = "无关" // LLM 未按格式输出时兜底
	}
	return resp.Answer, nil
}

// judgeGuess 让 LLM 判定玩家的猜测是否命中汤底。timeout 为 LLM 调用独立超时。
// 返回 (猜对, 大方向正确, 错误)：大方向正确仅在未猜对时用于肯定性提示，
// 提示语由代码固定拼接，LLM 不产生任何文字内容，杜绝泄露汤底细节。
func judgeGuess(ctx context.Context, client llm.LLMClient, g *turtleGame, guess string, timeout time.Duration) (bool, bool, error) {
	system := `你是海龟汤主持人，判断玩家的猜测是否命中汤底。判定标准（从严执行）：
- 猜对（correct=true）：猜测必须完整覆盖汤底的核心因果链与关键事实（谁做了什么、如何导致汤面现象），允许措辞不同，但不得遗漏关键环节
- 大方向正确（direction=true）：猜测的总体思路/切入角度与汤底大方向一致（如围绕同一主题、同一类成因），但关键事实或因果仍有缺失或错误。direction 只表达大方向对不对，是整体判断，不针对汤底任何具体元素
- 大方向错误（direction=false）：猜测的主题、成因类型与汤底南辕北辙，或过于含糊无法判断方向
注意：direction=true 不等于猜对；宁可整体从严，拿不准时 direction=false。
严禁输出任何解释、提示或与汤底内容相关的信息，只输出判定结果。`
	user := fmt.Sprintf("汤底：%s\n\n玩家猜测：%s\n\n仅输出 JSON：{\"correct\":false,\"direction\":false}", g.SoupBase, guess)
	resp := &turtleGuessResult{}
	if err := chatJSON(ctx, client, system, user, resp, timeout); err != nil {
		return false, false, err
	}
	return resp.Correct, resp.Direction, nil
}

// ============================================================
// Pass 实现
// ============================================================

// turtleSoupPass 按命令前缀分发处理海龟汤请求。
//
// LLM 出题/判定采用**异步模式**：命令到达后立即回复提示语并挂起管线（yield），
// 后台 goroutine 用独立长超时调用 LLM（不占消息 20s 预算，多慢都能完成），
// 完成后经段落投递通道（bot.stream.ch）自动发送结果。
// 这是为适配慢速/推理型 LLM（响应 10~30s 波动）的必要设计：同步等待必然超时。
type turtleSoupPass struct {
	llmClient llm.LLMClient
	kv        *database.PluginKVStore
	logger    *zap.Logger
	timeout   time.Duration // 异步出题/判定的最长等待（goroutine 独立 context 上限）

	// mu 保护游戏状态临界区（loadGame/saveGame）与 inFlight 标记。
	// LLM 调用本身在锁外执行（慢网络调用不持锁），由 inFlight 保证同一群同时只有一个异步任务。
	mu       sync.Mutex
	inFlight map[string]bool // soupKey → 是否有进行中的异步出题/判定
}

// streamChannelKey 段落投递通道键（复用 bot 层的流式段落机制，见 internal/bot/passes.go；
// 按 bizplugin 约定不 import bot 包，直接使用字符串字面量）。
const streamChannelKey = "bot.stream.ch"

// streamTimeout 异步出题/判定的独立上下文超时（配置 turtle_soup_timeout_seconds<=0 时的兜底）。
const streamTimeout = 120 * time.Second

// turtleSoupMaxTokens 海龟汤 LLM 调用（出题/判定）的输出 token 上限。
// 推理模型对 max_tokens 敏感（上限越大思考越久、响应越慢），
// 出题/判定只需几百 token 的 JSON，设 1024 可让模型收敛、显著提速。
const turtleSoupMaxTokens = 1024

func (pass *turtleSoupPass) Execute(ctx *conduit.MessageContext) error {
	msg := strings.TrimSpace(ctx.RawMsg)
	switch {
	case isOpenSoupCommand(ctx):
		return pass.open(ctx)
	case isAskSoupCommand(ctx):
		return pass.ask(ctx, strings.TrimSpace(strings.TrimPrefix(msg, "/问")))
	case isGuessSoupCommand(ctx):
		return pass.guess(ctx, strings.TrimSpace(strings.TrimPrefix(msg, "/猜")))
	case isGiveUpSoupCommand(ctx):
		return pass.giveUp(ctx)
	default:
		return nil
	}
}

// reply 输出一条回复。
func (pass *turtleSoupPass) reply(ctx *conduit.MessageContext, content string) {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: content,
	})
}

// beginAsync 启动一个异步 LLM 任务并挂起管线（yield）。
//
// 工作方式（复用 bot 层流式段落投递机制）：
//  1. 登记 inFlight（同一群同时只允许一个异步任务，防并发覆盖游戏状态）；
//  2. 同步回复 hint 提示语；
//  3. 将结果通道写入黑板（streamChannelKey），返回 ErrPassYielded；
//  4. 引擎 yield 后，bot 消费通道把 fn 写入的结果作为段落自动发送。
//
// fn 在后台 goroutine 执行，使用独立长超时 context（不受消息 20s 预算约束），
// 结果（最终回复文案）写入 ch，函数返回前由调用方保证 saveGame 已完成。
func (pass *turtleSoupPass) beginAsync(ctx *conduit.MessageContext, key, hint string, fn func(gctx context.Context, ch chan<- string)) error {
	pass.mu.Lock()
	if pass.inFlight[key] {
		pass.mu.Unlock()
		pass.reply(ctx, "蓝妹正在处理上一个请求，稍等一下~")
		return nil
	}
	pass.inFlight[key] = true
	pass.mu.Unlock()

	pass.reply(ctx, hint)
	ch := make(chan string, 1)
	conduit.Set(ctx, streamChannelKey, ch)
	go func() {
		defer close(ch)
		defer func() {
			pass.mu.Lock()
			delete(pass.inFlight, key)
			pass.mu.Unlock()
		}()
		gctx, cancel := context.WithTimeout(context.Background(), pass.asyncTimeout())
		defer cancel()
		fn(gctx, ch)
	}()
	return conduit.ErrPassYielded
}

// asyncTimeout 返回异步任务的独立超时（配置 turtle_soup_timeout_seconds，
// <=0 时退化为固定 120s）。
func (pass *turtleSoupPass) asyncTimeout() time.Duration {
	if pass.timeout > 0 {
		return pass.timeout
	}
	return streamTimeout
}

// open 开一局海龟汤（一个群同时只能一局）。出题异步进行，不阻塞消息预算。
func (pass *turtleSoupPass) open(ctx *conduit.MessageContext) error {
	if pass.llmClient == nil {
		pass.reply(ctx, "海龟汤需要 LLM 才能出题，当前未配置~")
		return nil
	}
	return pass.beginAsync(ctx, soupKey(ctx.GroupID, ctx.UserID), "正在煮汤，稍等一下…",
		func(gctx context.Context, ch chan<- string) {
			pass.mu.Lock()
			existing := loadGame(pass.kv, gctx, ctx.GroupID, ctx.UserID)
			pass.mu.Unlock()
			if existing != nil {
				ch <- "本群已经有一锅汤在炖啦！先 `/认输` 结束这局，再开新的 (´･ω･`)"
				return
			}
			game, err := generateSoup(gctx, pass.llmClient, pass.asyncTimeout())
			if err != nil {
				pass.logger.Warn("turtle_soup: 出题失败", zap.Error(err))
				ch <- "汤煮糊了，稍后再试一次吧 (￣ω￣;)"
				return
			}
			game.Creator = ctx.UserID
			pass.mu.Lock()
			saveGame(pass.kv, gctx, ctx.GroupID, ctx.UserID, game)
			pass.mu.Unlock()
			ch <- fmt.Sprintf(
				"海龟汤开张啦！\n——\n%s\n——\n\n用 `/问 <问题>` 提问（蓝妹只答 是/否/无关），`/猜 <答案>` 猜汤底，`/认输` 看答案。",
				game.SoupFace)
		})
}

// ask 处理提问（仅在有进行中的局时生效）。判定异步进行。
func (pass *turtleSoupPass) ask(ctx *conduit.MessageContext, question string) error {
	if question == "" {
		pass.reply(ctx, "格式：/问 <问题>，比如 `/问 他是因为停电死的吗`")
		return nil
	}
	if pass.llmClient == nil {
		pass.reply(ctx, "海龟汤需要 LLM 才能判定，当前未配置~")
		return nil
	}
	return pass.beginAsync(ctx, soupKey(ctx.GroupID, ctx.UserID), "蓝妹在琢磨你的问题…",
		func(gctx context.Context, ch chan<- string) {
			pass.mu.Lock()
			game := loadGame(pass.kv, gctx, ctx.GroupID, ctx.UserID)
			pass.mu.Unlock()
			if game == nil {
				ch <- "现在没有进行中的汤，用 `/开汤` 开一局吧"
				return
			}
			if game.QuestionCount >= turtleSoupMaxQuestions {
				ch <- "这锅汤已经问了太多啦，直接 `/猜` 或 `/认输` 吧"
				return
			}
			answer, err := judgeQuestion(gctx, pass.llmClient, game, question, pass.asyncTimeout())
			if err != nil {
				pass.logger.Warn("turtle_soup: 判定失败", zap.Error(err))
				ch <- "蓝妹走神了，再问一次吧 (￣ω￣;)"
				return
			}
			game.QuestionCount++
			game.QAPairs = append(game.QAPairs, turtleQA{Q: question, A: answer})
			pass.mu.Lock()
			saveGame(pass.kv, gctx, ctx.GroupID, ctx.UserID, game)
			pass.mu.Unlock()
			ch <- answer
		})
}

// guess 猜汤底，命中则揭晓结算。判定异步进行。
func (pass *turtleSoupPass) guess(ctx *conduit.MessageContext, guess string) error {
	if guess == "" {
		pass.reply(ctx, "格式：/猜 <答案>，比如 `/猜 他是冻死的`")
		return nil
	}
	if pass.llmClient == nil {
		pass.reply(ctx, "海龟汤需要 LLM 才能判定，当前未配置~")
		return nil
	}
	return pass.beginAsync(ctx, soupKey(ctx.GroupID, ctx.UserID), "蓝妹在琢磨你的答案…",
		func(gctx context.Context, ch chan<- string) {
			pass.mu.Lock()
			game := loadGame(pass.kv, gctx, ctx.GroupID, ctx.UserID)
			pass.mu.Unlock()
			if game == nil {
				ch <- "现在没有进行中的汤，用 `/开汤` 开一局吧"
				return
			}
			correct, direction, err := judgeGuess(gctx, pass.llmClient, game, guess, pass.asyncTimeout())
			if err != nil {
				pass.logger.Warn("turtle_soup: 猜答案判定失败", zap.Error(err))
				ch <- "蓝妹走神了，再猜一次吧 (￣ω￣;)"
				return
			}
			if !correct {
				// 只给大方向的整体肯定，固定文案不含汤底任何细节、线索或暗示。
				if direction {
					ch <- "方向正确，但还没完全抓住真相，继续加油~"
				} else {
					ch <- "猜错了，再想想看~"
				}
				return
			}
			pass.mu.Lock()
			saveGame(pass.kv, gctx, ctx.GroupID, ctx.UserID, nil) // 结算，清除本局
			pass.mu.Unlock()
			ch <- fmt.Sprintf("🎉 猜对啦！汤底是：\n%s", game.SoupBase)
		})
}

// giveUp 放弃并揭晓汤底（无 LLM 调用，同步完成）。
func (pass *turtleSoupPass) giveUp(ctx *conduit.MessageContext) error {
	key := soupKey(ctx.GroupID, ctx.UserID)
	pass.mu.Lock()
	defer pass.mu.Unlock()
	if pass.inFlight[key] {
		pass.reply(ctx, "蓝妹正在处理上一个请求，稍等一下再认输~")
		return nil
	}
	game := loadGame(pass.kv, ctx.Ctx, ctx.GroupID, ctx.UserID)
	if game == nil {
		pass.reply(ctx, "现在没有进行中的汤，用 `/开汤` 开一局吧")
		return nil
	}
	pass.reply(ctx, fmt.Sprintf(
		"认输啦~ 这锅汤的汤底是：\n%s\n\n开局的 %s 端走汤底，想再来一局就 `/开汤`",
		game.SoupBase, game.Creator))
	saveGame(pass.kv, ctx.Ctx, ctx.GroupID, ctx.UserID, nil)
	return nil
}
