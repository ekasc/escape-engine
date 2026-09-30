package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Subscription endpoints for the direct (non-CLI) provider backends.
const (
	// OpenCodeGoBaseURL is the OpenAI-compatible API for the OpenCode Go
	// subscription (chat/completions).
	OpenCodeGoBaseURL = "https://opencode.ai/zen/go/v1"

	// OpenCodeZenBaseURL is the OpenAI-compatible API for OpenCode Zen.
	OpenCodeZenBaseURL = "https://opencode.ai/zen/v1"

	// OpenCodeZenProviderID is the provider ID used by the OpenCode CLI.
	OpenCodeZenProviderID = "opencode"

	// OpenCodeZenDefaultModel is a free Zen model available to new users.
	OpenCodeZenDefaultModel = "space-bunny-free"
)

// OpenCodeGoKey returns the opencode-go subscription key: OPENCODE_GO_API_KEY
// env wins, else the key stored by `opencode login` at
// ~/.local/share/opencode/auth.json ("opencode-go".key).
func OpenCodeGoKey() string {
	if k := os.Getenv("ESCAPE_OPENCODE_GO_API_KEY"); k != "" {
		return k
	}
	if k := os.Getenv("OPENCODE_GO_API_KEY"); k != "" {
		return k
	}
	if k := escapeCredential("opencode-go"); k != "" {
		return k
	}
	return openCodeAuthKey("opencode-go")
}

// OpenCodeZenKey returns the OpenCode Zen key. The environment variable wins;
// otherwise the key is read from the OpenCode CLI auth store.
func OpenCodeZenKey() string {
	if k := os.Getenv("ESCAPE_OPENCODE_ZEN_API_KEY"); k != "" {
		return k
	}
	if k := os.Getenv("OPENCODE_ZEN_API_KEY"); k != "" {
		return k
	}
	if k := escapeCredential("opencode-zen"); k != "" {
		return k
	}
	return openCodeAuthKey(OpenCodeZenProviderID)
}

// RemoveOpenCodeKey deletes an Escape-owned API key stored by SaveOpenCodeKey.
// Keys from the environment or the OpenCode CLI auth store are not touched,
// so removing one of those reports that there is nothing Escape owns to delete.
func RemoveOpenCodeKey(providerID string) error {
	path := escapeCredentialsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("provider: no Escape-owned key for %s", providerID)
		}
		return fmt.Errorf("provider: read Escape credentials: %w", err)
	}
	values := make(map[string]string)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return fmt.Errorf("provider: read Escape credentials: %w", err)
		}
	}
	if _, ok := values[providerID]; !ok {
		return fmt.Errorf("provider: no Escape-owned key for %s", providerID)
	}
	delete(values, providerID)
	encoded, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("provider: encode Escape credentials: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("provider: write Escape credentials: %w", err)
	}
	return nil
}

// SaveOpenCodeKey stores an Escape-owned API key without requiring the
// OpenCode CLI. The file is private to the current user.
func SaveOpenCodeKey(providerID, key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("provider: API key cannot be empty")
	}
	path := escapeCredentialsPath()
	values := make(map[string]string)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return fmt.Errorf("provider: read Escape credentials: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider: read Escape credentials: %w", err)
	}
	values[providerID] = key
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("provider: encode Escape credentials: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("provider: create Escape credential directory: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("provider: write Escape credentials: %w", err)
	}
	return nil
}

func escapeCredential(providerID string) string {
	data, err := os.ReadFile(escapeCredentialsPath())
	if err != nil {
		return ""
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return ""
	}
	return values[providerID]
}

func escapeCredentialsPath() string {
	if path := os.Getenv("ESCAPE_CREDENTIALS_FILE"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".escape/credentials.json"
	}
	return filepath.Join(home, ".escape", "credentials.json")
}

func openCodeAuthKey(providerID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	if err != nil {
		return ""
	}
	var store map[string]struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return ""
	}
	return store[providerID].Key
}

// ChatGPTTokens loads the ChatGPT-subscription OAuth store (see DeviceLogin).
func ChatGPTTokens() (*TokenSet, error) {
	return LoadTokens()
}

var ErrNoSubscription = errors.New("no provider credentials: set ESCAPE_API_KEY, run `escape login`, or set OPENCODE_GO_API_KEY")
