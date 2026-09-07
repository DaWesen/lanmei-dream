package topic

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zrurf/conduit"
	"github.com/zrurf/conduit/store/memory"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/embedding"
	"github.com/DaWesen/lanmei-dream/internal/config"
)

const testGroup = "g1"

// testManager 构造话题管理器测试实例。
// store 为 nil 时纯内存运行；emb 为语义判定替身（nil 时降级成员制）。
func testManager(cfg *config.TopicConfig, store *memory.Store, emb *mockEmbedder) *Manager {
	if cfg == nil {
		cfg = &config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1}
	}
	// nil *mockEmbedder / *memory.Store 装箱进接口后不为 nil，需显式留空接口
	var e embedding.Embedder
	if emb != nil {
		e = emb
	}
	var st conduit.StateStore
	if store != nil {
		st = store
	}
	return NewManager(cfg, st, e, nil, nil, []string{"蓝妹", "蓝莓"}, zap.NewNop())
}

// groupMsg 构造一条群聊入站消息（默认 at 为空）。
func groupMsg(groupID, userID, content string, at ...string) *IncomingMsg {
	return &IncomingMsg{
		Platform:  "qq",
		SelfID:    "bot_self",
		GroupID:   groupID,
		UserID:    userID,
		UserName:  "成员" + userID,
		Content:   content,
		AtTargets: at,
		SentAt:    time.Now(),
	}
}

// judgeStrong / judgeWeak 构造 LLM 提及判定（强/弱）。
func judgeStrong(conf float64) *LinguisticJudge {
	return &LinguisticJudge{IsTalkingToBot: true, Role: RoleVocative, Confidence: conf}
}

func judgeWeak(conf float64) *LinguisticJudge {
	// 弱证据角色（转述/指代）模拟弱提及：依赖上下文或语义推断，须高置信度才强提及
	return &LinguisticJudge{IsTalkingToBot: true, Role: RoleRelay, Confidence: conf}
}

func groupTopics(m *Manager) []*Topic { return m.groups[m.groupKey("qq", testGroup)] }

// TestManagerStrongMentionCreatesTopic 强提及（at+请求）→ 创建话题并回复。
func TestManagerStrongMentionCreatesTopic(t *testing.T) {
	m := testManager(nil, nil, nil)
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 帮我查一下天气", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}
	if d.Topic == nil || !d.Topic.IsActive() {
		t.Fatalf("topic state = %v, want active", d.Topic)
	}
	if d.Topic.MemberCount() != 1 || !d.Topic.isMember("u1") {
		t.Fatalf("members = %v, want [u1]", d.Topic.Members)
	}
	if topics := groupTopics(m); len(topics) != 1 {
		t.Fatalf("group topics = %d, want 1", len(topics))
	}
	if d.Topic.DisplayLabel() != defaultTopicLabel {
		t.Fatalf("label = %q, want default %q", d.Topic.DisplayLabel(), defaultTopicLabel)
	}
}

// TestManagerJudgeStrongMentionCreatesTopic LLM 提及判定（高置信度）→ 强提及 → 创建话题并回复。
// 验证"在吗蓝妹"这类硬编码规则无法识别的自然语言提及可被 LLM 判为强提及。
func TestManagerJudgeStrongMentionCreatesTopic(t *testing.T) {
	m := testManager(nil, nil, nil)
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "在吗蓝妹"), judgeStrong(0.93))
	if !d.Reply {
		t.Fatal("linguistic strong mention should reply")
	}
	if d.Mention != MentionLinguistic {
		t.Fatalf("mention mode = %v, want linguistic", d.Mention)
	}
	if d.Topic == nil || !d.Topic.IsActive() {
		t.Fatalf("topic state = %v, want active", d.Topic)
	}
}

// TestManagerReenterExistingTopic 再次强提及 → 重入同一话题，不新建。
func TestManagerReenterExistingTopic(t *testing.T) {
	m := testManager(nil, nil, nil)
	d1 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 帮我查天气", "bot_self"), nil)
	d2 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u2", "蓝妹，也帮我看看"), judgeStrong(0.95))
	if !d2.Reply || d2.Topic == nil || d2.Topic.ID != d1.Topic.ID {
		t.Fatalf("reenter should hit same topic: got %v, first %v", d2.Topic, d1.Topic)
	}
	if topics := groupTopics(m); len(topics) != 1 {
		t.Fatalf("topics = %d, want 1 (reuse existing)", len(topics))
	}
	if !d2.Topic.isMember("u2") {
		t.Fatal("u2 should become member after reenter")
	}
}

