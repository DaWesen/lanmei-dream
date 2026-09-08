package random_beauty

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/command"
	"github.com/DaWesen/lanmei-dream/internal/config"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

type fakeSelector struct {
	selected *SelectedImage
	err      error
	calls    int
}

func (f *fakeSelector) Select(_ context.Context) (*SelectedImage, error) {
	f.calls++
	return f.selected, f.err
}

type blockingSelector struct{}

func (*blockingSelector) Select(ctx context.Context) (*SelectedImage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type controlledSelector struct {
	started chan struct{}
	release chan struct{}
}

func (s *controlledSelector) Select(ctx context.Context) (*SelectedImage, error) {
	close(s.started)
	select {
	case <-s.release:
		return nil, ErrNoSafeImage
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRandomBeautyCommandMatchesExactly(t *testing.T) {
	for _, tt := range []struct {
		message string
		want    bool
	}{
		{message: "/随机美图", want: true},
		{message: "  /随机美图  ", want: true},
		{message: "/随机美图xxx", want: false},
		{message: "/随机美图 风景", want: false},
		{message: "给我来一张随机美图", want: false},
	} {
		ctx := testMessageContext(tt.message)
		if got := isRandomBeautyCommand(ctx); got != tt.want {
			t.Errorf("isRandomBeautyCommand(%q) = %v, want %v", tt.message, got, tt.want)
		}
	}
}

func TestRandomBeautyTimeoutStaysBelowConduitBudget(t *testing.T) {
	cfg := normalizedConfig(config.RandomBeautyConfig{TimeoutSeconds: 20})
	if cfg.TimeoutSeconds != 18 {
		t.Fatalf("TimeoutSeconds = %d, want safe maximum 18", cfg.TimeoutSeconds)
	}
}

func TestRandomBeautyPassOutputsModeratedImageAndAttribution(t *testing.T) {
	imageBytes := []byte("moderated image")
	selector := &fakeSelector{selected: &SelectedImage{
		Candidate: &Candidate{IllustID: 123, Title: "晚霞", Author: "画师"},
		Image:     &DownloadedImage{Data: imageBytes, MIME: "image/png"},
	}}
	pass := &randomBeautyPass{selector: selector, cooldown: time.Minute, logger: zap.NewNop()}
	ctx := testMessageContext("/随机美图")

	if err := pass.Execute(ctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	segments, ok := conduit.Get[[]map[string]any](ctx, sendSegmentsKey)
	if !ok || len(segments) != 2 {
		t.Fatalf("segments = %#v", segments)
	}
	imageData := segments[0]["data"].(map[string]any)
	wantFile := "base64://" + base64.StdEncoding.EncodeToString(imageBytes)
	if segments[0]["type"] != "image" || imageData["file"] != wantFile {
		t.Fatalf("image segment = %#v", segments[0])
	}
	textData := segments[1]["data"].(map[string]any)
	wantText := "\n《晚霞》\n作者：画师\nPixiv：https://www.pixiv.net/artworks/123"
	if segments[1]["type"] != "text" || textData["text"] != wantText {
		t.Fatalf("text segment = %#v", segments[1])
	}
}

func TestRandomBeautyPassFailsClosedWithoutModerator(t *testing.T) {
	pass := &randomBeautyPass{logger: zap.NewNop()}
	ctx := testMessageContext("/随机美图")
	if err := pass.Execute(ctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ctx.Output) != 1 || ctx.Output[0].Content != "图难产了，稍等一会再试试吧~(￣ω￣;)" {
		t.Fatalf("output = %#v", ctx.Output)
	}
	if _, ok := conduit.Get[[]map[string]any](ctx, sendSegmentsKey); ok {
		t.Fatal("unsafe path must not set image segments")
	}
}

func TestRandomBeautyPassMapsSelectorErrors(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: ErrNoSafeImage, want: "图难产了，稍等一会再试试吧~(￣ω￣;)"},
		{err: ErrProviderUnavailable, want: "图难产了，稍等一会再试试吧~(￣ω￣;)"},
		{err: errors.New("unexpected"), want: "图难产了，稍等一会再试试吧~(￣ω￣;)"},
	}
	for _, tt := range tests {
		ctx := testMessageContext("/随机美图")
		pass := &randomBeautyPass{selector: &fakeSelector{err: tt.err}, logger: zap.NewNop()}
		if err := pass.Execute(ctx); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if len(ctx.Output) != 1 || ctx.Output[0].Content != tt.want {
			t.Errorf("error %v output = %#v", tt.err, ctx.Output)
		}
	}
}

func TestRandomBeautyPassTimesOutWithFallbackMessage(t *testing.T) {
	pass := &randomBeautyPass{selector: &blockingSelector{}, timeout: 20 * time.Millisecond, logger: zap.NewNop()}
	ctx := testMessageContext("/随机美图")
	started := time.Now()
	if err := pass.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Execute() did not honor plugin timeout")
	}
	if len(ctx.Output) != 1 || ctx.Output[0].Content != "图难产了，稍等一会再试试吧~(￣ω￣;)" {
		t.Fatalf("output = %#v", ctx.Output)
	}
}

func TestRandomBeautyPassRejectsConcurrentRequestsWithoutBlocking(t *testing.T) {
	selector := &controlledSelector{started: make(chan struct{}), release: make(chan struct{})}
	pass := &randomBeautyPass{
		selector: selector,
		timeout:  time.Second,
		inFlight: make(chan struct{}, 1),
		logger:   zap.NewNop(),
	}
	done := make(chan struct{})
	go func() {
		_ = pass.Execute(testMessageContext("/随机美图"))
		close(done)
	}()
	<-selector.started

	second := testMessageContext("/随机美图")
	started := time.Now()
	if err := pass.Execute(second); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("concurrent request should be rejected immediately")
	}
	if len(second.Output) != 1 || second.Output[0].Content != "图难产了，稍等一会再试试吧~(￣ω￣;)" {
		t.Fatalf("output = %#v", second.Output)
	}
	close(selector.release)
	<-done
}

