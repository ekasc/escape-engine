package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tool output is by far the bulkiest thing in a session: one `bash` returning a
// directory listing is kilobytes, and a long session accumulates thousands of
// them. Keeping the whole thing twice — once on disk, once parsed in memory — is
// what makes an idle agent cost 1.2x its own transcript.
//
// So large output is truncated when it is written, and the full text is spilled
// to a file beside the store. The model is told where the file is and how to
// search it, so nothing is actually lost: it costs a tool call to recover, which
// is the right price for output that was almost certainly irrelevant by then.

// ToolOutputMaxBytes and ToolOutputMaxLines bound what stays inline.
const (
	ToolOutputMaxBytes = 50 * 1024
	ToolOutputMaxLines = 2000
)

// ToolOutputRetention is how long a spilled file is kept. Spilled output is a
// convenience for going back to something, not an archive.
const ToolOutputRetention = 7 * 24 * time.Hour

// SpillToolOutput returns the output to store inline, the path of the spilled
// full text when it was too large, and whether a spill happened.
//
// The returned text is a preview plus a note telling the reader how to get the
// rest. Silently shortening an answer would be worse than useless: the model
// would act on a result that looked complete and was not.
func SpillToolOutput(output, spillDir, name string) (string, string, bool) {
	lines := strings.Split(output, "\n")
	if len(lines) <= ToolOutputMaxLines && len(output) <= ToolOutputMaxBytes {
		return output, "", false
	}

	kept := make([]string, 0, ToolOutputMaxLines)
	used := 0
	for i := 0; i < len(lines) && i < ToolOutputMaxLines; i++ {
		size := len(lines[i])
		if i > 0 {
			size++
		}
		if used+size > ToolOutputMaxBytes {
			break
		}
		kept = append(kept, lines[i])
		used += size
	}
	preview := strings.Join(kept, "\n")

	path, err := writeSpill(spillDir, name, output)
	if err != nil {
		// A failed spill must not lose the output. The preview plus a note that
		// the rest is unavailable is worse than a long line, and far better
		// than pretending the result was the whole thing.
		return preview + fmt.Sprintf("\n[truncated: %d lines omitted, and the full output could not be saved]", len(lines)-len(kept)), "", true
	}
	note := fmt.Sprintf(
		"\n\n[truncated: %d of %d lines shown. The full output is at %s — use grep to search it, or read it with an offset and limit. Do not read the whole file; search it.]",
		len(kept), len(lines), path,
	)
	return preview + note, path, true
}

func writeSpill(spillDir, name, text string) (string, error) {
	if err := os.MkdirAll(spillDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(spillDir, name)
	// A unique temp name, because two turns can spill at once and a fixed
	// suffix would let one rename the other's file away.
	tmp, err := os.CreateTemp(spillDir, ".spill-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return path, nil
}

// PruneSpilledToolOutput removes spilled files older than the retention window.
// It is best effort: a failure leaves a file behind, which is harmless.
func PruneSpilledToolOutput(spillDir string) int {
	entries, err := os.ReadDir(spillDir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-ToolOutputRetention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(spillDir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}
