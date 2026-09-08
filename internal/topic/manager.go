package topic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zrurf/conduit"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/ai/intent"
	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
	"github.com/DaWesen/lanmei-dream/internal/config"
)

// ── 常量 ──

// maxRecordRunes 单条消息写入话题窗口的内容长度上限（rune），防止超长消息拖垮上下文与归档。
const maxRecordRunes = 500

// maxArchiveAttempts 话题归档最大重试次数：超过后放弃归档并丢弃话题（防无限重试耗损 LLM 成本）。
const maxArchiveAttempts = 2

// maxLabelRunes 话题标签最大长度（rune）。
const maxLabelRunes = 24

// judgeContextMsgs 供提及判断（指代消解）注入的最近对话条数。
const judgeContextMsgs = 6

// topicIndexKey 持久化索引键：记录所有存在活跃/冷却话题的群，供启动恢复。
const topicIndexKey = "topic:index"

// Manager 群聊话题（Topic）状态管理器。
//
// 职责：
//   - HandleGroupMessage：对每条群消息做"是否应回复"的决策
//     （at 精确命中恒为强提及；其余由 LLM 提及判断 LinguisticJudge 划分强/弱）；
//   - 维护每个群的话题状态机（Active → Cooling → Archived）与成员、消息窗口、语义中心；
//   - 持久化到 conduit.StateStore（Redis）：话题状态 JSON + 群索引，支持重启恢复；
//   - 后台扫描：冷却超时话题异步归档到记忆层。
//
// 并发模型：单 Manager 实例；HandleGroupMessage / RecordBotReply / 后台协程之间
// 通过内部读写锁互斥。网络调用（embedding）在加锁前完成，避免长阻塞。
type Manager struct {
	mu        sync.RWMutex
	groups    map[string][]*Topic // groupKey(platform:groupID) → 话题列表
	seqs      map[string]int64    // groupKey → 群消息序列号（活跃窗口判定）
	indexed   map[string]bool     // groupKey → 是否已登记到持久化索引（避免重复读 Redis）
	store     conduit.StateStore  // 状态存储（Redis），nil 时仅内存运行
	emb       embedding.Embedder  // 语义判定（可 nil，nil 时降级为成员制）
	llm       llm.LLMClient       // 话题标签懒生成（可 nil）
	archive   *Archiver           // 冷却归档器（可 nil，nil 时超时直接丢弃）
	cfg       *config.TopicConfig
	nicknames []string // Bot 名字与别名（注入提及判断 LLM 的上下文，如 ["蓝妹","蓝莓"]）
	logger    *zap.Logger
	started   atomic.Bool    // Start 只执行一次
	wg        sync.WaitGroup // 后台协程（标签生成/归档扫描）
}

// NewManager 创建话题管理器。
// nicknames 为 Bot 名字与别名（主名 + 外号，如 ["蓝妹","蓝莓"]），为空时使用默认。
func NewManager(cfg *config.TopicConfig, store conduit.StateStore, emb embedding.Embedder,
	llmClient llm.LLMClient, arch *Archiver, nicknames []string, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	if len(nicknames) == 0 {
		nicknames = []string{"蓝妹", "蓝莓"}
	}
	if cfg == nil {
		cfg = &config.TopicConfig{}
	}
	return &Manager{
		groups:    make(map[string][]*Topic),
		seqs:      make(map[string]int64),
		indexed:   make(map[string]bool),
		store:     store,
		emb:       emb,
		llm:       llmClient,
		archive:   arch,
		cfg:       cfg,
		nicknames: nicknames,
		logger:    logger,
	}
}

// ── 群消息决策 ──

