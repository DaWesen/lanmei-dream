package model

import (
	"encoding/json"
	"math"
	"testing"
)

// TestParseFactsNewFormat 新格式 []FactItem 直接解析并归一化。
func TestParseFactsNewFormat(t *testing.T) {
	raw := []byte(`[{"value":"用户喜欢猫","confidence":0.85},{"value":"用户的猫叫小雪","confidence":1.5},{"value":"  ","confidence":0.9}]`)
	facts := ParseFacts(raw)
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2 (空白 value 应被跳过)", len(facts))
	}
	if facts[0].Value != "用户喜欢猫" || facts[0].Confidence != 0.85 {
		t.Fatalf("unexpected first fact: %+v", facts[0])
	}
	if facts[1].Confidence != 1.0 {
		t.Fatalf("confidence 越界未收敛: got %v, want 1.0", facts[1].Confidence)
	}
}

// TestParseFactsLegacyFormat 旧格式 []string 按 0.5 置信度转换。
func TestParseFactsLegacyFormat(t *testing.T) {
	raw, _ := json.Marshal([]string{"用户喜欢猫", "用户提到贫血"})
	facts := ParseFacts(raw)
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2", len(facts))
	}
	for _, f := range facts {
		if f.Confidence != 0.5 {
			t.Fatalf("legacy fact confidence = %v, want 0.5", f.Confidence)
		}
	}
}

// TestParseFactsEmpty 空/非法输入返回 nil。
func TestParseFactsEmpty(t *testing.T) {
	if facts := ParseFacts(nil); facts != nil {
		t.Fatal("nil input should return nil")
	}
	if facts := ParseFacts([]byte(`not json`)); facts != nil {
		t.Fatal("invalid json should return nil")
	}
	if facts := ParseFacts([]byte(`null`)); facts != nil {
		t.Fatal("null should return nil")
	}
}

// TestMergeFactsConfirmBump 相同 value 重复确认 → 置信度 +0.05 并封顶 0.98。
func TestMergeFactsConfirmBump(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Value: "用户喜欢猫", Confidence: 0.8}},
		[]FactItem{{Value: "用户喜欢猫", Confidence: 0.8}},
	)
	if len(merged) != 1 {
		t.Fatalf("got %d facts, want 1", len(merged))
	}
	if got := merged[0].Confidence; math.Abs(got-0.85) > 1e-9 {
		t.Fatalf("confirm bump: got %v, want 0.85", got)
	}

	// 反复确认封顶 0.98，不到 1.0
	cur := merged[0].Confidence
	for i := 0; i < 20; i++ {
		merged = MergeFacts(merged, []FactItem{{Value: "用户喜欢猫", Confidence: 0.9}})
		cur = merged[0].Confidence
		if cur > 0.98+1e-9 {
			t.Fatalf("confidence exceeded cap: %v", cur)
		}
	}
	if math.Abs(cur-0.98) > 1e-9 {
		t.Fatalf("expected cap 0.98, got %v", cur)
	}
}

// TestMergeFactsInsertAndKeepDifferent 不同 value 各自保留（不自动判定矛盾）。
func TestMergeFactsInsertAndKeepDifferent(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Value: "用户喜欢猫", Confidence: 0.8}},
		[]FactItem{{Value: "用户养了一只狗", Confidence: 0.6}},
	)
	if len(merged) != 2 {
		t.Fatalf("got %d facts, want 2 (不同事实各自保留)", len(merged))
	}
}

// TestMergeFactsEvidenceMerge 证据合并去重、新证据在前。
func TestMergeFactsEvidenceMerge(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Value: "用户喜欢猫", Confidence: 0.8, Evidence: []string{"conv:1-10"}}},
		[]FactItem{{Value: "用户喜欢猫", Confidence: 0.8, Evidence: []string{"conv:2-11", "conv:1-10"}}},
	)
	if len(merged[0].Evidence) != 2 {
		t.Fatalf("evidence should be deduped, got %v", merged[0].Evidence)
	}
	if merged[0].Evidence[0] != "conv:2-11" {
		t.Fatalf("new evidence should come first, got %v", merged[0].Evidence)
	}
}

// TestMergeFactsConflictDowngrade 同 Key 不同 Value → 矛盾降置信（保底 0.2）、保留新值、旧值记入 Conflict。
func TestMergeFactsConflictDowngrade(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Key: "猫的毛色", Value: "白色", Confidence: 0.85}},
		[]FactItem{{Key: "猫的毛色", Value: "黑色", Confidence: 0.8}},
	)
	if len(merged) != 1 {
		t.Fatalf("conflict should collapse to one entry, got %d", len(merged))
	}
	f := merged[0]
	if f.Value != "黑色" {
		t.Fatalf("conflict should keep new value, got %q", f.Value)
	}
	if f.Conflict != "白色" {
		t.Fatalf("conflict should record old value, got %q", f.Conflict)
	}
	// 0.85 vs 0.8 → min 0.8 − 0.15 = 0.65
	if math.Abs(f.Confidence-0.65) > 1e-9 {
		t.Fatalf("conflict downgrade: got %v, want 0.65", f.Confidence)
	}
}

// TestMergeFactsConflictFloor 矛盾降置信不跌破保底 0.2。
func TestMergeFactsConflictFloor(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Key: "k", Value: "v1", Confidence: 0.3}},
		[]FactItem{{Key: "k", Value: "v2", Confidence: 0.25}},
	)
	if math.Abs(merged[0].Confidence-0.2) > 1e-9 {
		t.Fatalf("conflict floor: got %v, want 0.2", merged[0].Confidence)
	}
}

// TestMergeFactsSameKeyDifferentValueNotConflict 同 Key 但 value 相同 → 走确认分支而非矛盾。
func TestMergeFactsSameKeyConfirm(t *testing.T) {
	merged := MergeFacts(
		[]FactItem{{Key: "宠物", Value: "用户喜欢猫", Confidence: 0.8}},
		[]FactItem{{Key: "宠物", Value: "用户喜欢猫", Confidence: 0.8}},
	)
	if len(merged) != 1 {
		t.Fatalf("same value should confirm, got %d entries", len(merged))
	}
	if merged[0].Conflict != "" {
		t.Fatalf("confirm should not set conflict, got %q", merged[0].Conflict)
	}
}

// TestMarshalFactsRoundTrip 序列化后再解析保持一致。
func TestMarshalFactsRoundTrip(t *testing.T) {
	facts := []FactItem{{Value: "用户喜欢猫", Confidence: 0.85, Evidence: []string{"conv:1-10"}}}
	raw := MarshalFacts(facts)
	back := ParseFacts(raw)
	if len(back) != 1 || back[0].Value != facts[0].Value || back[0].Confidence != facts[0].Confidence {
		t.Fatalf("round trip mismatch: %+v", back)
	}
}