// TestManagerTopicSwitchDetachesMember 成员话题切换 → 脱离原话题，成员清空立即冷却。
func TestManagerTopicSwitchDetachesMember(t *testing.T) {
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, SemanticThreshold: 0.5}, nil, &mockEmbedder{dim: 64})
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 帮我看看周末爬山装备怎么准备", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}
	// 语义不相关消息（未提及）→ 成员话题切换：脱离原话题、成员清空 → 立即冷却
	m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "今晚足球比赛谁赢了"), nil)
	topics := groupTopics(m)
	if len(topics) != 1 || topics[0].State != TopicCooling {
		t.Fatalf("switch should cool topic: topics=%d state=%v", len(topics), topics[0].State)
	}
	if topics[0].MemberCount() != 0 {
		t.Fatalf("members should be empty, got %d", topics[0].MemberCount())
	}
}

// TestManagerCoolingAfterWindow 窗口内无触碰 → 转冷却（消息序列窗口判定）。
func TestManagerCoolingAfterWindow(t *testing.T) {
	m := testManager(nil, nil, nil)
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 在吗", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}
	// 窗口内（20 条无触碰）→ 保持活跃
	for i := 0; i < 20; i++ {
		m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "other", "闲聊"+strconv.Itoa(i)), nil)
	}
	if topics := groupTopics(m); topics[0].State != TopicActive {
		t.Fatalf("within window: state=%v, want active", topics[0].State)
	}
	// 超出窗口（第 21 条）→ 冷却
	m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "other", "闲聊21"), nil)
	if topics := groupTopics(m); topics[0].State != TopicCooling {
		t.Fatalf("after window: state=%v, want cooling", topics[0].State)
	}
}

// TestManagerReenterRecoversCoolingTopic 冷却话题被强提及 → 恢复活跃。
func TestManagerReenterRecoversCoolingTopic(t *testing.T) {
	m := testManager(nil, nil, nil)
	d1 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 在吗", "bot_self"), nil)
	if !d1.Reply {
		t.Fatal("strong mention should reply")
	}
	for i := 0; i < 21; i++ {
		m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "other", "闲聊"+strconv.Itoa(i)), nil)
	}
	if topics := groupTopics(m); topics[0].State != TopicCooling {
		t.Fatalf("precondition: state=%v, want cooling", topics[0].State)
	}
	d2 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "接着聊刚才的可以吗"), judgeStrong(0.95))
	if !d2.Reply || d2.Topic == nil || d2.Topic.ID != d1.Topic.ID {
		t.Fatalf("reenter should hit same topic, got %v", d2.Topic)
	}
	if !d2.Topic.IsActive() {
		t.Fatal("reenter should restore topic to active")
	}
}

// TestManagerEvidenceRoleTiering 证据分档（借鉴"证据充分时放宽置信度门槛"）：
// 强证据角色（直接称呼/情感对象等）+ 提供证据 → 置信度仅达弱阈值即按强提及直接回复；
// 强证据角色但未提供证据 → 不享有放宽，按严格两档（无证据不强提）；
// 弱证据角色（转述/指代）须达强阈值才强提及，否则按弱提及拉入。
func TestManagerEvidenceRoleTiering(t *testing.T) {
	m := testManager(nil, nil, nil)

	// 强证据角色（affection）+ 提供证据 + 0.5（弱阈值档）→ 按强提及直接回复
	d1 := m.HandleGroupMessage(context.Background(),
		groupMsg(testGroup, "u1", "蓝莓我喜欢你"),
		&LinguisticJudge{IsTalkingToBot: true, Role: RoleAffection, Confidence: 0.5, Evidence: "用户表达对蓝莓的情感"})
	if !d1.Reply {
		t.Fatal("strong evidence role with evidence and weak-threshold confidence should reply")
	}

	// 强证据角色（affection）+ 未提供证据 + 0.5 → 无证据不放宽：按弱提及拉入，不立即回复
	d2 := m.HandleGroupMessage(context.Background(),
		groupMsg(testGroup, "u1", "蓝莓我喜欢你"),
		&LinguisticJudge{IsTalkingToBot: true, Role: RoleAffection, Confidence: 0.5})
	if d2.Reply {
		t.Fatal("strong evidence role WITHOUT evidence should not be relaxed to strong")
	}

	// 弱证据角色（relay）+ 0.5 → 弱提及：非成员拉入但不立即回复
	d3 := m.HandleGroupMessage(context.Background(),
		groupMsg(testGroup, "u2", "你们知道蓝妹吗"),
		&LinguisticJudge{IsTalkingToBot: true, Role: RoleRelay, Confidence: 0.5})
	if d3.Reply {
		t.Fatal("weak evidence role with weak-threshold confidence should not reply immediately")
	}
	if d3.Topic == nil || !d3.Topic.isMember("u2") {
		t.Fatal("weak evidence role should still pull user into topic")
	}

	// 弱证据角色（relay）+ 0.8（强阈值档）→ 强提及直接回复
	d4 := m.HandleGroupMessage(context.Background(),
		groupMsg(testGroup, "u2", "那你呢"),
		&LinguisticJudge{IsTalkingToBot: true, Role: RoleRelay, Confidence: 0.8})
	if !d4.Reply {
		t.Fatal("weak evidence role with strong-threshold confidence should reply")
	}
}