// HandleGroupMessage 对一条群消息做决策：是否应回复、命中/创建的话题、提及模式。
//
// judge 为意图分析 LLM 调用返回的提及判定（nil 表示未提供/LLM 不可用）：
//  1. at（平台 ID 精确命中）→ 恒强提及，创建/重入话题并回复（不依赖 judge）；
//  2. judge.IsTalkingToBot 且置信度达强阈值 → 强提及，同上；
//  3. judge.IsTalkingToBot 且置信度达弱阈值 → 弱提及：非成员拉入话题（静默，授配额），
//     成员按回复配额续聊（配额在 Bot 实际回复成功时消耗/授予）；
//  4. 未提及但为成员 → 语义相关性判定：不相关则脱离话题（话题切换），相关则仅入窗；
//  5. 冷却检查：窗口内无触碰的话题转冷却。
//
// 注意：向量化为网络调用，在加锁前完成（只依赖消息本身）。
func (m *Manager) HandleGroupMessage(ctx context.Context, msg *IncomingMsg, judge *LinguisticJudge) *Decision {
	if m == nil || msg == nil || msg.GroupID == "" {
		return &Decision{}
	}
	now := msg.SentAt
	if now.IsZero() {
		now = time.Now()
	}
	gk := m.groupKey(msg.Platform, msg.GroupID)

	// 提及分类：at 恒强；其余按 LLM 提及判断（LinguisticJudge）划分强/弱（无网络）
	mention := m.classifyMention(msg, judge)
	// 语义向量（每条消息最多一次 embedding，所有判定路径复用）
	vec, vecOK := m.embedMessage(ctx, msg)

	m.mu.Lock()
	defer m.mu.Unlock()

	seq := m.bumpSeq(gk)
	topics := m.groups[gk]

	// ── 1. 强提及：创建或重入话题，回复 ──
	if mention.Strong {
		t := semanticMatch(m, topics, msg, vec, vecOK)
		if t == nil {
			t = m.createTopicLocked(gk, msg, seq, now)
			if vecOK {
				t.updateVector(vec) // 新话题以当前消息为语义种子
			}
			topics = append(topics, t)
		} else {
			m.reenterLocked(t, msg, seq, now, vec, vecOK)
		}
		sortTopics(topics)
		m.groups[gk] = topics
		m.persistLocked(ctx, gk, topics)
		m.logger.Info("topic: 强提及 → 回复",
			zap.String("group", gk), zap.String("user", msg.UserID),
			zap.String("mode", mention.Mode.String()), zap.String("topic", t.ID))
		return &Decision{Reply: true, Topic: t, Mention: mention.Mode}
	}

	// ── 2. 弱提及：非成员拉入话题（静默、授配额）；成员按配额续聊回复 ──
	if mention.Mentioned && !mention.Strong {
		t := memberTopicOf(topics, msg.UserID)
		if t == nil {
			// 非成员：拉入最近活跃话题（或创建新话题），授回复配额
			t = semanticMatch(m, activeOnly(topics), msg, vec, vecOK)
			if t == nil {
				t = m.createTopicLocked(gk, msg, seq, now)
				if vecOK {
					t.updateVector(vec) // 新话题以当前消息为语义种子
				}
				topics = append(topics, t)
			} else {
				m.joinPassiveLocked(t, msg, seq, now, vec, vecOK)
			}
			t.grantCredit(msg.UserID) // 下次相关消息自动回复
			sortTopics(topics)
			m.groups[gk] = topics
			m.persistLocked(ctx, gk, topics)
			m.logger.Info("topic: 弱提及 → 拉入话题（静默）",
				zap.String("group", gk), zap.String("user", msg.UserID), zap.String("mode", mention.Mode.String()), zap.String("topic", t.ID))
			return &Decision{Reply: false, Topic: t, Mention: mention.Mode}
		}
		// 已是成员：续聊回复（纯媒体消息无文本不参与，配额由实际回复时消耗/授予）
		if msg.Content != "" {
			m.continueChatLocked(t, msg, seq, now, vec, vecOK)
			if m.hasCredit(t, msg.UserID) {
				m.persistLocked(ctx, gk, topics)
				m.logger.Info("topic: 成员续聊（配额）→ 回复",
					zap.String("group", gk), zap.String("user", msg.UserID), zap.String("topic", t.ID))
				return &Decision{Reply: true, Topic: t, Mention: mention.Mode}
			}
			m.persistLocked(ctx, gk, topics)
			m.logger.Debug("topic: 成员续聊（无配额）→ 静默",
				zap.String("group", gk), zap.String("user", msg.UserID), zap.String("topic", t.ID))
			return &Decision{Reply: false, Topic: t, Mention: mention.Mode}
		}
	}

	// ── 3. 未提及但为成员：话题切换检测 / 延续对话 ──
	t := memberTopicOf(topics, msg.UserID)
	if t != nil && msg.Content != "" {
		if semanticRelevant(m, t, msg, vec, vecOK) {
			m.continueChatLocked(t, msg, seq, now, vec, vecOK)
			// 延续对话：Bot 刚回复过该成员（回复配额有效）→ 视为继续对话并回复，
			// 而非仅入窗。解决"用户 @bot 问完第一个问题后连续追问"的场景——
			// 后续消息即使未被 LLM 判定为提及（承接语如"那具体怎么操作呢"），
			// 只要话题相关且配额有效就继续解答，避免对话断裂。
			if m.hasCredit(t, msg.UserID) {
				m.persistLocked(ctx, gk, topics)
				m.logger.Info("topic: 成员延续对话（配额）→ 回复",
					zap.String("group", gk), zap.String("user", msg.UserID), zap.String("topic", t.ID))
				return &Decision{Reply: true, Topic: t, Mention: mention.Mode}
			}
		} else {
			// 用户切换了话题：脱离原话题，成员清空则冷却
			t.detachMember(msg.UserID)
			if t.MemberCount() == 0 {
				t.markCooling()
			}
			m.logger.Debug("topic: 成员话题切换 → 脱离",
				zap.String("group", gk), zap.String("user", msg.UserID), zap.String("topic", t.ID))
		}
	}

	// ── 4. 冷却检查：窗口内无触碰的话题转冷却 ──
	changed := m.coolExpired(topics, seq)
	if changed || t != nil {
		m.persistLocked(ctx, gk, topics)
	}

	return &Decision{Reply: false, Mention: mention.Mode}
}

