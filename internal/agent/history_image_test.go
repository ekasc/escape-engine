package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/tools"
)

// fixturePNG returns a real PNG and its data URI, so the tests exercise the same
// bytes a screenshot would carry rather than a hand-written stand-in.
func fixturePNG(t *testing.T) ([]byte, string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	return data, (&provider.Image{MimeType: "image/png", Data: data}).DataURI()
}

func appendImageResult(t *testing.T, store *session.Store, dataURI string) {
	t.Helper()
	if _, err := store.Append(session.Entry{
		Type: session.TypeMessage,
		Message: &session.Message{
			Role:    session.RoleToolResult,
			Content: []session.Block{{Type: "text", Text: tools.ImageOutputPrefix + dataURI}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadImageAttachesWhenModelSeesImages(t *testing.T) {
	store, path := newCacheFixture(t)
	_, uri := fixturePNG(t)
	appendImageResult(t, store, uri)

	msgs, err := messagesFromEntries(mustReadAll(t, path), "sys", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	got := msgs[len(msgs)-1]
	if got.Image == nil {
		t.Fatal("image not attached for a model that accepts image input")
	}
	if got.Text != imageNote {
		t.Errorf("text = %q, want %q", got.Text, imageNote)
	}
	if strings.Contains(got.Text, "base64") {
		t.Error("base64 payload leaked into the message text")
	}
}

func TestReadImageSurrogateWhenModelIsTextOnly(t *testing.T) {
	store, path := newCacheFixture(t)
	_, uri := fixturePNG(t)
	appendImageResult(t, store, uri)

	msgs, err := messagesFromEntries(mustReadAll(t, path), "sys", "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	got := msgs[len(msgs)-1]
	if got.Image != nil {
		t.Error("image attached to a text-only model")
	}
	if !strings.Contains(got.Text, imageSurrogate) {
		t.Errorf("text = %q, want it to carry the surrogate %q", got.Text, imageSurrogate)
	}
	if strings.Contains(got.Text, "base64") {
		t.Error("base64 payload leaked into a text-only model's message")
	}
}

// A marker with a payload that does not decode must not be reported as an
// image, and must not be dropped either.
func TestReadImageRejectsCorruptPayload(t *testing.T) {
	store, path := newCacheFixture(t)
	appendImageResult(t, store, "data:image/png;base64,not-valid-base64!!")

	msgs, err := messagesFromEntries(mustReadAll(t, path), "sys", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	got := msgs[len(msgs)-1]
	if got.Image != nil {
		t.Error("attached an image that did not decode")
	}
	if got.Text == "" {
		t.Error("dropped the tool result")
	}
}

func TestReadImageLeavesOrdinaryResultsAlone(t *testing.T) {
	store, path := newCacheFixture(t)
	if _, err := store.Append(session.Entry{
		Type: session.TypeMessage,
		Message: &session.Message{
			Role:    session.RoleToolResult,
			Content: []session.Block{{Type: "text", Text: "1: package main"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	msgs, err := messagesFromEntries(mustReadAll(t, path), "sys", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	got := msgs[len(msgs)-1]
	if got.Image != nil || got.Text != "1: package main" {
		t.Errorf("ordinary result altered: image=%v text=%q", got.Image, got.Text)
	}
}

// The snapcompact archive is a screenshot like any other, so it obeys the same
// gate instead of being attached unconditionally.
func TestSnapcompactArchiveObeysTheGate(t *testing.T) {
	store, path := newCacheFixture(t)
	appendUser(store, "hello")
	kept := mustReadAll(t, path)[0].ID

	archive := `{"version":1,"frames":[` + quotedPNGFrame(t) + `],"textFallback":"USER: hello"}`
	if _, err := store.Append(session.Entry{
		Type:             session.TypeCompaction,
		Summary:          "earlier work",
		FirstKeptEntryID: kept,
		SnapcompactData:  archive,
	}); err != nil {
		t.Fatal(err)
	}
	entries := mustReadAll(t, path)

	seeing, err := messagesFromEntries(entries, "sys", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if !hasImages(seeing) {
		t.Error("archive not attached for a model that accepts image input")
	}

	blind, err := messagesFromEntries(entries, "sys", "glm-5")
	if err != nil {
		t.Fatal(err)
	}
	if hasImages(blind) {
		t.Error("archive attached to a text-only model")
	}
	if !strings.Contains(blind[1].Text, imageSurrogate) {
		t.Errorf("text = %q, want the surrogate", blind[1].Text)
	}
}

func quotedPNGFrame(t *testing.T) string {
	t.Helper()
	data, _ := fixturePNG(t)
	return `"` + base64.StdEncoding.EncodeToString(data) + `"`
}

func hasImages(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Image != nil || len(m.Images) > 0 {
			return true
		}
	}
	return false
}
