package random_beauty

import (
	"context"

	"github.com/DaWesen/lanmei-dream/internal/ai"
)

// ImageModerator 是插件依赖的最小视觉审核接口，便于失败关闭测试。
type ImageModerator interface {
	ModerateImage(ctx context.Context, data []byte, mime string) (*ai.ImageSafetyResult, error)
}
