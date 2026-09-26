package provider

import (
	"net/http"
	"strings"
)

// effortLadder orders reasoning-effort levels from strongest to weakest. When
// a model rejects the requested level, the fallback walks down this ladder
// (medium → low → minimal → drop) instead of failing the whole call.
var effortLadder = []string{"max", "xhigh", "high", "medium", "low", "minimal"}

// nextLowerEffort returns the next weaker level, or "" to drop reasoning.
func nextLowerEffort(level string) string {
	for i, e := range effortLadder {
		if e == level && i+1 < len(effortLadder) {
			return effortLadder[i+1]
		}
	}
	return ""
}

// isReasoningRejection reports whether a 4xx response looks like the API
// rejecting the reasoning-effort parameter (model does not support the level).
func isReasoningRejection(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "reasoning") || strings.Contains(low, "effort") || strings.Contains(low, "unsupported parameter")
}
