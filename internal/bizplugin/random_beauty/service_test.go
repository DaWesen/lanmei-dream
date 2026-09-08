package random_beauty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/ai"
	"go.uber.org/zap"
)

type fakeCandidateProvider struct {
	candidates []*Candidate
	errs       []error
	calls      int
}

func (f *fakeCandidateProvider) Next(_ context.Context) (*Candidate, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.candidates) {
		return nil, errors.New("no candidate")
	}
	return f.candidates[i], nil
}

type fakeImageDownloader struct {
	image *DownloadedImage
	err   error
	calls int
}

func (f *fakeImageDownloader) Download(_ context.Context, _ *Candidate) (*DownloadedImage, error) {
	f.calls++
	return f.image, f.err
}

type fakeImageModerator struct {
	results  []*ai.ImageSafetyResult
	errs     []error
	calls    int
	seenData [][]byte
}

func (f *fakeImageModerator) ModerateImage(_ context.Context, data []byte, _ string) (*ai.ImageSafetyResult, error) {
	f.seenData = append(f.seenData, append([]byte(nil), data...))
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.results) {
		return nil, errors.New("no moderation result")
	}
	return f.results[i], nil
}

func TestSelectorSkipsUnsafeMetadataBeforeDownload(t *testing.T) {
	provider := &fakeCandidateProvider{candidates: []*Candidate{
		{IllustID: 1, Tags: []string{"R-18"}},
		{IllustID: 2, Title: "风景"},
	}}
	downloader := &fakeImageDownloader{image: &DownloadedImage{Data: []byte("safe"), MIME: "image/png"}}
	moderator := &fakeImageModerator{results: []*ai.ImageSafetyResult{{Verdict: ai.ImageSafetySafe, Confidence: 0.99}}}
	selector := newSelector(provider, downloader, moderator, 2, 0.9, time.Second, zap.NewNop())

	got, err := selector.Select(context.Background())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if got.Candidate.IllustID != 2 || downloader.calls != 1 || moderator.calls != 1 {
		t.Fatalf("got=%+v downloadCalls=%d moderationCalls=%d", got, downloader.calls, moderator.calls)
	}
}

func TestSelectorRetriesEveryNonSafeVerdict(t *testing.T) {
	for _, verdict := range []ai.ImageSafetyVerdict{
		ai.ImageSafetySuggestive,
		ai.ImageSafetyAdult,
		ai.ImageSafetyUncertain,
	} {
		t.Run(string(verdict), func(t *testing.T) {
			provider := &fakeCandidateProvider{candidates: []*Candidate{{IllustID: 1}, {IllustID: 2}}}
			downloader := &fakeImageDownloader{image: &DownloadedImage{Data: []byte("same"), MIME: "image/jpeg"}}
			moderator := &fakeImageModerator{results: []*ai.ImageSafetyResult{
				{Verdict: verdict, Confidence: 0.99},
				{Verdict: ai.ImageSafetySafe, Confidence: 0.95},
			}}
			selector := newSelector(provider, downloader, moderator, 3, 0.9, time.Second, zap.NewNop())

			got, err := selector.Select(context.Background())
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if got.Candidate.IllustID != 2 || moderator.calls != 2 {
				t.Fatalf("got=%+v moderationCalls=%d", got, moderator.calls)
			}
		})
	}
}

func TestSelectorFailsClosedOnModerationErrorsAndLowConfidence(t *testing.T) {
	provider := &fakeCandidateProvider{candidates: []*Candidate{{IllustID: 1}, {IllustID: 2}}}
	downloader := &fakeImageDownloader{image: &DownloadedImage{Data: []byte("image"), MIME: "image/png"}}
	moderator := &fakeImageModerator{
		errs: []error{errors.New("model unavailable")},
		results: []*ai.ImageSafetyResult{
			nil,
			{Verdict: ai.ImageSafetySafe, Confidence: 0.89},
		},
	}
	selector := newSelector(provider, downloader, moderator, 2, 0.9, time.Second, zap.NewNop())

	if _, err := selector.Select(context.Background()); !errors.Is(err, ErrNoSafeImage) {
		t.Fatalf("Select() error = %v, want ErrNoSafeImage", err)
	}
}

func TestSelectorReturnsTheModeratedBytes(t *testing.T) {
	data := []byte("exact image bytes")
	provider := &fakeCandidateProvider{candidates: []*Candidate{{IllustID: 9}}}
	downloader := &fakeImageDownloader{image: &DownloadedImage{Data: data, MIME: "image/png"}}
	moderator := &fakeImageModerator{results: []*ai.ImageSafetyResult{{Verdict: ai.ImageSafetySafe, Confidence: 1}}}
	selector := newSelector(provider, downloader, moderator, 1, 0.9, time.Second, zap.NewNop())

	got, err := selector.Select(context.Background())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if string(got.Image.Data) != string(moderator.seenData[0]) {
		t.Fatalf("sent bytes %q differ from moderated bytes %q", got.Image.Data, moderator.seenData[0])
	}
}

func TestSelectorStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	selector := newSelector(&fakeCandidateProvider{}, &fakeImageDownloader{}, &fakeImageModerator{}, 5, 0.9, time.Second, zap.NewNop())
	if _, err := selector.Select(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select() error = %v, want context.Canceled", err)
	}
}

func TestSelectorReportsProviderUnavailable(t *testing.T) {
	provider := &fakeCandidateProvider{errs: []error{errors.New("down"), errors.New("down")}}
	selector := newSelector(provider, &fakeImageDownloader{}, &fakeImageModerator{}, 2, 0.9, time.Second, zap.NewNop())
	if _, err := selector.Select(context.Background()); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Select() error = %v, want ErrProviderUnavailable", err)
	}
}

func TestSelectorRetriesDownloadFailuresAtMostTwoTimes(t *testing.T) {
	provider := &fakeCandidateProvider{candidates: []*Candidate{
		{IllustID: 1}, {IllustID: 2}, {IllustID: 3}, {IllustID: 4},
	}}
	downloader := &fakeImageDownloader{err: errors.New("image too large")}
	selector := newSelector(provider, downloader, &fakeImageModerator{}, 9, 0.9, time.Second, zap.NewNop())

	if _, err := selector.Select(context.Background()); !errors.Is(err, ErrNoSafeImage) {
		t.Fatalf("Select() error = %v, want ErrNoSafeImage", err)
	}
	if provider.calls != 2 || downloader.calls != 2 {
		t.Fatalf("provider calls=%d downloader calls=%d, want 2", provider.calls, downloader.calls)
	}
}
