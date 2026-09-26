package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeGoSessionHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-opencode-session"); got != "session-123" {
			t.Fatalf("x-opencode-session = %q", got)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	client := NewOpenCodeGo("key", "deepseek-v4-flash")
	client.BaseURL = srv.URL
	client.SetSessionID("session-123")
	stream, err := client.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
}
