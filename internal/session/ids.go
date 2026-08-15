package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NewShortID returns an 8-hex-char message id, matching the shape pi uses for
// message entry ids (e.g. "c535e3e4").
func NewShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// NewSessionID returns a UUID-shaped session id (not versioned, but the same
// 8-4-4-4-12 shape as pi's).
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// SlugForDir renders the directory slug pi uses for the sessions root:
// "/Users/x/Proj" -> "--Users-x-Proj--" (leading slash stripped).
func SlugForDir(dir string) string {
	p := strings.TrimPrefix(filepath.ToSlash(dir), "/")
	return "--" + strings.ReplaceAll(p, "/", "-") + "--"
}

// NewPath builds a session file path under root for cwd:
// root/<slug>/<timestamp>_<id>.jsonl, with pi's filename timestamp shape
// (dashes instead of colons, UTC).
func NewPath(root, cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	now := NowISO()
	ts := strings.ReplaceAll(strings.ReplaceAll(now, ":", "-"), ".", "-")
	name := fmt.Sprintf("%s_%s.jsonl", ts, NewSessionID())
	return filepath.Join(root, SlugForDir(abs), name), nil
}

// DefaultRoot returns the sessions root (~/.pi/agent/sessions), honoring
// AGENT_GO_SESSIONS_DIR.
func DefaultRoot() string {
	if d := os.Getenv("AGENT_GO_SESSIONS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pi/agent/sessions"
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}
