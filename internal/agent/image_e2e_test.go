package agent

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

// newImageTurnAgent runs a two-round turn: the model reads a PNG, and the
// second round is the request that must carry the image. It returns whatever
// the provider saw on that second round.
func newImageTurnAgent(t *testing.T, model string) provider.Request {
	t.Helper()
	dir := t.TempDir()

	// Incompressible noise, because a smooth gradient encodes far under the
	// 50KB spill threshold and would not exercise the truncation this guards.
	// A real screenshot is photographic and lands well over it.
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	rnd := rand.New(rand.NewSource(1))
	for x := 0; x < 400; x++ {
		for y := 0; y < 400; y++ {
			img.Set(x, y, color.RGBA{R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := session.Open(filepath.Join(dir, "test.jsonl"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	var second provider.Request
	fake := &provider.Fake{Handler: func(ctx context.Context, req provider.Request) ([]provider.Event, error) {
		if IsNamingRequest(req) {
			return []provider.Event{{Kind: provider.EventText, Text: "ok"}, {Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" {
			return []provider.Event{
				{Kind: provider.EventToolCall, ToolCall: provider.ToolCall{ID: "read_1", Name: "read", Args: map[string]any{"path": "shot.png"}}},
				{Kind: provider.EventDone, StopReason: "tool_calls"},
			}, nil
		}
		if last.Role == "tool" {
			second = req
			return []provider.Event{{Kind: provider.EventDone, StopReason: "stop"}}, nil
		}
		return nil, errors.New("unexpected call")
	}}
	ag := New(Options{
		Store:    store,
		Provider: fake,
		Tools:    tools.Default(tools.Deps{Cwd: dir}),
		Cwd:      dir,
		Model:    model,
	})
	if _, err := ag.Send("look at the screenshot", nil); err != nil {
		t.Fatal(err)
	}
	waitSettle(t, ag, ReasonDone)
	if second.Model == "" {
		t.Fatal("the model never saw the tool result; the read call did not happen")
	}
	return second
}

func TestReadImageReachesTheModelThatCanSeeIt(t *testing.T) {
	req := newImageTurnAgent(t, "gpt-5.6-sol")
	last := req.Messages[len(req.Messages)-1]
	if last.Image == nil {
		t.Fatal("provider request carried no image")
	}
	if last.Image.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", last.Image.MimeType)
	}
	if len(last.Image.Data) < 50*1024 {
		t.Errorf("image is %d bytes; the spill path truncated it", len(last.Image.Data))
	}
	if strings.Contains(last.Text, "base64") {
		t.Error("base64 payload reached the model as text")
	}
	if last.Text != imageNote {
		t.Errorf("text = %q, want %q", last.Text, imageNote)
	}
}

func TestReadImageSurrogateReachesTheModelThatCannotSeeIt(t *testing.T) {
	req := newImageTurnAgent(t, "deepseek-v4-pro")
	last := req.Messages[len(req.Messages)-1]
	if last.Image != nil {
		t.Fatal("image attached to a text-only model")
	}
	if !strings.Contains(last.Text, imageSurrogate) {
		t.Errorf("text = %q, want the surrogate", last.Text)
	}
	if strings.Contains(last.Text, "base64") {
		t.Error("base64 payload reached a text-only model as text")
	}
}
