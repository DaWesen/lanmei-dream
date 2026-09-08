package random_beauty

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageDownloaderDownloadsValidatedPNG(t *testing.T) {
	data := testImageBytes(t, "png", 800, 900)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/i/safe.png" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	downloader, err := newImageDownloader(server.URL, server.Client(), int64(len(data)+1), 720, 720, true)
	if err != nil {
		t.Fatalf("newImageDownloader() error = %v", err)
	}
	got, err := downloader.Download(context.Background(), &Candidate{LocalPath: "/i/safe.png", Width: 800, Height: 900})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if got.MIME != "image/png" || got.Width != 800 || got.Height != 900 || !bytes.Equal(got.Data, data) {
		t.Fatalf("Download() = mime=%q size=%dx%d bytes=%d", got.MIME, got.Width, got.Height, len(got.Data))
	}
}

func TestImageDownloaderRejectsUnsafeResponses(t *testing.T) {
	pngData := testImageBytes(t, "png", 800, 800)
	jpegData := testImageBytes(t, "jpeg", 800, 800)

	tests := []struct {
		name        string
		contentType string
		body        []byte
		maxBytes    int64
		candidate   Candidate
	}{
		{name: "too large", contentType: "image/png", body: pngData, maxBytes: int64(len(pngData) - 1), candidate: Candidate{LocalPath: "/image", Width: 800, Height: 800}},
		{name: "html disguised as image", contentType: "image/png", body: []byte("<html>not an image</html>"), maxBytes: 1024, candidate: Candidate{LocalPath: "/image", Width: 800, Height: 800}},
		{name: "image with text content type", contentType: "text/plain", body: jpegData, maxBytes: int64(len(jpegData) + 1), candidate: Candidate{LocalPath: "/image", Width: 800, Height: 800}},
		{name: "unsupported gif", contentType: "image/gif", body: []byte("GIF89a"), maxBytes: 1024, candidate: Candidate{LocalPath: "/image", Width: 800, Height: 800}},
		{name: "below minimum dimensions", contentType: "image/png", body: testImageBytes(t, "png", 300, 300), maxBytes: 1 << 20, candidate: Candidate{LocalPath: "/image", Width: 300, Height: 300}},
		{name: "metadata dimensions disagree", contentType: "image/png", body: pngData, maxBytes: int64(len(pngData) + 1), candidate: Candidate{LocalPath: "/image", Width: 900, Height: 800}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = w.Write(tt.body)
			}))
			defer server.Close()
			downloader, err := newImageDownloader(server.URL, server.Client(), tt.maxBytes, 720, 720, true)
			if err != nil {
				t.Fatalf("newImageDownloader() error = %v", err)
			}
			if _, err := downloader.Download(context.Background(), &tt.candidate); err == nil {
				t.Fatal("Download() expected error")
			}
		})
	}
}

func TestImageDownloaderRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testImageBytes(t, "png", 800, 800))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen.png", http.StatusFound)
	}))
	defer source.Close()

	downloader, err := newImageDownloader(source.URL, source.Client(), 1<<20, 720, 720, true)
	if err != nil {
		t.Fatalf("newImageDownloader() error = %v", err)
	}
	if _, err := downloader.Download(context.Background(), &Candidate{LocalPath: "/image"}); err == nil {
		t.Fatal("Download() expected cross-origin redirect error")
	}
}

func TestImageDownloaderRejectsInvalidLocalPaths(t *testing.T) {
	downloader, err := newImageDownloader("https://example.com", http.DefaultClient, 1<<20, 720, 720, false)
	if err != nil {
		t.Fatalf("newImageDownloader() error = %v", err)
	}
	for _, path := range []string{"https://evil.example/a.png", "//evil.example/a.png", "relative.png"} {
		if _, err := downloader.Download(context.Background(), &Candidate{LocalPath: path}); err == nil {
			t.Errorf("Download(%q) expected error", path)
		}
	}
}

func TestNewImageDownloaderRequiresHTTPS(t *testing.T) {
	if _, err := newImageDownloader("http://example.com", http.DefaultClient, 1<<20, 720, 720, false); err == nil {
		t.Fatal("newImageDownloader() expected HTTPS error")
	}
}

func testImageBytes(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	default:
		t.Fatalf("unknown test image format %q", format)
	}
	if err != nil {
		t.Fatalf("encode test image: %v", err)
	}
	return buf.Bytes()
}
