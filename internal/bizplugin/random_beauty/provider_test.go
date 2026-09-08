package random_beauty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRandomMageClientNextUsesSafetyFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		want := map[string]string{
			"format":        "json",
			"strategy":      "random",
			"r18":           "0",
			"r18_strict":    "1",
			"ai_type":       "0",
			"min_width":     "720",
			"min_height":    "800",
			"min_bookmarks": "100",
		}
		for key, value := range want {
			if got := query.Get(key); got != value {
				t.Errorf("query %s = %q, want %q", key, got, value)
			}
		}
		if len(query["excluded_tags"]) == 0 {
			t.Error("excluded_tags must not be empty")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"code": "OK",
			"data": {
				"image": {
					"illust_id": "12345",
					"x_restrict": 0,
					"ai_type": 0,
					"title": "风景",
					"user": {"id":"9","name":"画师"}
				},
				"tags": ["風景", "青空"],
				"urls": {"local":"/i/7.jpg", "proxy":"https://evil.example/image.jpg"}
			}
		}`))
	}))
	defer server.Close()

	client, err := newRandomMageClient(server.URL, server.Client(), 720, 800, 100, true)
	if err != nil {
		t.Fatalf("newRandomMageClient() error = %v", err)
	}
	candidate, err := client.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if candidate.IllustID != 12345 || candidate.LocalPath != "/i/7.jpg" {
		t.Fatalf("Next() = %+v", candidate)
	}
	if candidate.Title != "风景" || candidate.Author != "画师" || len(candidate.Tags) != 2 {
		t.Fatalf("Next() metadata = %+v", candidate)
	}
}

func TestRandomMageClientNextRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "non 2xx", status: http.StatusBadGateway, body: `{}`},
		{name: "invalid json", status: http.StatusOK, body: `{`},
		{name: "api failure", status: http.StatusOK, body: `{"ok":false,"code":"FAILED"}`},
		{name: "missing image", status: http.StatusOK, body: `{"ok":true,"data":{}}`},
		{name: "missing r18 classification", status: http.StatusOK, body: `{"ok":true,"data":{"image":{"illust_id":"123","ai_type":0,"title":"safe","user":{"name":"artist"}},"tags":["safe"],"urls":{"local":"/i/7.jpg"}}}`},
		{name: "missing ai classification", status: http.StatusOK, body: `{"ok":true,"data":{"image":{"illust_id":"123","x_restrict":0,"title":"safe","user":{"name":"artist"}},"tags":["safe"],"urls":{"local":"/i/7.jpg"}}}`},
		{name: "adult", status: http.StatusOK, body: validResponse(`"x_restrict":1`)},
		{name: "ai generated", status: http.StatusOK, body: validResponse(`"ai_type":1`)},
		{name: "zero id", status: http.StatusOK, body: validResponse(`"illust_id":"0"`)},
		{name: "absolute local path", status: http.StatusOK, body: validResponse(`"local":"https://evil.example/a.jpg"`)},
		{name: "protocol relative local path", status: http.StatusOK, body: validResponse(`"local":"//evil.example/a.jpg"`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client, err := newRandomMageClient(server.URL, server.Client(), 720, 720, 100, true)
			if err != nil {
				t.Fatalf("newRandomMageClient() error = %v", err)
			}
			if _, err := client.Next(context.Background()); err == nil {
				t.Fatal("Next() expected error")
			}
		})
	}
}

func TestNewRandomMageClientRequiresHTTPS(t *testing.T) {
	if _, err := newRandomMageClient("http://example.com", http.DefaultClient, 720, 720, 100, false); err == nil {
		t.Fatal("newRandomMageClient() expected HTTPS validation error")
	}
}

func TestRandomMageClientRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validResponse("")))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/random", http.StatusFound)
	}))
	defer source.Close()

	client, err := newRandomMageClient(source.URL, source.Client(), 720, 720, 100, true)
	if err != nil {
		t.Fatalf("newRandomMageClient() error = %v", err)
	}
	if _, err := client.Next(context.Background()); err == nil {
		t.Fatal("Next() expected cross-origin redirect error")
	}
}

func TestRandomMageClientLeavesUnsafeMetadataForSelector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"image":{"illust_id":"123","x_restrict":0,"ai_type":0,"title":"safe","user":{"name":"artist"}},"tags":["R-18"],"urls":{"local":"/i/7.jpg"}}}`))
	}))
	defer server.Close()
	client, err := newRandomMageClient(server.URL, server.Client(), 720, 720, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := client.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() should leave content policy to selector: %v", err)
	}
	if metadataSafe(candidate) {
		t.Fatal("test candidate should remain unsafe for selector rejection")
	}
}

func validResponse(replacement string) string {
	fields := `"illust_id":"123","x_restrict":0,"ai_type":0,"title":"safe","user":{"name":"artist"}`
	switch {
	case replacement == `"x_restrict":1`:
		fields = `"illust_id":"123","x_restrict":1,"ai_type":0,"title":"safe","user":{"name":"artist"}`
	case replacement == `"ai_type":1`:
		fields = `"illust_id":"123","x_restrict":0,"ai_type":1,"title":"safe","user":{"name":"artist"}`
	case replacement == `"illust_id":"0"`:
		fields = `"illust_id":"0","x_restrict":0,"ai_type":0,"title":"safe","user":{"name":"artist"}`
	}
	local := `"local":"/i/7.jpg"`
	if replacement == `"local":"https://evil.example/a.jpg"` || replacement == `"local":"//evil.example/a.jpg"` {
		local = replacement
	}
	return `{"ok":true,"data":{"image":{` + fields + `},"tags":["safe"],"urls":{` + local + `}}}`
}