// TestManagerFollowUpRepliesWithoutMention 连续追问：@bot 问第一个问题并回复后，
// 用户不 @ 继续追问（未被 LLM 判为提及），只要话题相关且回复配额有效 → 延续回复。
// 解决"第一个问题明确提 bot、后续问题 bot 不解答"的对话断裂。
func TestManagerFollowUpRepliesWithoutMention(t *testing.T) {
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, CreditEnabled: true}, nil, nil)

	// 第一条：@bot 强提及 → 创建话题并回复
	d1 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 怎么查看积分", "bot_self"), nil)
	if !d1.Reply || d1.Topic == nil {
		t.Fatal("first @bot message should reply and create topic")
	}
	// 模拟 Bot 实际回复 → 授回复配额
	m.RecordBotReply(context.Background(), "qq", testGroup, d1.Topic.ID, "bot_self", "u1", "回复1")

	// 第二条：未提及（judge=nil，语义降级为成员制）→ 相关 + 有配额 → 延续回复
	d2 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "那具体怎么操作呢", "bot_self"), nil)
	if !d2.Reply {
		t.Fatal("follow-up without mention should reply when credit available")
	}
	if d2.Topic == nil || d2.Topic.ID != d1.Topic.ID {
		t.Fatal("follow-up should continue same topic")
	}

	// 第三条：继续未提及追问 → Bot 回复后再授配额 → 继续回复
	m.RecordBotReply(context.Background(), "qq", testGroup, d2.Topic.ID, "bot_self", "u1", "回复2")
	d3 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "好的，那其他群员也能看到吗", "bot_self"), nil)
	if !d3.Reply {
		t.Fatal("second follow-up without mention should keep replying")
	}
}

// TestManagerWeakMentionPullsInAndCreditReplies 弱提及（LLM 判定低置信度）：
// 非成员 → 拉入但不回复、授配额；成员续聊有配额 → 回复；
// 核心回归：配额只在 Bot 实际回复（RecordBotReply）时消耗，
// "决策回复但未实际回复"不吞配额 → 下一条相关消息仍回复。
func TestManagerWeakMentionPullsInAndCreditReplies(t *testing.T) {
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, CreditEnabled: true}, nil, nil)

	// 非成员弱提及 → 拉入话题（静默）、授配额
	d1 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "你们知道蓝妹吗"), judgeWeak(0.5))
	if d1.Reply {
		t.Fatal("weak mention (non-member) should not reply immediately")
	}
	if d1.Topic == nil || !d1.Topic.isMember("u1") {
		t.Fatal("weak mention should pull user into topic")
	}

	// 成员续聊：有配额（弱提及拉入授 1）→ 应回复
	d2 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "嗯 那她一般什么时候在线"), judgeWeak(0.5))
	if !d2.Reply {
		t.Fatal("member continue with credit should reply")
	}

	// 模拟"决策回复但未实际回复"（未走 RecordBotReply）：配额不得被误扣，
	// 下一条相关消息仍应回复（修复"运行一段时间后不再响应"）
	d3 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "好吧 那谢谢啦"), judgeWeak(0.5))
	if !d3.Reply {
		t.Fatal("credit should not be consumed unless actually replied")
	}

	// 真实回复成功后：配额保持可用，下一条相关消息继续回复
	m.RecordBotReply(context.Background(), "qq", testGroup, d1.Topic.ID, "bot_self", "u1", "她一般晚上在线")
	d4 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "好的 谢谢"), judgeWeak(0.5))
	if !d4.Reply {
		t.Fatal("member continue after bot reply (credit) should reply")
	}
}

// TestManagerJudgeNoneSilent LLM 判定未在跟 bot 说话（如第三人称转述）→ 静默，不回复。
func TestManagerJudgeNoneSilent(t *testing.T) {
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, CreditEnabled: true}, nil, nil)
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "你们知道蓝妹吗"),
		&LinguisticJudge{IsTalkingToBot: false, Role: RoleRelay, Confidence: 0.9})
	if d.Reply {
		t.Fatal("relay mention (not talking to bot) should be silent")
	}
	if d.Mention != MentionNone {
		t.Fatalf("mention mode = %v, want none", d.Mention)
	}
	if len(groupTopics(m)) != 0 {
		t.Fatal("no topic should be created for non-mention message")
	}
}

