package random_beauty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxProviderResponseBytes = 1 << 20

// Candidate 是经过上游字段校验的候选作品。
type Candidate struct {
	IllustID  int64
	Title     string
	Author    string
	Tags      []string
	LocalPath string
	XRestrict int
	AIType    int
	Width     int
	Height    int
}

// CandidateProvider 提供尚未经过本地内容审核的候选作品。
type CandidateProvider interface {
	Next(ctx context.Context) (*Candidate, error)
}

// RandomMageClient 调用 Random Mage 兼容接口获取候选作品。
type RandomMageClient struct {
	baseURL      *url.URL
	client       *http.Client
	minWidth     int
	minHeight    int
	minBookmarks int
}

func newRandomMageClient(baseURL string, client *http.Client, minWidth, minHeight, minBookmarks int, allowInsecureForTest bool) (*RandomMageClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 解析 API 地址: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && !allowInsecureForTest) {
		return nil, errors.New("random_beauty: API 地址必须是无凭据的 HTTPS 地址")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("random_beauty: API 地址协议无效")
	}
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("random_beauty: 候选接口重定向次数过多")
		}
		if !sameOrigin(parsed, req.URL) {
			return errors.New("random_beauty: 拒绝候选接口跨源重定向")
		}
		return nil
	}
	return &RandomMageClient{
		baseURL:      parsed,
		client:       &clientCopy,
		minWidth:     minWidth,
		minHeight:    minHeight,
		minBookmarks: minBookmarks,
	}, nil
}

type randomMageResponse struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
	Data struct {
		Image *struct {
			IllustID  json.Number `json:"illust_id"`
			Title     string      `json:"title"`
			XRestrict *int        `json:"x_restrict"`
			AIType    *int        `json:"ai_type"`
			Width     int         `json:"width"`
			Height    int         `json:"height"`
			User      struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"image"`
		Tags []string `json:"tags"`
		URLs struct {
			Local string `json:"local"`
		} `json:"urls"`
	} `json:"data"`
}

// Next 请求一张固定为全年龄、严格分级且非 AI 的候选图。
func (c *RandomMageClient) Next(ctx context.Context) (*Candidate, error) {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: "/random"})
	query := endpoint.Query()
	query.Set("format", "json")
	query.Set("strategy", "random")
	query.Set("r18", "0")
	query.Set("r18_strict", "1")
	query.Set("ai_type", "0")
	query.Set("min_width", strconv.Itoa(c.minWidth))
	query.Set("min_height", strconv.Itoa(c.minHeight))
	query.Set("min_bookmarks", strconv.Itoa(c.minBookmarks))
	for _, tag := range excludedTags() {
		query.Add("excluded_tags", tag)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 创建候选请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 请求候选: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("random_beauty: 候选接口状态码 %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 读取候选响应: %w", err)
	}
	if len(body) > maxProviderResponseBytes {
		return nil, errors.New("random_beauty: 候选响应过大")
	}
	var payload randomMageResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("random_beauty: 解析候选响应: %w", err)
	}
	if !payload.OK || payload.Data.Image == nil {
		return nil, fmt.Errorf("random_beauty: 候选接口失败 code=%s", payload.Code)
	}

	image := payload.Data.Image
	illustID, err := image.IllustID.Int64()
	if err != nil || illustID <= 0 {
		return nil, errors.New("random_beauty: 候选作品 ID 无效")
	}
	if image.XRestrict == nil || image.AIType == nil || *image.XRestrict != 0 || *image.AIType != 0 {
		return nil, errors.New("random_beauty: 候选分级不符合要求")
	}
	if err := validateLocalPath(payload.Data.URLs.Local); err != nil {
		return nil, err
	}

	candidate := &Candidate{
		IllustID:  illustID,
		Title:     strings.TrimSpace(image.Title),
		Author:    strings.TrimSpace(image.User.Name),
		Tags:      append([]string(nil), payload.Data.Tags...),
		LocalPath: payload.Data.URLs.Local,
		XRestrict: *image.XRestrict,
		AIType:    *image.AIType,
		Width:     image.Width,
		Height:    image.Height,
	}
	if !metadataWithinLimits(candidate) {
		return nil, errors.New("random_beauty: 候选元数据超出限制")
	}
	return candidate, nil
}

func validateLocalPath(localPath string) error {
	if !strings.HasPrefix(localPath, "/") || strings.HasPrefix(localPath, "//") {
		return errors.New("random_beauty: 候选本地路径无效")
	}
	parsed, err := url.Parse(localPath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil {
		return errors.New("random_beauty: 候选本地路径无效")
	}
	return nil
}
