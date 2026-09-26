package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeZenUsesChatCompletions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization = %q, want empty for free access", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewOpenCodeZen("", "opencode/space-bunny-free")
	client.BaseURL = srv.URL
	stream, err := client.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	event, err := stream.Next()
	if err != nil || event.Text != "ok" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}
