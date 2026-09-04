package ai

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// moodWindowCapacity 每个 scope 保留的发图情绪记录条数（环形容量）。
const moodWindowCapacity = 12

// moodSnapshotMax Snapshot 注入提示词时最多展示的记录条数（避免提示词过长）。
const moodSnapshotMax = 5

// moodEntry 一次表情发送事件的记录。
type moodEntry struct {
	emotion string    // 情绪标签（pick_sticker 调用时 LLM 给的 emotion 参数）
	gap     int       // 本次发图距上次发图的对话轮数（首次发图为 0）
	at      time.Time // 发图时刻（渲染相对时间用）
}

// scopeState 单个会话作用域的窗口状态。
type scopeState struct {
	entries []moodEntry // 环形缓冲（容量 moodWindowCapacity）
	rounds  int         // 距上次发图已进行的对话轮数（纯文字轮 +1，发图时读值清零）
}

// MoodWindow 表情情绪流滑动窗口（轻量短期记忆，纯内存不持久化，重启清零）。
// 按 scope 隔离（群聊 groupID / 私聊 "dm:"+平台用户ID），
// 供提示词注入"最近的表情情绪历史"，约束 LLM 发表情的频率与重复度。
//
// scopes map 不淘汰旧 scope（沿袭旧回复计数器的相同策略）：
// 条目数量级 = 群/私聊会话数，每个 scope 仅几十字节，增长量可接受。
type MoodWindow struct {
	mu     sync.Mutex
	scopes map[string]*scopeState
}

// NewMoodWindow 创建表情情绪流滑动窗口。
func NewMoodWindow() *MoodWindow {
	return &MoodWindow{scopes: make(map[string]*scopeState)}
}

// Tick 每轮纯文字 LLM 回复后调用，累计距上次发图的对话轮数。
func (w *MoodWindow) Tick(scope string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state(scope)
	st.rounds++
}

// Record 本轮真实发出表情后调用：记录情绪标签并清零轮数计数。
// gap 取记录时的 rounds（即本次发图距上次发图的轮数），存入 entry 自身后 rounds 归零。
func (w *MoodWindow) Record(scope, emotion string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state(scope)
	st.entries = append(st.entries, moodEntry{emotion: emotion, gap: st.rounds, at: time.Now()})
	// 超出容量淘汰最旧（slice 头部淘汰即可，量小无需真环形）。
	if len(st.entries) > moodWindowCapacity {
		st.entries = st.entries[len(st.entries)-moodWindowCapacity:]
	}
	st.rounds = 0
}

// Snapshot 生成注入提示词的表情情绪历史摘要。
//   - 该 scope 从未发过图（entries 为空）：提示"可主动发"，鼓励情绪合适时首次发表情；
//   - 已有记录：给出距上次发图的轮数 + 最近几条情绪记录（含距今轮数与相对时间），
//     供 LLM 自主控制发图节奏（别刷屏、别重复发相近情绪）。
func (w *MoodWindow) Snapshot(scope string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.scopes[scope]
	if !ok || len(st.entries) == 0 {
		return "本会话尚未主动发表情，当前情绪合适时可主动发"
	}

	// 距今轮数推导（倒序累加 gap）：
	// rounds 是"距最近一次发图的轮数"，故最新 entry（第 1 新）距今 = rounds；
	// gap 的语义是"本条记录发图时距上一次（更旧一条）发图隔的轮数"，存在每条 entry 自身。
	// 因此每往旧走一条，累加"较新那条"的 gap：
	//   第 k 新距今 = rounds + gap₁ + gap₂ + … + gap₍k₋₁₎（gapᵢ 为第 i 新记录的 gap）。
	// 即倒序遍历（新→旧）：第 1 条只取 rounds，之后每往旧走一条累加那条（较新条目）的 gap。
	parts := make([]string, 0, moodSnapshotMax)
	acc := st.rounds
	now := time.Now()
	for i := len(st.entries) - 1; i >= 0 && len(parts) < moodSnapshotMax; i-- {
		e := st.entries[i]
		parts = append(parts, fmt.Sprintf("【%s】%d 轮前（约 %s）", e.emotion, acc, relTime(now.Sub(e.at))))
		acc += e.gap
	}
	return fmt.Sprintf("距上次发表情已 %d 轮对话；最近的表情情绪：%s。避免短时间内重复发表情或发相近情绪",
		st.rounds, strings.Join(parts, "、"))
}

// state 取指定作用域的状态（调用方须已持锁；不存在时惰性创建）。
func (w *MoodWindow) state(scope string) *scopeState {
	st, ok := w.scopes[scope]
	if !ok {
		st = &scopeState{}
		w.scopes[scope] = st
	}
	return st
}

// relTime 按时长渲染相对时间：<1h → "X 分钟前"、<24h → "X 小时前"、更长 → "X 天前"（X 取整）。
func relTime(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}