func TestRandomBeautyPassAppliesCooldown(t *testing.T) {
	store := newTestStateStore()
	selector := &fakeSelector{selected: &SelectedImage{
		Candidate: &Candidate{IllustID: 1, Title: "图", Author: "作者"},
		Image:     &DownloadedImage{Data: []byte("image"), MIME: "image/png"},
	}}
	pass := &randomBeautyPass{selector: selector, store: store, cooldown: time.Minute, logger: zap.NewNop()}

	first := testMessageContext("/随机美图")
	if err := pass.Execute(first); err != nil {
		t.Fatal(err)
	}
	second := testMessageContext("/随机美图")
	if err := pass.Execute(second); err != nil {
		t.Fatal(err)
	}
	if selector.calls != 1 {
		t.Fatalf("selector calls = %d, want 1", selector.calls)
	}
	if len(second.Output) != 1 || second.Output[0].Content != "请求太频繁啦，请稍后再试" {
		t.Fatalf("second output = %#v", second.Output)
	}
}

func TestRandomBeautyPluginRegistersTrackedResources(t *testing.T) {
	store := newTestStateStore()
	engine := conduit.New(store)
	registry := pluginpkg.NewRegistry(engine, store, nil, command.New(), nil, zap.NewNop())
	plugin := newPluginWithSelector(config.RandomBeautyConfig{}, &fakeSelector{}, zap.NewNop())
	if err := registry.Register(plugin); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.InitPlugins(context.Background()); err != nil {
		t.Fatalf("InitPlugins() error = %v", err)
	}
	if _, ok := engine.GetPass(pluginpkg.PassID(pluginID, "fetch")); !ok {
		t.Fatal("plugin pass was not registered")
	}
	if _, ok := engine.GetPipeline(pluginpkg.PipelineID(pluginID, "main")); !ok {
		t.Fatal("plugin pipeline was not registered")
	}
	if _, ok := engine.GetSubtree(pluginpkg.SubtreeID(pluginID)); !ok {
		t.Fatal("plugin subtree was not registered")
	}
	if err := registry.Unregister(pluginID); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if _, ok := engine.GetPass(pluginpkg.PassID(pluginID, "fetch")); ok {
		t.Fatal("tracked pass leaked after unregister")
	}
}

func testMessageContext(content string) *conduit.MessageContext {
	return conduit.NewMessageContext(&conduit.InputMessage{
		UserID:  "u1",
		GroupID: "g1",
		IsGroup: true,
		Content: content,
		Extra: map[string]any{
			"platform": "qq",
		},
	})
}

type testStateStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newTestStateStore() *testStateStore { return &testStateStore{data: make(map[string]string)} }

func (s *testStateStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}
func (s *testStateStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}
func (s *testStateStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}
func (s *testStateStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok, nil
}
func (s *testStateStore) CompareAndSwap(_ context.Context, key, oldValue, newValue string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[key] != oldValue {
		return false, nil
	}
	s.data[key] = newValue
	return true, nil
}
func (s *testStateStore) IncrBy(_ context.Context, key string, delta int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value int64
	_, _ = fmt.Sscan(s.data[key], &value)
	value += delta
	s.data[key] = fmt.Sprint(value)
	return value, nil
}
func (s *testStateStore) SetIfNotExists(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		return false, nil
	}
	s.data[key] = value
	return true, nil
}
func (s *testStateStore) Close() error { return nil }
