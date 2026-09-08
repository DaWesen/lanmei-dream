package random_beauty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxImageDimension = 12000
	maxImagePixels    = 50_000_000
	maxRedirects      = 5
)

// DownloadedImage 保存将被审核并原样发送的图片字节。
type DownloadedImage struct {
	Data   []byte
	MIME   string
	Width  int
	Height int
}

// ImageDownloader 下载并验证候选图片。
type ImageDownloader interface {
	Download(ctx context.Context, candidate *Candidate) (*DownloadedImage, error)
}

type sameOriginDownloader struct {
	baseURL   *url.URL
	client    *http.Client
	maxBytes  int64
	minWidth  int
	minHeight int
}

func newImageDownloader(baseURL string, client *http.Client, maxBytes int64, minWidth, minHeight int, allowInsecureForTest bool) (*sameOriginDownloader, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 解析图片源地址: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && !allowInsecureForTest) {
		return nil, errors.New("random_beauty: 图片源必须是无凭据的 HTTPS 地址")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("random_beauty: 图片源协议无效")
	}
	if maxBytes <= 0 {
		return nil, errors.New("random_beauty: 图片大小限制无效")
	}
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("random_beauty: 图片重定向次数过多")
		}
		if !sameOrigin(parsed, req.URL) {
			return errors.New("random_beauty: 拒绝跨源图片重定向")
		}
		return nil
	}
	return &sameOriginDownloader{
		baseURL:   parsed,
		client:    &clientCopy,
		maxBytes:  maxBytes,
		minWidth:  minWidth,
		minHeight: minHeight,
	}, nil
}

func (d *sameOriginDownloader) Download(ctx context.Context, candidate *Candidate) (*DownloadedImage, error) {
	if candidate == nil {
		return nil, errors.New("random_beauty: 候选为空")
	}
	if err := validateLocalPath(candidate.LocalPath); err != nil {
		return nil, err
	}
	ref, err := url.Parse(candidate.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 解析图片路径: %w", err)
	}
	endpoint := d.baseURL.ResolveReference(ref)
	if !sameOrigin(d.baseURL, endpoint) || endpoint.User != nil {
		return nil, errors.New("random_beauty: 图片路径跨源")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 创建图片请求: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 下载图片: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("random_beauty: 图片接口状态码 %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, d.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 读取图片: %w", err)
	}
	if len(data) == 0 || int64(len(data)) > d.maxBytes {
		return nil, errors.New("random_beauty: 图片为空或超过大小限制")
	}

	headerMIME, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !supportedImageMIME(headerMIME) {
		return nil, fmt.Errorf("random_beauty: 图片响应 MIME 无效 %q", headerMIME)
	}
	sniffedMIME := http.DetectContentType(data)
	if !supportedImageMIME(sniffedMIME) || headerMIME != sniffedMIME {
		return nil, fmt.Errorf("random_beauty: 图片实际 MIME %q 与响应 %q 不符", sniffedMIME, headerMIME)
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("random_beauty: 图片解码失败: %w", err)
	}
	decodedMIME := "image/" + format
	if format == "jpg" {
		decodedMIME = "image/jpeg"
	}
	if decodedMIME != sniffedMIME {
		return nil, errors.New("random_beauty: 图片解码格式不一致")
	}
	if config.Width < d.minWidth || config.Height < d.minHeight {
		return nil, fmt.Errorf("random_beauty: 图片尺寸过小 %dx%d", config.Width, config.Height)
	}
	if config.Width > maxImageDimension || config.Height > maxImageDimension || int64(config.Width)*int64(config.Height) > maxImagePixels {
		return nil, fmt.Errorf("random_beauty: 图片尺寸过大 %dx%d", config.Width, config.Height)
	}
	if candidate.Width > 0 && candidate.Width != config.Width || candidate.Height > 0 && candidate.Height != config.Height {
		return nil, errors.New("random_beauty: 图片实际尺寸与候选元数据不一致")
	}

	return &DownloadedImage{Data: data, MIME: sniffedMIME, Width: config.Width, Height: config.Height}, nil
}

func supportedImageMIME(value string) bool {
	return value == "image/jpeg" || value == "image/png"
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
