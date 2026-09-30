// Package settings loads agent settings from
// ~/.escape/settings.json (global) and <cwd>/.escape/settings.json (project),
// nested-merging them with project settings winning over global settings,
// and applying defaults for anything unset.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ekasc/escape-engine/internal/session"
)

// Settings is the merged agent configuration. It mirrors the subset of the
// settings surface that the Escape runtime uses. Nested groups (Compaction,
// BranchSummary, Retry) merge field-by-field, so a project file that only
// sets compaction.reserveTokens keeps the global compaction.enabled value.
type Settings struct {
	DefaultProvider      string `json:"defaultProvider"`
	DefaultModel         string `json:"defaultModel"`
	DefaultThinkingLevel string `json:"defaultThinkingLevel"`
	ApprovalMode         string `json:"approvalMode"` // "auto" | "ask"
	Compaction           struct {
		Enabled          bool `json:"enabled"`
		ReserveTokens    int  `json:"reserveTokens"`
		KeepRecentTokens int  `json:"keepRecentTokens"`
	} `json:"compaction"`
	BranchSummary struct {
		ReserveTokens int  `json:"reserveTokens"`
		SkipPrompt    bool `json:"skipPrompt"`
	} `json:"branchSummary"`
	Retry struct {
		Enabled     bool `json:"enabled"`
		MaxRetries  int  `json:"maxRetries"`
		BaseDelayMs int  `json:"baseDelayMs"`
	} `json:"retry"`
	SteeringMode        string   `json:"steeringMode"` // "all" | "one-at-a-time"
	FollowUpMode        string   `json:"followUpMode"` // "all" | "one-at-a-time"
	SessionDir          string   `json:"sessionDir"`
	DefaultTools        []string `json:"defaultTools"`
	Skills              []string `json:"skills"`
	Prompts             []string `json:"prompts"`
	Extensions          []string `json:"extensions"`
	Packages            []string `json:"packages"`
	EnableSkillCommands bool     `json:"enableSkillCommands"`
	// DisabledSkills are skill paths switched off for every project. A path is
	// the only stable identifier a skill has: names collide across directories
	// and a skill can move. Sorted and deduplicated so a write is
	// deterministic.
	DisabledSkills []string `json:"disabledSkills"`
	// SkillOverrides are this project's decisions, each one beating the global
	// list. A path that is absent inherits the global choice, which is what
	// makes this a tri-state: inherit, force on, force off.
	//
	// This is a list rather than a map on purpose. Settings merge key by key, so
	// a map would let one project's overrides arrive in another project already
	// decided. A list of objects replaces wholesale, which is the semantics a
	// per-project decision needs.
	SkillOverrides      []SkillOverride `json:"skillOverrides"`
	DefaultProjectTrust string          `json:"defaultProjectTrust"` // "ask"|"always"|"never"

	// APIEndpointBaseURL overrides the endpoint an OpenAI-compatible provider is
	// called on. It is read on every request rather than baked in, so changing
	// it does not require rebuilding the provider.
	APIEndpointBaseURL string `json:"apiEndpointBaseURL"`
	TitleModel         string `json:"titleModel"`
}

// Defaults returns a Settings with the documented defaults applied.
func Defaults() *Settings {
	s := &Settings{}
	s.Compaction.Enabled = true
	s.Compaction.ReserveTokens = 16384
	s.Compaction.KeepRecentTokens = 20000
	s.BranchSummary.ReserveTokens = 16384
	s.BranchSummary.SkipPrompt = false
	s.Retry.Enabled = true
	s.Retry.MaxRetries = 3
	s.Retry.BaseDelayMs = 2000
	s.SteeringMode = "one-at-a-time"
	s.FollowUpMode = "one-at-a-time"
	s.ApprovalMode = "auto"
	s.EnableSkillCommands = true
	s.DefaultProjectTrust = "ask"
	return s
}

// GlobalDir returns the global configuration directory, ~/.escape.
// SessionRoot resolves the configured session root, honouring a leading ~.
// It lives here rather than in the command layer because the RPC layer needs it
// too, when a project switch re-reads settings for the new directory.
func SessionRoot(value string) string {
	if strings.TrimSpace(value) == "" {
		return session.DefaultRoot()
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			value = filepath.Join(home, strings.TrimPrefix(value, "~"))
		}
	}
	if abs, err := filepath.Abs(value); err == nil {
		return abs
	}
	return value
}

// GlobalDir returns the global configuration root. Escape keeps its own tree
// rather than sharing another tool's, so this is ~/.escape: its own settings,
// sessions, skills, and context files, and nothing of anyone else's.
func GlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".escape")
}

// ProjectDir returns the project .escape directory for cwd. It starts at cwd and
// walks up to the git root (a directory containing .git) or the filesystem
// root, returning the first <dir>/.escape found. When no ancestor has an
// .escape directory it falls back to <cwd>/.escape, the location Load reads
// settings from.
func ProjectDir(cwd string) string {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		dir = filepath.Clean(cwd)
	}
	dir = filepath.Clean(dir)
	start := dir
	for {
		esc := filepath.Join(dir, ".escape")
		if st, err := os.Stat(esc); err == nil && st.IsDir() {
			return esc
		}
		// Stop the walk at the git root (a directory containing .git, which
		// may be a file in worktrees) or the filesystem root. The root
		// itself is checked first, so an .escape at the git root counts.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(start, ".escape")
}

