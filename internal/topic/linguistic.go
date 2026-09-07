package topic

// 提及判断（LLM 主判官）的数据类型。
//
// 设计演进：
//   - 旧方案：硬编码文本结构规则（呼格/祈使/被动）识别提及，规则无法穷尽
//     自然语言句式，导致大量提及被漏判而静默；
//   - 新方案：提及判断并入意图分析 LLM 调用（见 internal/ai/intent），
//     一次调用同时返回意图与"是否在跟机器人说话"，话题系统按
//     LinguisticJudge 的置信度划分强/弱提及；at（平台 ID）仍为免费强信号。

// MentionRole 提及的语言学方式（LLM 分析返回的角色分类）。
// 用于日志可观测与后续决策参考；强弱主要由置信度阈值决定。
type MentionRole string

const (
	RoleNone           MentionRole = "none"              // 无提及
	RoleVocative       MentionRole = "vocative"          // 呼格：直接叫名字
	RoleSubject        MentionRole = "subject"           // 主语提及：机器人是句子主语
	RoleImperativeObj  MentionRole = "imperative_object" // 祈使/请求宾语：让机器人做事
	RoleRelativeClause MentionRole = "relative_clause"   // 关系从句提及
	RoleConditional    MentionRole = "conditional"       // 条件句/假设句提及
	RoleTopicMarker    MentionRole = "topic_marker"      // 话题标记式提及（"说到蓝妹…"）
	RoleAffection      MentionRole = "affection"         // 情感/评价对象（"蓝莓我喜欢你"）
	RoleRelay          MentionRole = "relay"             // 传话/第三人称提及（不指向机器人）
)

// LinguisticJudge 提及判定（由意图分析 LLM 调用返回）。
// TopicGatePass 将 intent.Result 中的提及字段转换为本类型后传入
// Manager.HandleGroupMessage 作为提及决策依据。
type LinguisticJudge struct {
	IsTalkingToBot bool        // 是否在"跟机器人说话"（期望机器人回应）
	Role           MentionRole // 提及角色
	Confidence     float64     // 提及置信度 0~1
	Evidence       string      // 提及判断依据（LLM 给出的可核实理由，用于证据分档与日志）
}

// strongEvidenceRole 强证据提及角色：直接称呼/让机器人做事/以机器人为对象表达情感——
// 这类角色在结构上就明确"在跟机器人说话"，置信度只需达弱阈值即可按强提及处理
// （弱阈值对应"证据充分但信心略低"，不必苛求高置信度）。
var strongEvidenceRole = map[MentionRole]bool{
	RoleVocative:      true, // 直接叫名字
	RoleSubject:       true, // 机器人是句子主语
	RoleImperativeObj: true, // 让机器人做事
	RoleAffection:     true, // 情感/评价对象
}

// linguisticStrongThreshold 提及判断"强提及"置信度阈值（默认 0.7）。
func (m *Manager) linguisticStrongThreshold() float64 {
	if m.cfg != nil && m.cfg.LinguisticStrongThreshold > 0 {
		return m.cfg.LinguisticStrongThreshold
	}
	return 0.7
}

// StrongThreshold 返回"强提及"置信度阈值（供启动日志等外部展示）。
func (m *Manager) StrongThreshold() float64 { return m.linguisticStrongThreshold() }

// linguisticWeakThreshold 提及判断"弱提及"置信度下限（默认 0.4）。
func (m *Manager) linguisticWeakThreshold() float64 {
	if m.cfg != nil && m.cfg.LinguisticWeakThreshold > 0 {
		return m.cfg.LinguisticWeakThreshold
	}
	return 0.4
}

// WeakThreshold 返回"弱提及"置信度下限（供启动日志等外部展示）。
func (m *Manager) WeakThreshold() float64 { return m.linguisticWeakThreshold() }