// classifyMention 将 at 与 LLM 提及判定合并为强/弱/无三档提及。
//
// at（平台 ID 精确命中）恒为强提及；其余按 judge.IsTalkingToBot 与
// 证据分档 + 置信度阈值划分：
//   - 强证据角色（直接称呼/让做事/情感对象）**且提供了可核实证据（Evidence 非空）**：
//     结构上已明确"在跟机器人说话"，置信度达弱阈值即可按强提及处理；
//   - 其余（弱证据角色，或强证据角色但未提供证据）：严格按置信度两档
//     （强 0.7 / 弱 0.4），证据缺失不享有门槛放宽。
//
// Evidence 缺失即按更严格门槛（对齐"证据必须可核实"：无证据不强提）。
func (m *Manager) classifyMention(msg *IncomingMsg, judge *LinguisticJudge) MentionResult {
	if msg != nil && containsString(msg.AtTargets, msg.SelfID) {
		return MentionResult{Mentioned: true, Mode: MentionAt, Strong: true}
	}
	if judge != nil && judge.IsTalkingToBot {
		if strongEvidenceRole[judge.Role] && judge.Evidence != "" {
			// 强证据角色 + 有证据：达弱阈值即强提及；低于弱阈值但判为提及时按弱提及拉入
			if judge.Confidence >= m.linguisticWeakThreshold() {
				return MentionResult{Mentioned: true, Mode: MentionLinguistic, Strong: true}
			}
			if judge.Confidence > 0 {
				return MentionResult{Mentioned: true, Mode: MentionLinguistic, Strong: false}
			}
			return MentionResult{}
		}
		// 弱证据角色，或强证据角色但未提供证据：原置信度两档（强 0.7 / 弱 0.4）
		switch {
		case judge.Confidence >= m.linguisticStrongThreshold():
			return MentionResult{Mentioned: true, Mode: MentionLinguistic, Strong: true}
		case judge.Confidence >= m.linguisticWeakThreshold():
			return MentionResult{Mentioned: true, Mode: MentionLinguistic, Strong: false}
		}
	}
	return MentionResult{}
}