// TestManagerRecordBotReplyGrantsCredit Bot 回复记录 → 消息入窗 + 授配额。
func TestManagerRecordBotReplyGrantsCredit(t *testing.T) {
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, CreditEnabled: true}, nil, nil)
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 帮我看看今天天气怎么样", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}
	m.RecordBotReply(context.Background(), "qq", testGroup, d.Topic.ID, "bot_self", "u1", "今天晴转多云")
	topic := groupTopics(m)[0]
	foundBot := false
	for _, tm := range topic.MsgWindow {
		if tm.IsBot && tm.UserID == "bot_self" {
			foundBot = true
		}
	}
	if !foundBot {
		t.Fatal("bot reply should be recorded in topic window")
	}
	d2 := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "那明天呢"), judgeWeak(0.5))
	if !d2.Reply {
		t.Fatal("member continue after bot reply (credit) should reply")
	}
}

// TestManagerArchiveCoolingTopic 冷却超时 → 归档（异步）→ 从内存与持久化移除。
func TestManagerArchiveCoolingTopic(t *testing.T) {
	store := memory.New()
	arch := NewArchiver(nil, nil, nil, nil, zap.NewNop())
	m := NewManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1}, store, nil, nil, arch, nil, zap.NewNop())
	d := m.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 在吗", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}
	topic := groupTopics(m)[0]
	topic.markCooling()
	topic.LastActiveAt = time.Now().Add(-2 * time.Minute) // 超过冷却超时

	m.scanAndArchive(context.Background())
	m.wg.Wait() // 等待异步归档完成

	if len(m.groups) != 0 {
		t.Fatalf("after archive groups = %d, want 0", len(m.groups))
	}
	// 持久化键也应删除
	if got, _ := store.Get(context.Background(), m.redisKey(m.groupKey("qq", testGroup))); got != "" {
		t.Fatalf("persisted topic key not cleaned up: %q", got)
	}
}

// TestManagerRestoreFromStore 持久化 → 重建管理器恢复话题状态。
func TestManagerRestoreFromStore(t *testing.T) {
	store := memory.New()
	m1 := testManager(nil, store, nil)
	d := m1.HandleGroupMessage(context.Background(), groupMsg(testGroup, "u1", "@蓝妹 你好吗", "bot_self"), nil)
	if !d.Reply {
		t.Fatal("strong mention should reply")
	}

	// 模拟重启：同存储新建管理器并恢复
	m2 := testManager(nil, store, nil)
	m2.restore(context.Background())
	topics, ok := m2.groups[m2.groupKey("qq", testGroup)]
	if !ok || len(topics) != 1 {
		t.Fatalf("restore: group missing or topics=%d", len(topics))
	}
	if topics[0].LastTouchSeq != 0 {
		t.Fatalf("restore should reset LastTouchSeq, got %d", topics[0].LastTouchSeq)
	}
	if !topics[0].isMember("u1") {
		t.Fatal("restore should keep members")
	}
	if len(topics[0].MsgWindow) == 0 {
		t.Fatal("restore should keep message window")
	}
	if !topics[0].IsActive() {
		t.Fatalf("restore with window+members should stay active, got %v", topics[0].State)
	}
}

// TestManagerConcurrentHandling 并发压测：多协程同时投递群消息，
// 验证状态机/序列/成员/持久化在读写锁下的并发安全（配合 go test -race）。
func TestManagerConcurrentHandling(t *testing.T) {
	store := memory.New()
	m := testManager(&config.TopicConfig{TopicWindowMsgs: 20, CoolingTimeoutMinutes: 1, CreditEnabled: true}, store, nil)

	const workers = 32
	const perWorker = 25
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				uid := fmt.Sprintf("u%d", (w+i)%5)
				content := "闲聊" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
				var at []string
				if i%4 == 0 {
					content = "@蓝妹 帮我看看" + strconv.Itoa(i)
					at = []string{"bot_self"}
				}
				m.HandleGroupMessage(context.Background(), groupMsg(testGroup, uid, content, at...), nil)
			}
		}(w)
	}
	wg.Wait()

	// 收尾校验：不变量检查 —— 活跃话题必须至少有一名成员
	//（成员切换路径保证"成员清空即冷却"，冷却可由窗口到期或成员清空触发，
	// 因此冷却话题仍可能保留成员，属合法状态）。
	m.mu.RLock()
	defer m.mu.RUnlock()
	for gk, topics := range m.groups {
		for _, topic := range topics {
			if topic.IsActive() && topic.MemberCount() == 0 {
				t.Errorf("group %s topic %s: active but empty members", gk, topic.ID)
			}
		}
	}
}
