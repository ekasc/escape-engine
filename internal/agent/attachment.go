package agent

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekasc/escape-engine/internal/session"
)

// No size cap. A cap was here and was removed deliberately: a file the model
// cannot use should fail at the model, with the provider's own reason, rather
// than be refused here by a number the person dropping it cannot see or
// predict. The cost is on disk — an attachment is base64'd into the session
// file, so the bytes are kept at about 4/3 of their size for as long as the
// session lives — and in the request, which carries the same base64 inline.

// readAttachment turns a dropped path into the block that carries it.
//
// The path is read here, not in the shell, for the same reason the cap is: the
// shell has no idea what the model's context window is, and a file that is
// missing, unreadable, a directory, or too large is an engine-side question.
//
// Mime type comes from the extension, with a sniff as the fallback. It is
// recorded verbatim and deliberately not checked against what the model accepts:
// the decision is to attach and let the request fail, so that a file the model
// cannot read produces a refusal from the provider naming the type, rather than
// a guess made here that quietly drops it.
func readAttachment(path string) (*session.Block, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot attach %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("cannot attach %s: it is a folder", filepath.Base(path))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot attach %s: %w", filepath.Base(path), err)
	}

	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(raw)
	}
	// A type the platform could not name is worse than a generic one: a
	// "application/octet-stream" is rejected with a legible reason, whereas an
	// empty string is rejected with a puzzling one.
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	blockType := session.BlockMedia
	if strings.HasPrefix(mimeType, "image/") {
		blockType = session.BlockImage
	}
	return &session.Block{
		Type:     blockType,
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(raw),
		Source:   path,
	}, nil
}

// attachmentBlocks reads every path, collecting the ones that work. A bad file
// is skipped with a note rather than failing the turn: dropping four screenshots
// and having one of them be a symlink to nothing should still send the other
// three.
func attachmentBlocks(paths []string) ([]session.Block, []string) {
	var (
		blocks []session.Block
		notes  []string
	)
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		block, err := readAttachment(path)
		if err != nil {
			notes = append(notes, err.Error())
			continue
		}
		blocks = append(blocks, *block)
	}
	return blocks, notes
}

// eventAttachments picks the attachment blocks out of a persisted message so the
// message_end that announces it can carry the same bytes. The shell has the
// paths already — it is the one that dropped them — but not the payload, and
// having it read the file back would mean two sides of the wire both deciding
// what a file is and how big it may be.
func eventAttachments(blocks []session.Block) []Attachment {
	var out []Attachment
	for _, block := range blocks {
		if block.Type != session.BlockImage && block.Type != session.BlockMedia {
			continue
		}
		out = append(out, Attachment{
			Kind:     block.Type,
			MimeType: block.MimeType,
			Data:     block.Data,
			Source:   block.Source,
		})
	}
	return out
}