// BuildJudgeContext 构建供意图分析 LLM 做提及判断的群聊上下文
// （含最近对话，供 LLM 做指代消解：如"那你呢"中的"你"指机器人）。
//
// 只读操作：取当前用户所在话题（或最近活跃话题）的最近若干条消息，不含当前消息。
// 调用时机在 HandleGroupMessage 之前；内部加读锁，与并发消息处理互斥。
func (m *Manager) BuildJudgeContext(msg *IncomingMsg) *intent.JudgeContext {
	if m == nil || msg == nil || msg.GroupID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	jc := &intent.JudgeContext{BotNames: append([]string(nil), m.nicknames...)}
	gk := m.groupKey(msg.Platform, msg.GroupID)
	topics := m.groups[gk]

	var t *Topic
	for _, x := range topics {
		if x.isMember(msg.UserID) {
			t = x
			break
		}
	}
	if t == nil && len(topics) > 0 {
		t = topics[0] // 最近活跃话题（列表按 LastActiveAt 降序）
	}
	if t == nil {
		return jc
	}

	start := max(len(t.MsgWindow)-judgeContextMsgs, 0)
	for _, tm := range t.MsgWindow[start:] {
		if tm.Content == "" {
			continue
		}
		// 用户消息以「昵称(用户ID)」标注发言者：用户ID 是稳定身份锚点，
		// 群昵称常变，若只标注昵称，意图分析会认错人；Bot 消息统一用 "bot"
		// （与意图分析 prompt 中"bot 发言即机器人的话"约定一致）。
		speaker := "user"
		if tm.IsBot {
			speaker = "bot"
		} else {
			speaker = SpeakerLabel(tm.Nickname, tm.UserID)
		}
		jc.Recent = append(jc.Recent, intent.JudgeMessage{
			Speaker: speaker,
			Content: truncateRunes(tm.Content, maxRecordRunes),
		})
	}
	return jc
}

// RecordBotReply 记录一次 Bot 回复到话题：追加消息窗口、刷新活跃时间、
// 并给被回复用户授回复配额（下一次相关消息自动回复）。
// 由 RoleplayStreamPass 在流式回复完成后调用。
func (m *Manager) RecordBotReply(ctx context.Context, platform, groupID, topicID, selfID, userID, content string) {
	if m == nil || topicID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	gk := m.groupKey(platform, groupID)
	topics := m.groups[gk]
	for _, t := range topics {
		if t.ID != topicID {
			continue
		}
		t.pushMsg(TopicMsg{UserID: selfID, IsBot: true, Content: truncateRunes(content, maxRecordRunes), SentAt: now})
		t.LastActiveAt = now
		t.LastTouchSeq = m.bumpSeq(gk)
		// 真实回复成功后才消耗/授配额：消耗本次续聊额度并授新额度，
		// 若"决策回复但未实际回复"（意图忽略/失败）则不经过此路径，配额得以保留。
		t.consumeCredit(userID)
		t.grantCredit(userID)
		m.persistLocked(ctx, gk, topics)
		m.logger.Debug("topic: Bot 回复已记录", zap.String("group", gk), zap.String("topic", t.ID))
		return
	}
}

// BuildTopicContext 构建供对话管线注入的话题上下文。
// excludeTail 为排除窗口末尾的消息条数（通常为 1：当前消息已入窗，避免与用户消息重复）。
// 内部加读锁：该方法通常在 HandleGroupMessage 返回后（锁已释放）由管线调用，
// 需与并发消息处理（pushMsg/upsertMember）互斥。
func (m *Manager) BuildTopicContext(t *Topic, excludeTail int) *llm.TopicContext {
	if t == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	tc := &llm.TopicContext{TopicID: t.ID, Label: t.DisplayLabel()}
	// 成员以「昵称(用户ID)」标注，ID 锚定身份，昵称变化不影响成员识别
	memberNames := make([]string, 0, len(t.Members))
	for _, mem := range t.Members {
		memberNames = append(memberNames, SpeakerLabel(mem.Nickname, mem.UserID))
	}
	sort.Strings(memberNames)
	tc.Members = memberNames
	end := min(max(len(t.MsgWindow)-excludeTail, 0), len(t.MsgWindow))
	for _, tm := range t.MsgWindow[:end] {
		tc.Recent = append(tc.Recent, llm.TopicMsg{
			UserID:   tm.UserID,
			Nickname: tm.Nickname,
			IsBot:    tm.IsBot,
			Content:  tm.Content,
			At:       tm.At,
			SentAt:   tm.SentAt,
		})
	}
	return tc
}

