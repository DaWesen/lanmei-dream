package model

import (
	"encoding/json"
	"strings"
	"time"
)

// FactItem 一条结构化事实，带置信度与证据来源。
//
// 设计（借鉴蒸馏管线"结论须可核实、置信度随重复确认演化、矛盾降置信保留双结论"）：
//   - Key：命题主题（细粒度，如"猫的毛色"而非"宠物"），同一 Key 应只有一种取值；
//     用于矛盾检测（同 Key 不同 Value = 冲突，降置信而非武断丢弃）；
//   - Confidence：该事实的置信度 0~1。压缩时由 LLM 自评；
//     跨摘要合并时重复确认 +0.05 封顶 0.98；矛盾 −0.15 保底 0.2；
//   - Evidence：证据来源标识（如对话批次 "conv:first-last"），供可审计；
//   - Conflict：矛盾时被替换的旧值（保留双结论，供审阅）；非矛盾为空；
//   - At：最近证据来源条目的创建时间（消费端判断"证据较早"用）。
//
// 存储形式：EpisodeSummary.Facts / TopicCluster.Facts 为 jsonb，内容是 []FactItem；
// 兼容旧数据（jsonb 为 []string 时按 0.5 置信度转换）。
type FactItem struct {
	Key        string    `json:"key,omitempty"`      // 命题主题（LLM 编，细粒度）
	Value      string    `json:"value"`              // 事实内容
	Confidence float64   `json:"confidence"`         // 置信度 0~1
	Evidence   []string  `json:"evidence,omitempty"` // 证据来源
	Conflict   string    `json:"conflict,omitempty"` // 矛盾时被替换的旧值
	At         time.Time `json:"at,omitempty"`       // 最近证据来源时间
}

// maxFactConfidence 重复确认的置信度封顶：任何路径都到不了 1.0。
const maxFactConfidence = 0.98

// factBumpStep 每次重复确认的置信度提升步长。
const factBumpStep = 0.05

// factConflictFloor 矛盾降置信的保底值（distill：0.2）。
const factConflictFloor = 0.2

// factConflictPenalty 矛盾时的置信度惩罚（distill：−0.15）。
const factConflictPenalty = 0.15

// maxFactEvidence 单条事实保留的证据条数上限（防止行被撑大）。
const maxFactEvidence = 50

// factDefaultConfidence 无置信度信息（旧数据/LLM 未给出）时的兜底值。
const factDefaultConfidence = 0.5

// 消费端门槛（借鉴蒸馏管线"越靠近 agent 门槛越高"）：
//   - FactMinConfidence：低于此值的事实不进对话上下文（给 agent 的必须站得住）；
//   - FactThinConfidence：0.5~0.65 的事实进上下文但标注"⚠︎证据较少"。
const (
	FactMinConfidence  = 0.5
	FactThinConfidence = 0.65
)

// ParseFacts 解析 Facts jsonb，兼容两种形态：
//   - []FactItem（新）：直接采用；
//   - []string（旧）：转换为置信度 0.5 的事实。
//
// 解析失败/空返回 nil（调用方可安全 for 循环）。
func ParseFacts(raw []byte) []FactItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var items []FactItem
	if err := json.Unmarshal(raw, &items); err == nil {
		out := make([]FactItem, 0, len(items))
		for _, it := range items {
			it.Value = strings.TrimSpace(it.Value)
			if it.Value == "" {
				continue
			}
			it.Confidence = normalizeFactConfidence(it.Confidence)
			out = append(out, it)
		}
		return out
	}

	// 旧格式：[]string
	var legacy []string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		out := make([]FactItem, 0, len(legacy))
		for _, s := range legacy {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			out = append(out, FactItem{Value: s, Confidence: factDefaultConfidence})
		}
		return out
	}
	return nil
}

// MarshalFacts 序列化 []FactItem 为 jsonb（nil 时输出 "[]"）。
func MarshalFacts(facts []FactItem) []byte {
	if facts == nil {
		facts = []FactItem{}
	}
	data, err := json.Marshal(facts)
	if err != nil {
		return []byte("[]")
	}
	return data
}

