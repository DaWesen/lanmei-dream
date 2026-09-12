package bizplugin

import (
	"reflect"
	"testing"
)

func TestBuildWelcomeSegments(t *testing.T) {
	tests := []struct {
		name      string
		newUserID string
		content   string
		imageFile string
		wantTypes []string
	}{
		{
			name:      "正常：at+文本+图片三段",
			newUserID: "12345",
			content:   "欢迎来到蓝山招新群！",
			imageFile: "base64://abc",
			wantTypes: []string{"at", "text", "image"},
		},
		{
			name:      "RustFS 不可用：降级 at+文本两段",
			newUserID: "12345",
			content:   "欢迎来到蓝山招新群！",
			imageFile: "",
			wantTypes: []string{"at", "text"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs := buildWelcomeSegments(tt.newUserID, tt.content, tt.imageFile)
			if len(segs) != len(tt.wantTypes) {
				t.Fatalf("段数 = %d, want %d（%v）", len(segs), len(tt.wantTypes), segs)
			}
			gotTypes := make([]string, 0, len(segs))
			for _, seg := range segs {
				gotTypes = append(gotTypes, seg["type"].(string))
			}
			if !reflect.DeepEqual(gotTypes, tt.wantTypes) {
				t.Errorf("段类型序列 = %v, want %v", gotTypes, tt.wantTypes)
			}
		})
	}

	t.Run("图片段 file 字段透传", func(t *testing.T) {
		segs := buildWelcomeSegments("12345", "欢迎", "base64://abc")
		img := segs[2]
		data, ok := img["data"].(map[string]any)
		if !ok {
			t.Fatalf("image 段 data 缺失或类型不符: %v", img)
		}
		if file, _ := data["file"].(string); file != "base64://abc" {
			t.Errorf("image file = %q, want %q", file, "base64://abc")
		}
	})
}