// ── 生命周期 ──

// Start 启动后台协程：恢复持久化话题 + 周期性冷却归档扫描。
// ctx 取消时优雅退出（归档扫描停止；进行中的归档不中断）。
func (m *Manager) Start(ctx context.Context) {
	if m.started.Swap(true) {
		return
	}
	m.restore(ctx)

	interval := m.cfg.ArchiveIntervalSeconds
	if interval <= 0 {
		interval = 60
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.scanAndArchive(ctx)
			}
		}
	}()
	m.logger.Info("topic: 管理器已启动",
		zap.Int("window", m.windowMsgs()), zap.Int("cooling_timeout_min", m.cfg.CoolingTimeoutMinutes))
}

// archiveJob 归档任务：话题引用 + 锁内冻结的归档快照。
// 归档在锁外异步执行，快照保证归档期间话题被重入修改也不会产生数据竞争。
type archiveJob struct {
	t    *Topic
	snap *ArchiveSnapshot
}

// scanAndArchive 周期性扫描：冷却超时的话题触发异步归档。
func (m *Manager) scanAndArchive(ctx context.Context) {
	timeout := time.Duration(m.cfg.CoolingTimeoutMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	var archived []*archiveJob
	m.mu.Lock()
	for gk, topics := range m.groups {
		keep := topics[:0]
		changed := false
		for _, t := range topics {
			if t.State == TopicCooling && time.Since(t.LastActiveAt) > timeout {
				if m.archive == nil {
					// 无归档器：直接丢弃（数据已保存在会话历史表）
					m.logger.Warn("topic: 无归档器，冷却话题直接丢弃", zap.String("topic", t.ID))
					changed = true
					continue
				}
				t.ArchiveAttempts++
				if t.ArchiveAttempts > maxArchiveAttempts {
					m.logger.Warn("topic: 归档重试耗尽，丢弃话题", zap.String("topic", t.ID), zap.Int("attempts", t.ArchiveAttempts))
					changed = true
					continue
				}
				// 锁内冻结归档快照（窗口/成员/标签），供锁外异步归档使用
				snap := &ArchiveSnapshot{
					ID:       t.ID,
					Platform: t.Platform,
					GroupID:  t.GroupID,
					Label:    t.Label,
					Window:   append([]TopicMsg(nil), t.MsgWindow...),
					Members:  make([]string, 0, len(t.Members)),
				}
				for uid, mem := range t.Members {
					n := mem.Nickname
					if n == "" {
						n = uid
					}
					snap.Members = append(snap.Members, n)
				}
				archived = append(archived, &archiveJob{t: t, snap: snap})
				keep = append(keep, t) // 保留至异步归档成功
				continue
			}
			keep = append(keep, t)
		}
		m.groups[gk] = keep
		if len(keep) == 0 {
			delete(m.groups, gk)
			m.deleteGroupLocked(ctx, gk)
		} else if changed {
			m.persistLocked(ctx, gk, keep)
		}
	}
	m.mu.Unlock()

	for _, job := range archived {
		m.wg.Add(1)
		go m.archiveTopic(job)
	}
}

// archiveTopic 异步归档单个话题；成功后从内存与持久化移除。
// job.snap 为锁内冻结的归档快照，归档期间话题被重入也不影响归档数据。
func (m *Manager) archiveTopic(job *archiveJob) {
	defer m.wg.Done()
	bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := m.archive.Archive(bgCtx, job.snap); err != nil {
		m.logger.Error("topic: 归档失败（将重试）", zap.String("topic", job.t.ID), zap.Error(err))
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// 归档期间话题被强提及恢复（Active）时，放弃移除（归档结果保留为快照）
	if job.t.State != TopicCooling {
		m.logger.Debug("topic: 归档完成但话题已恢复活跃，保留状态", zap.String("topic", job.t.ID))
		return
	}
	gk := m.groupKey(job.snap.Platform, job.snap.GroupID)
	topics := m.groups[gk]
	keep := make([]*Topic, 0, len(topics))
	for _, x := range topics {
		if x.ID != job.t.ID {
			keep = append(keep, x)
		}
	}
	m.groups[gk] = keep
	if len(keep) == 0 {
		delete(m.groups, gk)
		m.deleteGroupLocked(context.Background(), gk)
	} else {
		m.persistLocked(context.Background(), gk, keep)
	}
}

// ── 状态机操作（需在持锁状态下调用）──

// createTopicLocked 创建新话题（强提及/被动提及路径）。
func (m *Manager) createTopicLocked(gk string, msg *IncomingMsg, seq int64, now time.Time) *Topic {
	t := &Topic{
		ID:           fmt.Sprintf("topic:%s:%s:%d", msg.Platform, msg.GroupID, m.nextSeq(gk)),
		Platform:     msg.Platform,
		GroupID:      msg.GroupID,
		State:        TopicActive,
		Members:      make(map[string]*Member),
		LastTouchSeq: seq,
		CreatedAt:    now,
	}
	t.LastMentionAt = now
	t.LastActiveAt = now
	t.upsertMember(msg.UserID, msg.UserName, now, true)
	t.pushMsg(m.msgToTopicMsg(msg))
	m.lazyLabel(gk, t)
	return t
}

// reenterLocked 强提及重入已有话题（含冷却恢复）。
func (m *Manager) reenterLocked(t *Topic, msg *IncomingMsg, seq int64, now time.Time, vec []float32, vecOK bool) {
	t.restoreActive(now)
	t.LastTouchSeq = seq
	t.upsertMember(msg.UserID, msg.UserName, now, true)
	t.touchMention(m.msgToTopicMsg(msg), now)
	if vecOK {
		t.updateVector(vec)
	}
	m.lazyLabel(m.groupKey(msg.Platform, msg.GroupID), t)
}

// joinPassiveLocked 被动提及用户加入已有活跃话题（不刷新提及时间）。
func (m *Manager) joinPassiveLocked(t *Topic, msg *IncomingMsg, seq int64, now time.Time, vec []float32, vecOK bool) {
	t.LastTouchSeq = seq
	t.upsertMember(msg.UserID, msg.UserName, now, false)
	t.touchChat(m.msgToTopicMsg(msg), now)
	if vecOK {
		t.updateVector(vec)
	}
}

// continueChatLocked 成员在话题内续聊（不刷新提及时间）。
func (m *Manager) continueChatLocked(t *Topic, msg *IncomingMsg, seq int64, now time.Time, vec []float32, vecOK bool) {
	t.LastTouchSeq = seq
	t.upsertMember(msg.UserID, msg.UserName, now, false)
	t.touchChat(m.msgToTopicMsg(msg), now)
	if vecOK {
		t.updateVector(vec)
	}
}

// hasCredit 判断成员续聊是否应回复（回复配额检查，只读不消耗）。
// 配额的实际消耗延迟到 Bot 真实回复成功时（RecordBotReply），
// 避免"决策回复但未实际回复（意图忽略/调用失败等）"时配额被误扣，
// 导致用户后续消息被静默丢弃（表现为服务运行一段时间后不再响应）。
func (m *Manager) hasCredit(t *Topic, userID string) bool {
	if m.cfg == nil || !m.cfg.CreditEnabled {
		return false
	}
	return t.hasCredit(userID)
}

// coolExpired 将窗口内无触碰的话题转冷却。返回是否有状态变化。
func (m *Manager) coolExpired(topics []*Topic, seq int64) bool {
	window := int64(m.windowMsgs())
	changed := false
	for _, t := range topics {
		if t.IsActive() && seq-t.LastTouchSeq > window {
			t.markCooling()
			changed = true
		}
	}
	return changed
}

// msgToTopicMsg 将入站消息转换为窗口消息记录。
func (m *Manager) msgToTopicMsg(msg *IncomingMsg) TopicMsg {
	sentAt := msg.SentAt
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	return TopicMsg{
		UserID:   msg.UserID,
		Nickname: msg.UserName,
		Content:  truncateRunes(msg.Content, maxRecordRunes),
		At:       containsString(msg.AtTargets, msg.SelfID),
		SentAt:   sentAt,
	}
}

// ── 话题标签懒生成 ──

// lazyLabel 异步生成话题标签（LLM 一次调用；无 LLM/已生成/生成中则跳过）。
// 在锁内快照消息窗口后交 goroutine 使用，避免与主协程 pushMsg 的数据竞争。
func (m *Manager) lazyLabel(gk string, t *Topic) {
	if m.llm == nil || t.Label != "" || t.LabelPending {
		return
	}
	t.LabelPending = true
	// 锁内快照（lazyLabel 由 createTopicLocked/reenterLocked 在持锁状态下调用）
	window := make([]TopicMsg, len(t.MsgWindow))
	copy(window, t.MsgWindow)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		bgCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		label := m.genLabel(bgCtx, window)
		m.mu.Lock()
		t.Label = label
		t.LabelPending = false
		m.persistLocked(context.Background(), gk, m.groups[gk])
		m.mu.Unlock()
	}()
}

// genLabel 通过 LLM 为话题生成一句话标签；失败时返回默认值。
// window 为调用方（lazyLabel）快照的消息窗口，本函数只读不修改。
func (m *Manager) genLabel(ctx context.Context, window []TopicMsg) string {
	resp, err := m.llm.Chat(ctx, &llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: labelSystemPrompt},
			{Role: llm.RoleUser, Content: formatWindow(window)},
		},
	})
	if err != nil || resp == nil || resp.Content == "" {
		return defaultTopicLabel
	}
	label := strings.TrimSpace(strings.Trim(resp.Content, `"'"。`))
	if label == "" {
		return defaultTopicLabel
	}
	return truncateRunes(label, maxLabelRunes)
}

