package tools

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekasc/escape/engine/internal/provider"
)

// ImageOutputPrefix marks a read result whose Output is an image data URI
// rather than numbered text lines:
//
//	ImageOutputPrefix + "data:<mime>;base64,<payload>"
//
// The agent loop detects this prefix when building history and attaches the
// image to the next model call as an image content part instead of sending
// base64 as text.
const ImageOutputPrefix = "Read image: "

// imageMimes maps supported image extensions to MIME types. Used as a
// fallback when magic-byte detection is inconclusive.
var imageMimes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// sniffImage detects the MIME type of an image from its magic bytes. Supports
// PNG, JPEG, GIF and WebP; ok is false for anything else.
func sniffImage(header []byte) (mime string, ok bool) {
	switch {
	case len(header) >= 8 && bytes.Equal(header[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png", true
	case len(header) >= 3 && header[0] == 0xff && header[1] == 0xd8 && header[2] == 0xff:
		return "image/jpeg", true
	case len(header) >= 4 && string(header[:4]) == "GIF8":
		return "image/gif", true
	case len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}

// imageMIME reports the MIME type of an image file, detected from magic bytes
// with an extension fallback. ok is false for non-image files.
func imageMIME(path string) (mime string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	header := make([]byte, 12)
	n, _ := io.ReadFull(f, header)
	if mime, ok := sniffImage(header[:n]); ok {
		return mime, true
	}
	if mime, ok := imageMimes[strings.ToLower(filepath.Ext(path))]; ok {
		return mime, true
	}
	return "", false
}

// imageResult formats a read image as a note plus a base64 data URI (MIME
// type included) so the agent loop can recognise it and forward it to the
// model as an image content part.
func imageResult(mime string, data []byte) Result {
	return Result{Output: ImageOutputPrefix + (&provider.Image{MimeType: mime, Data: data}).DataURI()}
}
