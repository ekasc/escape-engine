package session

import "sync"

// Control is the shared handle between the process that builds the tool set and
// the RPC server that owns the agent. It exists because those two need the same
// live state but are built in that order: the server cannot hand its store to a
// tool set that already exists.
type Control struct {
	mu     sync.RWMutex
	path   string
	resume string
}

// SetCurrent records the session the engine is on. The server calls this
// whenever it builds an agent.
func (c *Control) SetCurrent(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
}

// Current reports the live session path.
func (c *Control) Current() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// RequestResume records a session switch asked for from inside a turn.
//
// It cannot be applied immediately. Resuming means building a new agent, and
// buildAgent stops and waits on the old one, which is the agent currently
// running the tool that asked for it. Applying it inline would deadlock the
// engine on itself, so the RPC layer drains this once the turn settles.
func (c *Control) RequestResume(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resume = path
}

// TakeResume returns the requested path and clears it. The last request wins:
// asking twice in one turn is a correction, not a queue.
func (c *Control) TakeResume() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.resume
	c.resume = ""
	return path
}
