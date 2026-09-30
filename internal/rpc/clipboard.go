package rpc

import (
	"strings"

	"github.com/atotto/clipboard"

	"github.com/ekasc/escape-engine/internal/agent"
)

// turnEndState names how a turn finished.
//
// The engine settles with a free-form reason; the shell needs three distinct
// outcomes so it can tell a completed turn from one the user cut short. A
// stopped turn is not a failure, and rendering it as an error would tell the
// reader something untrue.
func turnEndState(reason string) string {
	switch reason {
	case agent.ReasonStopped:
		return "interrupted"
	case agent.ReasonError:
		return "error"
	default:
		return "completed"
	}
}

// clipboardMaxBytes bounds a single copy. The clipboard is a shared system
// resource, and a runaway multi-megabyte paste from a diff should not be able
// to wedge the pasteboard.
const clipboardMaxBytes = 1 << 20

// copyToClipboard puts text on the system clipboard.
//
// This lives in the engine rather than the shell because the clipboard is a
// side effect on shared system state, and the shell has no API for it — the
// same reason git writes stay here. `atotto/clipboard` shells out to pbcopy on
// macOS and is already an indirect dependency, so this adds no new module.
func copyToClipboard(text string) error {
	if text == "" {
		return nil
	}
	// A very large diff is more useful truncated than refused: the user wanted
	// the code, and a hard error would leave them with nothing.
	if len(text) > clipboardMaxBytes {
		text = text[:clipboardMaxBytes] + "\n…[copied text truncated]"
	}
	// A NUL byte makes pbcopy fail outright, so it is dropped rather than
	// allowed to take the whole copy down.
	text = strings.ReplaceAll(text, "\x00", "")
	return clipboard.WriteAll(text)
}
