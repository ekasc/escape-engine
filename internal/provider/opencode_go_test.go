package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenCodeGoUsesChatCompletions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer key" {
			t.Fatalf("request = %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewOpenCodeGo("key", "deepseek-v4-flash")
	c.BaseURL = srv.URL
	s, err := c.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ev, err := s.Next()
	if err != nil || ev.Text != "ok" {
		t.Fatalf("event=%+v err=%v", ev, err)
	}
}

func TestOpenCodeGoUsesResponsesForGrok(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":{\"text\":\"ok\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer srv.Close()
	c := NewOpenCodeGo("key", "grok-4.6")
	c.BaseURL = srv.URL
	s, err := c.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var text strings.Builder
	for {
		ev, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "ok" {
		t.Fatalf("text = %q", text.String())
	}
}
