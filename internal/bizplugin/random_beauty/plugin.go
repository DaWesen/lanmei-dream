package random_beauty

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/config"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

const (
	pluginID        = "random_beauty"
	sendSegmentsKey = "bot.send.segments"
)

const (
	messageFailure     = "图难产了，稍等一会再试试吧~(￣ω￣;)"
	messageRateLimited = "请求太频繁啦，请稍后再试"
)

type imageSelector interface {
	Select(ctx context.Context) (*SelectedImage, error)
}

// Plugin 是严格安全审核的 Pixiv 随机美图内置插件。
type Plugin struct {
	cfg      config.RandomBeautyConfig
	selector imageSelector
	logger   *zap.Logger
}

// New 使用 Random Mage 兼容 API 构建随机美图插件。
func New(cfg config.RandomBeautyConfig, moderator ImageModerator, logger *zap.Logger) (*Plugin, error) {
	cfg = normalizedConfig(cfg)
	if logger == nil {
		logger = zap.NewNop()
	}
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	provider, err := newRandomMageClient(cfg.APIBaseURL, client, cfg.MinWidth, cfg.MinHeight, cfg.MinBookmarks, false)
	if err != nil {
		return nil, err
	}
	downloader, err := newImageDownloader(cfg.APIBaseURL, client, cfg.MaxImageBytes, cfg.MinWidth, cfg.MinHeight, false)
	if err != nil {
		return nil, err
	}
	var selected imageSelector
	if moderator != nil {
		selected = newSelector(
			provider,
			downloader,
			moderator,
			cfg.MaxAttempts,
			cfg.SafeConfidence,
			time.Duration(cfg.ModerationTimeoutSeconds)*time.Second,
			logger,
		)
	}
	return newPluginWithSelector(cfg, selected, logger), nil
}

func newPluginWithSelector(cfg config.RandomBeautyConfig, selected imageSelector, logger *zap.Logger) *Plugin {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Plugin{cfg: normalizedConfig(cfg), selector: selected, logger: logger}
}

func normalizedConfig(cfg config.RandomBeautyConfig) config.RandomBeautyConfig {
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		cfg.APIBaseURL = "https://i.mukyu.ru"
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 18 {
		cfg.TimeoutSeconds = 18
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 2
	}
	if cfg.MaxAttempts > 2 {
		cfg.MaxAttempts = 2
	}
	if cfg.CooldownSeconds < 0 {
		cfg.CooldownSeconds = 30
	}
	if cfg.MaxImageBytes <= 0 || cfg.MaxImageBytes > 10*1024*1024 {
		cfg.MaxImageBytes = 10 * 1024 * 1024
	}
	if cfg.MinWidth <= 0 {
		cfg.MinWidth = 720
	}
	if cfg.MinHeight <= 0 {
		cfg.MinHeight = 720
	}
	if cfg.MinBookmarks < 0 {
		cfg.MinBookmarks = 100
	}
	if cfg.SafeConfidence <= 0 || cfg.SafeConfidence > 1 {
		cfg.SafeConfidence = 0.9
	}
	if cfg.ModerationTimeoutSeconds < 1 || cfg.ModerationTimeoutSeconds > 20 {
		cfg.ModerationTimeoutSeconds = 8
	}
	return cfg
}

func (p *Plugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          pluginID,
		Name:        "随机美图",
		Description: "获取经过成人内容与擦边内容安全审核的 Pixiv 随机插画",
		Version:     "1.0.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "随机美图", Description: "发送一张经过严格安全审核的 Pixiv 随机插画；不需要参数", Order: 60},
		},
		SubtreeID: pluginpkg.SubtreeID(pluginID),
	}
}

