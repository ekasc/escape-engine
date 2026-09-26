package diagnostics

import (
	"fmt"
	"io"
)

// WriteReport renders a snapshot as a plain-text report.
//
// One format serves both the CLI and the RPC, so a report pasted into a bug
// looks the same however it was produced.
func WriteReport(w io.Writer, s Snapshot) {
	fmt.Fprintf(w, "escape diagnostics\n")
	fmt.Fprintf(w, "  provider    %s\n  model       %s\n  thinking    %s\n", s.Provider, s.Model, s.Thinking)
	fmt.Fprintf(w, "  session     %d entries, %s\n", s.SessionEntries, humanBytes(s.SessionSize))
	fmt.Fprintf(w, "  tools       %d\n", s.Tools)
	if s.LastError != "" {
		fmt.Fprintf(w, "  last error  %s\n", s.LastError)
	}
	if len(s.Turns) == 0 {
		fmt.Fprintln(w, "\n  no turn has completed yet")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-10s %10s %10s %10s %10s %8s\n", "turn", "read", "build", "1st token", "total", "iters")
	for i, t := range s.Turns {
		fmt.Fprintf(w, "  %-10d %10s %10s %10s %10s %8d",
			i+1, msInt(t.HistoryReadMs), msInt(t.BuildMs), msInt(t.FirstTokenMs), msInt(t.TotalMs), t.Iterations)
		if t.Compacted {
			fmt.Fprint(w, "  compacted")
		}
		if t.Error != "" {
			fmt.Fprintf(w, "  %s", t.Error)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n  first token p50 %s  p95 %s\n", msInt(s.FirstTokenP50), msInt(s.FirstTokenP95))
	if s.ContextWindow > 0 {
		fmt.Fprintf(w, "  context window assumed %d tokens\n", s.ContextWindow)
	}
}

func msInt(v int64) string { return fmt.Sprintf("%dms", v) }

func humanBytes(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
