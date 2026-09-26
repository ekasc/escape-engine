// Package settings loads pi-compatible agent settings from
// ~/.pi/agent/settings.json (global) and <cwd>/.pi/settings.json (project),
// nested-merging them with project settings winning over global settings,
// and applying pi's documented defaults for anything unset.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Settings is the merged agent configuration. It mirrors the subset of pi's
// settings surface that the escape runtime uses. Nested groups (Compaction,
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
}

// Defaults returns a Settings with pi's documented defaults applied.
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

// GlobalDir returns the global agent configuration directory, ~/.pi/agent.
func GlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".pi", "agent")
}

// ProjectDir returns the project .pi directory for cwd. It starts at cwd and
// walks up to the git root (a directory containing .git) or the filesystem
// root, returning the first <dir>/.pi found. When no ancestor has a .pi
// directory it falls back to <cwd>/.pi, the location Load reads settings from.
func ProjectDir(cwd string) string {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		dir = filepath.Clean(cwd)
	}
	dir = filepath.Clean(dir)
	start := dir
	for {
		pi := filepath.Join(dir, ".pi")
		if st, err := os.Stat(pi); err == nil && st.IsDir() {
			return pi
		}
		// Stop the walk at the git root (a directory containing .git, which
		// may be a file in worktrees) or the filesystem root. The root
		// itself is checked for .pi first, so a .pi at the git root counts.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(start, ".pi")
}

// Load merges the global settings file (~/.pi/agent/settings.json) with the
// project settings file (<cwd>/.pi/settings.json), project winning, and
// applies the pi defaults to any field neither file sets. Missing files are
// not an error; malformed JSON is.
func Load(cwd string) (*Settings, error) {
	merged, err := defaultsMap()
	if err != nil {
		return nil, err
	}

	if err := mergeFile(merged, filepath.Join(GlobalDir(), "settings.json"), "global"); err != nil {
		return nil, err
	}
	if err := mergeFile(merged, filepath.Join(cwd, ".pi", "settings.json"), "project"); err != nil {
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
	return setSkillsKey(filepath.Join(cwd, ".pi", "settings.json"), "skillOverrides", cleaned)
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
// override. This matches pi's deepMergeObjects semantics.
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