// labelSystemPrompt 话题命名 prompt。
const labelSystemPrompt = `你是群聊话题命名助手。根据以下群聊对话片段，用不超过 12 个字概括这个话题的核心内容。
只输出话题名本身，不要引号、不要解释、不要标点。例如：周末爬山计划`

// ── 持久化 ──

// persistLocked 将某群的话题列表序列化写入 Redis（TTL = 冷却超时 × 2），并登记群索引。
// 空列表时删除键与索引（需在持锁状态下调用）。
func (m *Manager) persistLocked(ctx context.Context, gk string, topics []*Topic) {
	if m.store == nil {
		return
	}
	ttl := m.persistTTL()
	if len(topics) == 0 {
		m.deleteGroupLocked(ctx, gk)
		return
	}
	data, err := json.Marshal(topics)
	if err != nil {
		m.logger.Warn("topic: 序列化失败", zap.String("group", gk), zap.Error(err))
		return
	}
	if err := m.store.Set(ctx, m.redisKey(gk), string(data), ttl); err != nil {
		m.logger.Warn("topic: 持久化失败", zap.String("group", gk), zap.Error(err))
		return
	}
	m.indexAddLocked(ctx, gk, ttl)
}

// deleteGroupLocked 删除某群的持久化状态与索引（需在持锁状态下调用）。
func (m *Manager) deleteGroupLocked(ctx context.Context, gk string) {
	if m.store == nil {
		return
	}
	_ = m.store.Delete(ctx, m.redisKey(gk))
	m.indexRemoveLocked(ctx, gk)
	delete(m.seqs, gk)
	delete(m.indexed, gk)
}

