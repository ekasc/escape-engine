package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testImages are minimal byte sequences carrying the magic bytes of each
// supported format (not fully valid images; detection only sniffs headers).
var testImages = []struct {
	name string
	mime string
	data []byte
}{
	{"png", "image/png", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02}},
	{"jpeg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}},
	{"gif", "image/gif", []byte{'G', 'I', 'F', '8', '9', 'a', 0x01}},
	{"webp", "image/webp", []byte{'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'}},
}

func TestSniffImage(t *testing.T) {
	for _, tc := range testImages {
		mime, ok := sniffImage(tc.data)
		if !ok || mime != tc.mime {
			t.Errorf("%s: mime=%q ok=%v, want %q", tc.name, mime, ok, tc.mime)
		}
	}
	if mime, ok := sniffImage([]byte("plain text")); ok {
		t.Errorf("text sniffed as image: %q", mime)
	}
	if mime, ok := sniffImage(nil); ok {
		t.Errorf("empty sniffed as image: %q", mime)
	}
}

func TestImageMIMEByMagicBytes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range testImages {
		// Wrong extension on purpose: magic bytes must win.
		path := filepath.Join(dir, tc.name+".bin")
		if err := os.WriteFile(path, tc.data, 0o644); err != nil {
			t.Fatal(err)
		}
		mime, ok := imageMIME(path)
		if !ok || mime != tc.mime {
			t.Errorf("%s: mime=%q ok=%v, want %q", tc.name, mime, ok, tc.mime)
		}
	}
}

func TestImageMIMEExtensionFallback(t *testing.T) {
	dir := t.TempDir()
	// Text content with an image extension: magic bytes fail, extension wins.
	path := filepath.Join(dir, "notreally.png")
	if err := os.WriteFile(path, []byte("not an image\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mime, ok := imageMIME(path)
	if !ok || mime != "image/png" {
		t.Fatalf("mime=%q ok=%v, want image/png", mime, ok)
	}
}

func TestImageMIMENonImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := imageMIME(path); ok {
		t.Fatal("text file detected as image")
	}
	if _, ok := imageMIME(filepath.Join(dir, "missing.png")); ok {
		t.Fatal("missing file detected as image")
	}
}

func TestReadImageReturnsDataURI(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02}
	if err := os.WriteFile(filepath.Join(dir, "pic.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := Read(dir)
	res := tool.Run(context.Background(), map[string]any{"path": "pic.png"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	prefix := ImageOutputPrefix + "data:image/png;base64,"
	if !strings.HasPrefix(res.Output, prefix) {
		t.Fatalf("output = %q", res.Output)
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(res.Output, prefix))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded = %v, want %v", got, payload)
	}
}
