package random_beauty

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

var (
	ErrNoSafeImage         = errors.New("random_beauty: no safe image")
	ErrProviderUnavailable = errors.New("random_beauty: provider unavailable")
)

// SelectedImage 将可信作品元数据与已审核的同一份图片字节绑定。
type SelectedImage struct {
	Candidate *Candidate
	Image     *DownloadedImage
}

type selector struct {
	provider          CandidateProvider
	downloader        ImageDownloader
	moderator         ImageModerator
	maxAttempts       int
	safeConfidence    float64
	moderationTimeout time.Duration
	logger            *zap.Logger
}

func newSelector(provider CandidateProvider, downloader ImageDownloader, moderator ImageModerator, maxAttempts int, safeConfidence float64, moderationTimeout time.Duration, logger *zap.Logger) *selector {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if maxAttempts > 2 {
		maxAttempts = 2
	}
	if safeConfidence <= 0 || safeConfidence > 1 {
		safeConfidence = 0.9
	}
	if moderationTimeout <= 0 {
		moderationTimeout = 8 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &selector{
		provider:          provider,
		downloader:        downloader,
		moderator:         moderator,
		maxAttempts:       maxAttempts,
		safeConfidence:    safeConfidence,
		moderationTimeout: moderationTimeout,
		logger:            logger,
	}
}

// Select 最多尝试固定次数，只返回明确审核为安全的图片。
func (s *selector) Select(ctx context.Context) (*SelectedImage, error) {
	if s.provider == nil || s.downloader == nil || s.moderator == nil {
		return nil, ErrNoSafeImage
	}
	sawCandidate := false
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate, err := s.provider.Next(ctx)
		if err != nil {
			s.logger.Warn("random_beauty: 获取候选失败", zap.Int("attempt", attempt+1), zap.Error(err))
			continue
		}
		sawCandidate = true
		if !metadataSafe(candidate) {
			s.logger.Debug("random_beauty: 元数据审核拒绝", zap.Int64("pid", candidateID(candidate)))
			continue
		}
		image, err := s.downloader.Download(ctx, candidate)
		if err != nil {
			s.logger.Warn("random_beauty: 图片下载或校验失败", zap.Int64("pid", candidateID(candidate)), zap.Error(err))
			continue
		}

		moderationCtx, cancel := context.WithTimeout(ctx, s.moderationTimeout)
		result, err := s.moderator.ModerateImage(moderationCtx, image.Data, image.MIME)
		cancel()
		if err != nil {
			s.logger.Warn("random_beauty: 图片安全审核失败", zap.Int64("pid", candidateID(candidate)), zap.Error(err))
			continue
		}
		if !result.IsSafe(s.safeConfidence) {
			s.logger.Debug("random_beauty: 图片安全审核拒绝",
				zap.Int64("pid", candidateID(candidate)),
				zap.String("verdict", string(result.Verdict)),
				zap.Float64("confidence", result.Confidence))
			continue
		}
		return &SelectedImage{Candidate: candidate, Image: image}, nil
	}
	if !sawCandidate {
		return nil, ErrProviderUnavailable
	}
	return nil, ErrNoSafeImage
}

func candidateID(candidate *Candidate) int64 {
	if candidate == nil {
		return 0
	}
	return candidate.IllustID
}