// indexAddLocked 将群登记到索引（幂等，进程内缓存避免重复读）。
func (m *Manager) indexAddLocked(ctx context.Context, gk string, ttl time.Duration) {
	if m.indexed[gk] {
		return
	}
	keys, err := m.readIndexLocked(ctx)
	if err != nil {
		m.logger.Warn("topic: 索引读取失败", zap.Error(err))
		return
	}
	for _, k := range keys {
		if k == gk {
			m.indexed[gk] = true
			return
		}
	}
	keys = append(keys, gk)
	if err := m.store.Set(ctx, topicIndexKey, mustJSON(keys), ttl); err != nil {
		m.logger.Warn("topic: 索引写入失败", zap.Error(err))
		return
	}
	m.indexed[gk] = true
}

// indexRemoveLocked 将群从索引移除（列表清空时删除索引键）。
func (m *Manager) indexRemoveLocked(ctx context.Context, gk string) {
	keys, err := m.readIndexLocked(ctx)
	if err != nil {
		delete(m.indexed, gk)
		return
	}
	out := keys[:0]
	for _, k := range keys {
		if k != gk {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		_ = m.store.Delete(ctx, topicIndexKey)
	} else {
		_ = m.store.Set(ctx, topicIndexKey, mustJSON(out), m.persistTTL())
	}
	delete(m.indexed, gk)
}

// readIndexLocked 读取群索引列表（已删除或空时返回空列表）。
func (m *Manager) readIndexLocked(ctx context.Context) ([]string, error) {
	raw, err := m.store.Get(ctx, topicIndexKey)
	if err != nil || raw == "" {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// restore 启动时从 Redis 恢复各群话题状态。
func (m *Manager) restore(ctx context.Context) {
	if m.store == nil {
		return
	}
	keys, err := m.readIndexLocked(ctx)
	if err != nil || len(keys) == 0 {
		return
	}
	for _, gk := range keys {
		data, err := m.store.Get(ctx, m.redisKey(gk))
		if err != nil || data == "" {
			continue
		}
		var topics []*Topic
		if err := json.Unmarshal([]byte(data), &topics); err != nil {
			m.logger.Warn("topic: 恢复反序列化失败", zap.String("group", gk), zap.Error(err))
			continue
		}
		for _, t := range topics {
			if t.Members == nil {
				t.Members = make(map[string]*Member)
			}
			t.LastTouchSeq = 0 // 进程内消息序列已重置，冷却窗口重新累计
			if len(t.MsgWindow) == 0 || t.MemberCount() == 0 {
				t.markCooling() // 无窗口/无成员的话题恢复后置冷却，等待归档
			}
		}
		m.groups[gk] = topics
		m.indexed[gk] = true
		m.logger.Info("topic: 已恢复话题状态", zap.String("group", gk), zap.Int("topics", len(topics)))
	}
}

// ── 内部工具 ──

// groupKey 生成内存/索引用的群键（平台隔离，避免跨平台 groupID 冲突）。
func (m *Manager) groupKey(platform, groupID string) string {
	if platform == "" {
		return groupID
	}
	return platform + ":" + groupID
}

// redisKey 生成 Redis 存储键。
func (m *Manager) redisKey(gk string) string {
	return conduit.MakeStoreKey("topic", "g", gk)
}

// persistTTL 话题状态在 Redis 的存活时间（冷却超时 × 2，兜底 1 小时）。
func (m *Manager) persistTTL() time.Duration {
	ttl := time.Duration(m.cfg.CoolingTimeoutMinutes) * 2 * time.Minute
	if ttl <= 0 {
		ttl = time.Hour
	}
	return ttl
}

// windowMsgs 活跃窗口大小（topic_window_msgs，兜底 20）。
func (m *Manager) windowMsgs() int {
	if m.cfg != nil && m.cfg.TopicWindowMsgs > 0 {
		return m.cfg.TopicWindowMsgs
	}
	return topicWindowDefault
}

// bumpSeq 群消息序列号自增（每条群消息 +1，用于活跃窗口判定）。
func (m *Manager) bumpSeq(gk string) int64 {
	m.seqs[gk]++
	return m.seqs[gk]
}

// nextSeq 生成话题 ID 序列号（取群内现有话题最大序号 + 1）。
func (m *Manager) nextSeq(gk string) int64 {
	var max int64
	for _, t := range m.groups[gk] {
		idx := strings.LastIndexByte(t.ID, ':')
		if idx < 0 {
			continue
		}
		if n, err := strconv.ParseInt(t.ID[idx+1:], 10, 64); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// sortTopics 按最近活跃时间降序排序话题（确定性遍历顺序）。
func sortTopics(topics []*Topic) {
	sort.Slice(topics, func(i, j int) bool {
		return topics[i].LastActiveAt.After(topics[j].LastActiveAt)
	})
}

// mustJSON 序列化为 JSON 字符串（失败时返回空 JSON 数组，避免破坏持久化键）。
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(data)
}
