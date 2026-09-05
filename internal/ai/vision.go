package ai

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/DaWesen/lanmei-dream/internal/ai/llm"
)

// visionSystemPrompt 视觉理解系统提示：要求模型以中文客观描述图片内容。
const visionSystemPrompt = "你是图片理解助手。请用简体中文客观、简洁地描述这张图片的内容（主体、场景、文字、氛围等），" +
	"控制在 200 字以内，不要猜测图片之外的信息。"

// stickerTagSystemPrompt 表情包标签生成系统提示：输出 2~4 个简短中文标签。
// 标签用于表情库检索（ILIKE 双向匹配 + trgm 相似度），长句标签无法命中，
// 因此严格要求"简短词"而非描述句。
const stickerTagSystemPrompt = "你是表情包标注助手。看图生成 2~4 个适合检索的简短中文标签，" +
	"描述情绪、内容与使用场景（如：无语、吐槽、开心、被坑、求安慰、委屈）。" +
	"只输出标签本身，用中文逗号分隔，不要输出任何其他内容。"

// VisionService 基于多模态 LLM 的图片理解服务。
//
// 降级链（调用方 MediaPass 负责）：
//  1. 多模态模型可用 → 返回文字描述；
//  2. 模型调用失败 → Describe 返回错误，调用方回退 "[图片]" 占位；
//  3. 未配置视觉模型（Vision=nil）→ 直接占位，不进入本服务。
type VisionService struct {
	model        model.BaseChatModel
	systemPrompt string
	timeout      time.Duration // 单次理解超时
	maxDescLen   int           // 描述最大长度（rune）
	logger       *zap.Logger
}

// NewVisionService 创建视觉理解服务。
// m 为支持多模态输入的 eino 模型（建议使用独立的视觉模型，不占用主对话模型配额）。
func NewVisionService(m model.BaseChatModel, logger *zap.Logger) *VisionService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &VisionService{
		model:        m,
		systemPrompt: visionSystemPrompt,
		timeout:      30 * time.Second,
		maxDescLen:   200,
		logger:       logger,
	}
}

// Describe 对图片 URL 执行视觉理解，返回中文文字描述。
// imageURL 需为模型可访问的 URL（可传 RustFS 预签名 URL）。
func (v *VisionService) Describe(ctx context.Context, imageURL string) (string, error) {
	if strings.TrimSpace(imageURL) == "" {
		return "", errors.New("vision: image url 为空")
	}
	if v.model == nil {
		return "", errors.New("vision: 模型未配置")
	}

	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	msgs := []*schema.Message{
		{Role: schema.System, Content: v.systemPrompt},
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &imageURL},
			}},
		}},
	}

	resp, err := v.model.Generate(ctx, msgs)
	if err != nil {
		return "", fmt.Errorf("vision: 图片理解失败: %w", err)
	}

	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		return "", errors.New("vision: 模型返回空描述")
	}

	// rune 级截断，避免切割 UTF-8 字符
        r := []rune(desc)
        if len(r) > v.maxDescLen {
                desc = string(r[:v.maxDescLen]) + "…"
        }
        v.logger.Debug("图片理解完成", zap.String("url", imageURL), zap.Int("len", len([]rune(desc))))
        return desc, nil
}

// GenerateTags 用视觉模型为表情包图片生成检索标签（2~4 个简短中文词）。
// imageData 为图片字节，以 data URL 内嵌请求——无需对象存储可被 API 公网访问，
// 本地/内网部署（RustFS 预签名 URL 外部模型拉不到）也能打标。
// 打标是短输出任务，调用关闭推理思考（thinking=disabled）：
// 推理模型开思考会拖慢响应，且可能出现"只思考不输出正文"（PR #39 同源问题）。
func (v *VisionService) GenerateTags(ctx context.Context, imageData []byte, mime string) ([]string, error) {
        if len(imageData) == 0 {
                return nil, errors.New("vision: image data 为空")
        }
        if v.model == nil {
                return nil, errors.New("vision: 模型未配置")
        }
        if mime == "" {
                mime = "image/png"
        }
        dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imageData)

        ctx, cancel := context.WithTimeout(ctx, v.timeout)
        defer cancel()

        msgs := []*schema.Message{
                {Role: schema.System, Content: stickerTagSystemPrompt},
                {Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
                        {Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
                                MessagePartCommon: schema.MessagePartCommon{URL: &dataURL},
                        }},
                }},
        }
        resp, err := v.model.Generate(ctx, msgs, llm.DisableThinkingOption())
        if err != nil {
                return nil, fmt.Errorf("vision: 表情标签生成失败: %w", err)
        }

        tags := parseStickerTags(resp.Content)
        if len(tags) == 0 {
                return nil, fmt.Errorf("vision: 标签解析为空（原始输出: %.60q）", resp.Content)
        }
        v.logger.Info("表情标签生成完成", zap.Strings("tags", tags))
        return tags, nil
}

// parseStickerTags 解析视觉模型返回的标签串：按逗号/顿号/分号/空白切分，
// 剥离 markdown 反引号与引号包裹，去重；过滤空项与超长项
// （>12 rune 的是描述句不是检索标签，恰是本次自动打标要消灭的数据质量问题）。
func parseStickerTags(raw string) []string {
        s := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "`"))
        if s == "" {
                return nil
        }
        parts := strings.FieldsFunc(s, func(r rune) bool {
                return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' ||
                        r == '\n' || r == '\r' || r == ' ' || r == '\t'
        })
        seen := make(map[string]struct{}, 4)
        tags := make([]string, 0, 4)
        for _, p := range parts {
                tag := strings.Trim(strings.TrimSpace(p), "\"'`· 。.")
                if tag == "" || utf8.RuneCountInString(tag) > 12 {
                        continue
                }
                if _, dup := seen[tag]; dup {
                        continue
                }
                seen[tag] = struct{}{}
                tags = append(tags, tag)
                if len(tags) >= 4 {
                        break
                }
        }
        return tags
}
