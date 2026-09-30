package agent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekasc/escape-engine/internal/session"
)

func TestReadAttachmentKeepsTypeAndPath(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}

	block, err := readAttachment(png)
	if err != nil {
		t.Fatalf("readAttachment: %v", err)
	}
	// Images keep the block type they always had, so a session file written
	// before attachments existed reads back the same way.
	if block.Type != session.BlockImage {
		t.Errorf("Type = %q, want %q", block.Type, session.BlockImage)
	}
	if !strings.HasPrefix(block.MimeType, "image/") {
		t.Errorf("MimeType = %q, want an image/*", block.MimeType)
	}
	if block.Source != png {
		t.Errorf("Source = %q, want %q", block.Source, png)
	}
	if block.Data == "" {
		t.Error("Data is empty; the bytes are what the provider renders")
	}
}

func TestReadAttachmentAcceptsNonImageMedia(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7"), 0o644); err != nil {
		t.Fatal(err)
	}

	block, err := readAttachment(pdf)
	if err != nil {
		t.Fatalf("readAttachment: %v", err)
	}
	// The decision is to attach and let the request fail, so a PDF is a media
	// block carrying its real type, not a rejected drop.
	if block.Type != session.BlockMedia {
		t.Errorf("Type = %q, want %q", block.Type, session.BlockMedia)
	}
	if !strings.Contains(block.MimeType, "pdf") {
		t.Errorf("MimeType = %q, want a pdf type", block.MimeType)
	}
}

func TestReadAttachmentRefusesWhatCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a-folder")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path, want string }{
		{"missing", filepath.Join(dir, "nope.png"), "cannot attach"},
		{"folder", sub, "it is a folder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readAttachment(c.path)
			if err == nil {
				t.Fatalf("readAttachment(%q) accepted it", c.path)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
			// The name is in the message so a user who dropped four files knows
			// which one was refused.
			if !strings.Contains(err.Error(), filepath.Base(c.path)) {
				t.Errorf("error = %q, want it to name the file", err)
			}
		})
	}
}

func TestReadAttachmentHasNoSizeCap(t *testing.T) {
	// The 8 MiB cap was removed deliberately, so this is here to make putting it
	// back a visible change rather than a quiet one. What replaces it as the
	// limit is the model's: the file goes out base64'd in the request and is
	// refused there, with a reason that names the file's problem, rather than
	// refused here by a number nobody can see or predict.
	dir := t.TempDir()
	big := filepath.Join(dir, "big.png")
	// Comfortably past the cap that used to be 8 MiB.
	raw := make([]byte, 12<<20)
	copy(raw, []byte("\x89PNG\r\n\x1a\n"))
	if err := os.WriteFile(big, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	block, err := readAttachment(big)
	if err != nil {
		t.Fatalf("readAttachment refused a 12 MiB file: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(block.Data)
	if err != nil {
		t.Fatalf("stored payload is not base64: %v", err)
	}
	if len(decoded) != len(raw) {
		t.Errorf("stored %d bytes of %d", len(decoded), len(raw))
	}
	if block.MimeType != "image/png" || block.Type != session.BlockImage {
		t.Errorf("got %q/%q, want image/png", block.Type, block.MimeType)
	}
}

func TestAttachmentBlocksKeepsTheGoodOnesAndNamesTheRest(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(good, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blocks, notes := attachmentBlocks([]string{good, filepath.Join(dir, "gone.png"), "  "})
	if len(blocks) != 1 {
		t.Fatalf("kept %d attachments, want 1", len(blocks))
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1: %v", len(notes), notes)
	}
	if !strings.Contains(notes[0], "gone.png") {
		t.Errorf("note = %q, want it to name the dropped file", notes[0])
	}
}