// Load merges the global settings file (~/.escape/settings.json) with the
// project settings file (<cwd>/.escape/settings.json), project winning, and
// applies the defaults to any field neither file sets. Missing files are
// not an error; malformed JSON is.
func Load(cwd string) (*Settings, error) {
	merged, err := defaultsMap()
	if err != nil {
		return nil, err
	}

	if err := mergeFile(merged, filepath.Join(GlobalDir(), "settings.json"), "global"); err != nil {
		return nil, err
	}
	if err := mergeFile(merged, filepath.Join(cwd, ".escape", "settings.json"), "project"); err != nil {
		return nil, err
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("settings: marshal merged settings: %w", err)
	}
	var s Settings
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("settings: unmarshal merged settings: %w", err)
	}
	return &s, nil
}

// SetGlobalProvider records the provider selected by the interactive login
// menu while preserving the other global settings.
func SetGlobalProvider(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("settings: provider name cannot be empty")
	}

	path := filepath.Join(GlobalDir(), "settings.json")
	values := make(map[string]any)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("settings: read global settings: %w", err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return fmt.Errorf("settings: parse global settings: %w", err)
		}
		if values == nil {
			values = make(map[string]any)
		}
	}
	values["defaultProvider"] = name
	encoded, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode global settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: create global settings directory: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("settings: write global settings: %w", err)
	}
	return nil
}

// SkillOverride is one project's decision about one skill.
type SkillOverride struct {
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

// SetDisabledSkills persists the set of skill paths switched off for every
// project, replacing whatever was there.
//
// It goes through the same read-modify-write as SetGlobalProvider rather than
// rewriting the struct, so keys this build does not know about survive a toggle.
func SetDisabledSkills(paths []string) error {
	cleaned := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		cleaned = append(cleaned, p)
	}
	sort.Strings(cleaned)
	return setSkillsKey(filepath.Join(GlobalDir(), "settings.json"), "disabledSkills", cleaned)
}

// SetSkillOverrides persists this project's skill decisions, replacing whatever
// the project file already said. A nil override list clears them, which returns
// every skill in the project to inheriting the global choice.
func SetSkillOverrides(cwd string, overrides []SkillOverride) error {
	cleaned := make([]SkillOverride, 0, len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		o.Path = strings.TrimSpace(o.Path)
		if o.Path == "" || seen[o.Path] {
			continue
		}
		seen[o.Path] = true
		cleaned = append(cleaned, o)
	}
	sort.Slice(cleaned, func(i, j int) bool { return cleaned[i].Path < cleaned[j].Path })
	return setSkillsKey(filepath.Join(cwd, ".escape", "settings.json"), "skillOverrides", cleaned)
}

// WriteGlobal persists values into the global settings file through the same
// read-modify-write as SetGlobalProvider, so keys this build does not know
// about survive. Writing the decoded struct instead would drop them, and the
// settings file is shared with other builds and by hand.
func WriteGlobal(values map[string]any) error {
	path := filepath.Join(GlobalDir(), "settings.json")
	existing := make(map[string]any)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("settings: read global settings: %w", err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("settings: parse global settings: %w", err)
		}
		if existing == nil {
			existing = make(map[string]any)
		}
	}
	for k, v := range values {
		existing[k] = v
	}
	encoded, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode global settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: create global settings dir: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("settings: write global settings: %w", err)
	}
	return nil
}

func setSkillsKey(path string, key string, value any) error {
	values := make(map[string]any)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("settings: read global settings: %w", err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return fmt.Errorf("settings: parse global settings: %w", err)
		}
		if values == nil {
			values = make(map[string]any)
		}
	}
	values[key] = value
	encoded, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode global settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: create global settings directory: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("settings: write global settings: %w", err)
	}
	return nil
}

// defaultsMap serializes Defaults() into a nested map with lowercased keys so
// file keys (matched case-insensitively by encoding/json) merge exactly.
func defaultsMap() (map[string]any, error) {
	data, err := json.Marshal(Defaults())
	if err != nil {
		return nil, fmt.Errorf("settings: marshal defaults: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("settings: unmarshal defaults: %w", err)
	}
	return lowerKeys(m), nil
}

// mergeFile reads the settings file at path (skipping missing or empty files)
// and deep-merges it into merged, with the file winning.
func mergeFile(merged map[string]any, path, scope string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("settings: read %s settings %s: %w", scope, path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil // empty file: no settings
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("settings: parse %s settings %s: %w", scope, path, err)
	}
	deepMerge(merged, lowerKeys(m))
	return nil
}

// deepMerge merges overrides into base in place: nested maps merge
// recursively, everything else (including arrays) is replaced by the
// override. Nested groups merge key by key rather than being replaced whole.
func deepMerge(base, overrides map[string]any) {
	for k, v := range overrides {
		if sub, ok := v.(map[string]any); ok {
			if baseSub, ok := base[k].(map[string]any); ok {
				deepMerge(baseSub, sub)
				continue
			}
		}
		base[k] = v
	}
}

// lowerKeys returns a copy of m with every map key lowercased, recursively.
func lowerKeys(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = lowerKeys(sub)
		}
		out[strings.ToLower(k)] = v
	}
	return out
}
