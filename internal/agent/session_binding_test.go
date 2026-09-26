package agent

import (
	"testing"

	"github.com/ekasc/escape/engine/internal/provider"
	"github.com/ekasc/escape/engine/internal/session"
)

// sessionCapturingProvider records the session ID bound by the agent.
type sessionCapturingProvider struct {
	provider.Provider
	sessionID string
}

func (p *sessionCapturingProvider) SetSessionID(id string) { p.sessionID = id }

// TestNewBindsProviderSessionID guards the RPC regression where the provider never
// received a session ID (the CLI entry points patched it externally, the RPC path
// did not), causing OpenCode "MissingSessionID" errors.
func TestNewBindsProviderSessionID(t *testing.T) {
	dir := t.TempDir()
	store, err := session.Open(dir+"/s.jsonl", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	p := &sessionCapturingProvider{Provider: &provider.Fake{}}
	New(Options{Store: store, Provider: p})

	if p.sessionID == "" {
		t.Fatal("provider session ID was not bound by agent.New")
	}
	if p.sessionID != store.ID() {
		t.Fatalf("session ID = %q, want %q", p.sessionID, store.ID())
	}
}
