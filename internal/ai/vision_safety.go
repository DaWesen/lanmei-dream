package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
)

const imageSafetySystemPrompt = `你是严格的图片内容安全审核器。只根据图片中实际可见内容判断，不猜测图片外信息。

将图片归入以下唯一类别：
- safe：普通风景、动物、食物、物品，或穿着正常且没有性暗示姿势、构图和身体强调的人物
- suggestive：泳装、内衣、浴巾、湿身、透视、走光、明显裸露上身、强调胸部臀部大腿等私密部位、性暗示姿势或恋物表现
- adult：裸体、可见性器官或乳头、性行为、成人用品或明确色情内容
- uncertain：无法确认、画面不清、遮挡严重，或无法可靠判断人物是否被性化

任何未成年或疑似未成年人物的性化表现至少归为 suggestive。动漫、插画、真人使用相同标准。
仅输出一行 JSON，不要 Markdown，不要解释：
{"verdict":"safe|suggestive|adult|uncertain","confidence":0.0,"reasons":["简短原因"]}`

// ImageSafetyVerdict 是视觉审核的封闭结论集合。
type ImageSafetyVerdict string

const (
	ImageSafetySafe       ImageSafetyVerdict = "safe"
	ImageSafetySuggestive ImageSafetyVerdict = "suggestive"
	ImageSafetyAdult      ImageSafetyVerdict = "adult"
	ImageSafetyUncertain  ImageSafetyVerdict = "uncertain"
)

// ImageSafetyResult 是结构化图片安全审核结果。
type ImageSafetyResult struct {
	Verdict    ImageSafetyVerdict `json:"verdict"`
	Confidence float64            `json:"confidence"`
	Reasons    []string           `json:"reasons"`
}

// IsSafe 仅在明确安全且置信度达到阈值时返回 true。
func (r *ImageSafetyResult) IsSafe(minConfidence float64) bool {
	return r != nil && r.Verdict == ImageSafetySafe && r.Confidence >= minConfidence
}

// ModerateImage 对实际图片字节执行结构化内容安全审核。
// 调用方必须采用失败关闭策略：任何错误都不得当作安全结果继续处理。
func (v *VisionService) ModerateImage(ctx context.Context, imageData []byte, mime string) (*ImageSafetyResult, error) {
	if len(imageData) == 0 {
		return nil, errors.New("vision: image data 为空")
	}
	if v.model == nil {
		return nil, errors.New("vision: 模型未配置")
	}
	if mime == "" {
		mime = "image/png"
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("vision: 不支持的图片 MIME %q", mime)
	}

	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imageData)
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	msgs := []*schema.Message{
		{Role: schema.System, Content: imageSafetySystemPrompt},
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &dataURL},
			}},
		}},
	}
	resp, err := v.model.Generate(ctx, msgs, llm.DisableThinkingOption())
	if err != nil {
		return nil, fmt.Errorf("vision: 图片安全审核失败: %w", err)
	}
	if resp == nil {
		return nil, errors.New("vision: 图片安全审核返回空响应")
	}
	result, err := parseImageSafetyResult(resp.Content)
	if err != nil {
		return nil, err
	}
	v.logger.Info("图片安全审核完成",
		zap.String("verdict", string(result.Verdict)),
		zap.Float64("confidence", result.Confidence))
	return result, nil
}

func parseImageSafetyResult(raw string) (*ImageSafetyResult, error) {
	raw = strings.TrimSpace(raw)
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return nil, errors.New("vision: 图片安全审核结果不是 JSON")
	}

	var result ImageSafetyResult
	if err := json.Unmarshal([]byte(raw[start:end+1]), &result); err != nil {
		return nil, fmt.Errorf("vision: 解析图片安全审核结果: %w", err)
	}
	switch result.Verdict {
	case ImageSafetySafe, ImageSafetySuggestive, ImageSafetyAdult, ImageSafetyUncertain:
	default:
		return nil, fmt.Errorf("vision: 未知图片安全结论 %q", result.Verdict)
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return nil, fmt.Errorf("vision: 图片安全置信度越界 %.4f", result.Confidence)
	}

	reasons := make([]string, 0, min(len(result.Reasons), 8))
	for _, reason := range result.Reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		runes := []rune(reason)
		if len(runes) > 80 {
			reason = string(runes[:80])
		}
		reasons = append(reasons, reason)
		if len(reasons) == 8 {
			break
		}
	}
	result.Reasons = reasons
	return &result, nil
}