// MergeFacts 跨摘要合并事实集合（三态 + 矛盾降置信，以 value 精确匹配为稳定 key）：
//   - insert：新 value → 追加，保留原置信度；
//   - confirm：相同 value 重复确认 → 置信度 min(0.98, max(旧,新) + 0.05)，证据合并；
//   - conflict：同 Key 但 value 不同（同一命题的相反/不同取值）→
//     置信度 max(0.2, min(旧,新) − 0.15)，保留新 value，旧 value 记入 Conflict（不武断丢弃）；
//   - 无 Key 或不同 Key 的不同 value → 各自保留（insert）。
//
// 语义：重复确认不等于绝对真理（封顶 0.98）；矛盾暴露不确定性（降置信而非删除），
// 与"保留双结论供人判断"一致。
func MergeFacts(existing, incoming []FactItem) []FactItem {
	out := make([]FactItem, 0, len(existing)+len(incoming))
	valueIdx := make(map[string]int, len(existing)+len(incoming)) // value -> out 索引
	keyIdx := make(map[string]int, len(existing)+len(incoming))   // key -> out 索引

	for _, f := range existing {
		f.Value = strings.TrimSpace(f.Value)
		if f.Value == "" {
			continue
		}
		if _, dup := valueIdx[f.Value]; dup {
			continue
		}
		f.Confidence = normalizeFactConfidence(f.Confidence)
		valueIdx[f.Value] = len(out)
		if f.Key != "" {
			keyIdx[f.Key] = len(out)
		}
		out = append(out, f)
	}

	for _, f := range incoming {
		f.Value = strings.TrimSpace(f.Value)
		if f.Value == "" {
			continue
		}
		f.Confidence = normalizeFactConfidence(f.Confidence)

		// 1) 确认：同一 value 重复出现
		if i, ok := valueIdx[f.Value]; ok {
			out[i].Confidence = min(max(out[i].Confidence, f.Confidence)+factBumpStep, maxFactConfidence)
			out[i].Evidence = mergeFactEvidence(out[i].Evidence, f.Evidence)
			if f.At.After(out[i].At) {
				out[i].At = f.At
			}
			if f.Key != "" {
				keyIdx[f.Key] = i
			}
			continue
		}

		// 2) 矛盾：同 Key 但 value 不同（同一命题出现相反/不同取值）
		if f.Key != "" {
			if j, ok := keyIdx[f.Key]; ok && out[j].Value != f.Value {
				old := out[j]
				nc := min(old.Confidence, f.Confidence) - factConflictPenalty
				if nc < factConflictFloor {
					nc = factConflictFloor
				}
				out[j].Value = f.Value
				out[j].Confidence = nc
				out[j].Conflict = old.Value
				out[j].Evidence = mergeFactEvidence(old.Evidence, f.Evidence)
				if f.At.After(old.At) {
					out[j].At = f.At
				}
				delete(valueIdx, old.Value)
				valueIdx[f.Value] = j
				keyIdx[f.Key] = j
				continue
			}
		}

		// 3) 插入：新事实
		valueIdx[f.Value] = len(out)
		if f.Key != "" {
			keyIdx[f.Key] = len(out)
		}
		out = append(out, f)
	}
	return out
}

// mergeFactEvidence 合并证据列表（去重、新证据在前、上限 maxFactEvidence）。
func mergeFactEvidence(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, e := range incoming {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		merged = append(merged, e)
	}
	for _, e := range existing {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		merged = append(merged, e)
	}
	if len(merged) > maxFactEvidence {
		merged = merged[:maxFactEvidence]
	}
	return merged
}

// normalizeFactConfidence 归一化置信度：越界收敛 0~1；垃圾值（NaN）按 0.5 兜底。
func normalizeFactConfidence(v float64) float64 {
	if v != v { // NaN
		return factDefaultConfidence
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
