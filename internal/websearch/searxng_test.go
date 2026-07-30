package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearch_ParsesAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format=json expected, got %q", r.URL.Query().Get("format"))
		}
		if q := r.URL.Query().Get("q"); q != "yii3 release" {
			t.Errorf("unexpected query %q", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Yii3", "url": "https://yiiframework.com", "content": "Yii3 released"},
				{"title": "No URL", "url": "", "content": "skip me"},
				{"title": "Reddit", "url": "https://reddit.com/r/yii", "content": "discussion"},
				{"title": "Github", "url": "https://github.com/yiisoft/yii3", "content": "source"},
			},
		})
	}))
	defer srv.Close()

	s := New(srv.URL, 2)
	got, err := s.Search(context.Background(), "yii3 release")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results (maxResults + skip empty url), got %d", len(got))
	}
	if got[0].URL != "https://yiiframework.com" {
		t.Errorf("first url = %q", got[0].URL)
	}
	if got[1].URL != "https://reddit.com/r/yii" {
		t.Errorf("second url = %q", got[1].URL)
	}
}

func TestSearch_EmptyBaseURLErrors(t *testing.T) {
	s := New("", 5)
	if _, err := s.Search(context.Background(), "q"); err == nil {
		t.Fatal("expected error for empty base url")
	}
}

func TestSearch_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s := New(srv.URL, 5)
	if _, err := s.Search(context.Background(), "q"); err == nil ||
		!strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("expected HTTP 502 error, got %v", err)
	}
}

func TestFormatResults(t *testing.T) {
	out := FormatResults([]Result{
		{Title: "Yii3", URL: "https://yiiframework.com", Content: "released"},
		{Title: "NoSnippet", URL: "https://x.test", Content: ""},
	})
	if !strings.Contains(out, "1. Yii3") || !strings.Contains(out, "https://yiiframework.com") {
		t.Errorf("unexpected format:\n%s", out)
	}
	if !strings.Contains(out, "2. NoSnippet") {
		t.Errorf("title-only result missing:\n%s", out)
	}
}