func (p *Plugin) OnInit(ctx *pluginpkg.PluginContext) error {
	passID := pluginpkg.PassID(pluginID, "fetch")
	pass := &randomBeautyPass{
		selector: p.selector,
		store:    ctx.Store,
		cooldown: time.Duration(p.cfg.CooldownSeconds) * time.Second,
		timeout:  time.Duration(p.cfg.TimeoutSeconds) * time.Second,
		inFlight: make(chan struct{}, 1),
		logger:   p.logger,
	}
	if err := ctx.Engine.RegisterPass(passID, pass); err != nil {
		return fmt.Errorf("register random_beauty pass: %w", err)
	}
	ctx.Registry.TrackPass(pluginID, passID)

	pipelineID := pluginpkg.PipelineID(pluginID, "main")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(pipelineID, passID)); err != nil {
		return fmt.Errorf("register random_beauty pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline(pluginID, pipelineID)

	subtree := conduit.NewSequence(
		conduit.NewCondition(isRandomBeautyCommand),
		conduit.NewAction(pipelineID),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID(pluginID), subtree); err != nil {
		return fmt.Errorf("register random_beauty subtree: %w", err)
	}
	return nil
}

func (p *Plugin) OnStart(_ *pluginpkg.PluginContext) error { return nil }
func (p *Plugin) OnStop(_ *pluginpkg.PluginContext) error  { return nil }

func isRandomBeautyCommand(ctx *conduit.MessageContext) bool {
	return ctx != nil && strings.TrimSpace(ctx.RawMsg) == "/随机美图"
}

type randomBeautyPass struct {
	selector imageSelector
	store    conduit.StateStore
	cooldown time.Duration
	timeout  time.Duration
	inFlight chan struct{}
	logger   *zap.Logger
}

func (pass *randomBeautyPass) Execute(ctx *conduit.MessageContext) error {
	if pass.selector == nil {
		pass.reply(ctx, messageFailure)
		return nil
	}
	if !pass.acquireSlot() {
		pass.reply(ctx, messageFailure)
		return nil
	}
	defer pass.releaseSlot()
	if !pass.acquireCooldown(ctx) {
		pass.reply(ctx, messageRateLimited)
		return nil
	}

	requestCtx := ctx.Ctx
	cancel := func() {}
	if pass.timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx.Ctx, pass.timeout)
	}
	selected, err := pass.selector.Select(requestCtx)
	cancel()
	if err != nil {
		pass.logger.Warn("random_beauty: 获取美图失败", zap.Error(err))
		pass.reply(ctx, messageFailure)
		return nil
	}
	if selected == nil || selected.Candidate == nil || selected.Image == nil || len(selected.Image.Data) == 0 {
		pass.logger.Warn("random_beauty: 选择器返回空结果")
		pass.reply(ctx, messageFailure)
		return nil
	}

	file := "base64://" + base64.StdEncoding.EncodeToString(selected.Image.Data)
	attribution := formatAttribution(selected.Candidate)
	conduit.Set(ctx, sendSegmentsKey, []map[string]any{
		{"type": "image", "data": map[string]any{"file": file}},
		{"type": "text", "data": map[string]any{"text": attribution}},
	})
	return nil
}

func (pass *randomBeautyPass) acquireSlot() bool {
	if pass.inFlight == nil {
		return true
	}
	select {
	case pass.inFlight <- struct{}{}:
		return true
	default:
		return false
	}
}

func (pass *randomBeautyPass) releaseSlot() {
	if pass.inFlight != nil {
		<-pass.inFlight
	}
}

func (pass *randomBeautyPass) acquireCooldown(ctx *conduit.MessageContext) bool {
	if pass.store == nil || pass.cooldown <= 0 {
		return true
	}
	key := pluginpkg.StoreKey(pluginID, cooldownScope(ctx))
	acquired, err := pass.store.SetIfNotExists(ctx.Ctx, key, "1", pass.cooldown)
	if err != nil {
		pass.logger.Warn("random_beauty: 冷却状态写入失败，放行请求", zap.Error(err))
		return true
	}
	return acquired
}

func cooldownScope(ctx *conduit.MessageContext) string {
	platform, _ := ctx.Extra["platform"].(string)
	if platform == "" {
		platform = "unknown"
	}
	scope := "private"
	if ctx.IsGroup {
		scope = "group:" + ctx.GroupID
	}
	return fmt.Sprintf("cooldown:%s:%s:user:%s", platform, scope, ctx.UserID)
}

func formatAttribution(candidate *Candidate) string {
	title := singleLine(candidate.Title, "未命名")
	author := singleLine(candidate.Author, "未知画师")
	return fmt.Sprintf("\n《%s》\n作者：%s\nPixiv：https://www.pixiv.net/artworks/%d", title, author, candidate.IllustID)
}

func singleLine(value, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	return value
}

func (pass *randomBeautyPass) reply(ctx *conduit.MessageContext, content string) {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID: ctx.UserID, GroupID: ctx.GroupID, IsGroup: ctx.IsGroup,
		Content: content,
	})
}
