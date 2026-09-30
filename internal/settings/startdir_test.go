package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStartDir(t *testing.T) {
	real := t.TempDir()
	home := homeDir()
	missing := filepath.Join(t.TempDir(), "gone")

	cases := []struct {
		name      string
		requested string
		fallback  string
		want      string
	}{
		// A caller that named a real directory has already answered.
		{"a real directory always wins", real, otherDir(t), real},
		{"a real directory wins over a configured default", real, real, real},

		// The filesystem root is the answer nobody means. macOS hands it to a
		// Finder-opened app before anyone has chosen anything, and it is a
		// directory, so only an explicit rejection keeps it out.
		{"the root is not a starting point", "/", real, real},
		{"the root falls through to home", "/", "", home},

		// No directory at all, which is the same case by another route.
		{"nothing requested falls to the default", "", real, real},
		{"nothing requested and no default falls to home", "", "", home},

		// A default that has been deleted or moved must not be used: starting
		// somewhere that is not there fails later and further from the cause.
		{"a missing default falls through", "/", missing, home},

		// A configured default that does exist is the whole point.
		{"the configured default is used", "/", real, real},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveStartDir(tc.requested, &Settings{DefaultProject: tc.fallback})
			if got != tc.want {
				t.Errorf("ResolveStartDir(%q, default %q) = %q, want %q", tc.requested, tc.fallback, got, tc.want)
			}
		})
	}
}

func otherDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// The setting has to survive a write, or setting it in the UI would do nothing.
func TestDefaultProjectRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESCAPE_GLOBAL_DIR", dir)
	want := filepath.Join(dir, "projects")
	if err := WriteGlobal(map[string]any{"defaultProject": want}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultProject != want {
		t.Errorf("DefaultProject = %q, want %q", got.DefaultProject, want)
	}
}

func TestResolveStartDirIsUsableByTheApp(t *testing.T) {
	// A relative or trailing-slash value from a hand-edited settings file still
	// has to name a directory that exists.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := ResolveStartDir("", &Settings{DefaultProject: filepath.Join(dir, "work") + string(os.PathSeparator)})
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("resolved to %q, which is not a directory: %v", got, err)
	}
}

func TestExpandHome(t *testing.T) {
	home := homeDir()
	other := t.TempDir()

	cases := map[string]string{
		"~":              home,
		"~/":             home,
		"~/Projects":     filepath.Join(home, "Projects"),
		"  ~/Projects  ": filepath.Join(home, "Projects"),
		"/absolute/path": "/absolute/path",
		"relative/path":  "relative/path",
		"$HOME/thing":    "$HOME/thing",
		"":               "",
		other:            other,
	}
	for in, want := range cases {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheHomeDirectoryIsTheDefault(t *testing.T) {
	// Nothing configured and "configured to the home directory" are the same
	// answer, so the settings field can read "~/" and mean it.
	home := homeDir()
	if got := ResolveStartDir("/", &Settings{}); got != home {
		t.Errorf("with nothing set, ResolveStartDir(\"/\") = %q, want the default %q", got, home)
	}
	if got := ResolveStartDir("", &Settings{DefaultProject: "~"}); got != home {
		t.Errorf("DefaultProject \"~\" resolved to %q, want %q", got, home)
	}
	// And a tilde in the setting is expanded rather than looked for on disk.
	if got := ResolveStartDir("/", &Settings{DefaultProject: "~/."}); got != home {
		t.Errorf("a tilde in the setting resolved to %q, want %q", got, home)
	}
}
