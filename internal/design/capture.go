package design

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Viewport is a capture size in CSS pixels.
type Viewport struct {
	Width  int
	Height int
}

// The two viewports a design run photographs. Desktop is what a reviewer looks
// at; mobile is where a fixed-width layout breaks.
var (
	Desktop = Viewport{Width: 1440, Height: 1000}
	Mobile  = Viewport{Width: 390, Height: 844}
)

func shotFile(v Viewport) string { return v.String() + ".png" }

// String names a viewport after its size, so a screenshot is identifiable by its
// filename without a sidecar index that could disagree with the directory.
func (v Viewport) String() string {
	return fmt.Sprintf("shot-%dx%d", v.Width, v.Height)
}

// Shot is one captured viewport.
type Shot struct {
	Viewport Viewport
	Path     string
}

// captureTimeout bounds a whole capture, both viewports included.
const captureTimeout = 90 * time.Second

// stepTimeout bounds one agent-browser invocation. A hung browser is the common
// failure and it must not consume the whole budget silently.
const stepTimeout = 45 * time.Second

// teardownTimeout bounds the close that runs after the capture's own context is
// already cancelled, which is exactly when a leaked browser is most likely.
const teardownTimeout = 15 * time.Second

// settleScript freezes the page after it has loaded and waits for it to stop
// moving, so a repeat capture of an unchanged page is byte-identical.
//
// The order matters and is not the obvious one. Injecting the stylesheet after
// load wins the cascade against the page's own rules, which is what makes the
// freeze stick; injecting it before navigation, via --init-script, loses to them
// and leaves CSS animations running, which is the one thing that made captures
// differ between otherwise identical runs. Animations and transitions are the
// only source of run-to-run pixel drift observed here: fonts, scrollbars and
// layout were stable on their own once the viewport was pinned.
const settleScript = `(async () => {
  const style = document.createElement("style");
  style.textContent = "*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}";
  document.head.appendChild(style);
  if (document.fonts && document.fonts.ready) await document.fonts.ready;
  await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
})()`

// Capturer photographs pages through the agent-browser CLI.
//
// It is one call rather than a session the caller drives. A half-configured
// browser is a screenshot at the wrong size with no indication why, and making
// that reachable from the caller would put the determinism rules in two places.
type Capturer struct {
	bin       string
	namespace string
}

// NewCapturer resolves the agent-browser binary and derives the browser
// namespace from the session id. A missing binary is reported here, at
// construction, instead of halfway through a capture.
func NewCapturer(sessionID string) (*Capturer, error) {
	if !validSessionID(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	bin, err := exec.LookPath("agent-browser")
	if err != nil {
		return nil, fmt.Errorf("capture needs agent-browser on PATH: %w", err)
	}
	return &Capturer{bin: bin, namespace: "design-" + sessionID}, nil
}

// Capture photographs url at both viewports into outDir and returns the shots in
// order. The browser session is closed before Capture returns, on success, on
// failure, and on cancellation.
func (c *Capturer) Capture(ctx context.Context, target, outDir string) ([]Shot, error) {
	host, err := captureHost(target)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	defer c.close(ctx)

	// The allowlist is built from the host captureHost just approved, so the
	// check that admits a target and the flag that fences the browser cannot
	// drift apart.
	flags := c.flags(host)

	// The viewport is set before navigation so the page lays out once at the
	// capture size, rather than being measured at one size and rescaled.
	for _, v := range []Viewport{Desktop, Mobile} {
		if err := c.call(ctx, flags, "set", "viewport", strconv.Itoa(v.Width), strconv.Itoa(v.Height)); err != nil {
			return nil, fmt.Errorf("set viewport %s: %w", v, err)
		}
	}
	if err := c.call(ctx, flags, "open", target); err != nil {
		return nil, fmt.Errorf("open %s: %w", target, err)
	}
	if err := c.call(ctx, flags, "eval", settleScript); err != nil {
		return nil, fmt.Errorf("settle: %w", err)
	}

	shots := make([]Shot, 0, 2)
	for _, v := range []Viewport{Desktop, Mobile} {
		if err := c.call(ctx, flags, "set", "viewport", strconv.Itoa(v.Width), strconv.Itoa(v.Height)); err != nil {
			return nil, fmt.Errorf("set viewport %s: %w", v, err)
		}
		path := filepath.Join(outDir, shotFile(v))
		if err := c.call(ctx, flags, "screenshot", path); err != nil {
			return nil, fmt.Errorf("screenshot %s: %w", v, err)
		}
		shots = append(shots, Shot{Viewport: v, Path: path})
	}
	return shots, nil
}

// flags are the browser settings every invocation shares. Each one is a
// determinism or containment rule, not a preference: a pinned engine so an
// upstream default cannot change the pixels, hidden scrollbars so a scrollbar
// does not shift layout, and a domain allowlist that also stops a redirect from
// reaching off the machine.
func (c *Capturer) flags(host string) []string {
	return []string{
		"--namespace", c.namespace,
		"--engine", "chrome",
		"--hide-scrollbars", "true",
		"--allowed-domains", host,
	}
}

// ErrBrowserUnavailable reports that the browser itself could not be started or
// reached, as opposed to a step that ran and failed.
//
// These are different problems and conflating them is what made a capture
// failure read as "exit status 1": the browser not launching is a property of
// the machine, and the useful thing to say about it is that, rather than
// letting a caller read it as a broken page.
var ErrBrowserUnavailable = errors.New("agent-browser could not be started")

// browserUnavailableMarkers are the phrasings agent-browser uses when it cannot
// reach or configure its browser. Matching on its output is brittle by nature —
// these strings are not this package's — so the result is only ever used to
// choose a clearer message and to let a live test skip instead of reporting a
// determinism failure it never measured. A step that runs and fails is never
// reclassified by this.
var browserUnavailableMarkers = []string{
	"could not configure browser",
	"failed to connect",
	"no such file or directory",
}

// saysBrowserUnavailable matches agent-browser's own wording, which is the only
// way to tell a browser that would not start from a step that ran and failed.
func saysBrowserUnavailable(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range browserUnavailableMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// BrowserUnavailable reports whether err is the browser failing to start.
func BrowserUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBrowserUnavailable) {
		return true
	}
	return saysBrowserUnavailable(err.Error())
}

// call runs one agent-browser invocation under a per-step deadline.
func (c *Capturer) call(ctx context.Context, flags []string, args ...string) error {
	full := append(append([]string{}, flags...), args...)

	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	cmd := exec.CommandContext(stepCtx, c.bin, full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("agent-browser %s timed out after %s", args[0], stepTimeout)
		}
		detail := bytes.TrimSpace(out.Bytes())
		if saysBrowserUnavailable(string(detail)) || BrowserUnavailable(err) {
			return fmt.Errorf("%w: %s: %s", ErrBrowserUnavailable, args[0], detail)
		}
		return fmt.Errorf("agent-browser %s: %w: %s", args[0], err, detail)
	}
	return nil
}

// close tears the browser session down on a context that survives the capture's
// own cancellation. Without this the cancel path is the one path that leaks a
// browser process, which is the opposite of when you want one left running.
func (c *Capturer) close(ctx context.Context) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
	defer cancel()

	cmd := exec.CommandContext(cleanup, c.bin, "--namespace", c.namespace, "close")
	_ = cmd.Run()
	// A session whose driver died leaves nothing for the scoped close to reach.
	fallback := exec.CommandContext(cleanup, c.bin, "--namespace", c.namespace, "close", "--all")
	_ = fallback.Run()
}

// captureHost checks that a capture target is a local page and returns its host.
//
// Design Mode photographs its own preview server, so a non-loopback target is
// not a case to support: it is a case that should be impossible to ask for by
// accident. Loopback is also what the --allowed-domains flag above enforces, so
// the two agree instead of one quietly widening the other.
func captureHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("capture target %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("capture target %q: want an http or https URL", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return host, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("capture target %q is not on loopback", raw)
	}
	return host, nil
}

// captured reports whether both viewports are on disk at the size they claim to
// be. The dimensions are read from the file rather than trusted from the
// filename, so a truncated or stale screenshot cannot pass as this run's.
func (s *Store) captured() (bool, error) {
	for _, v := range []Viewport{Desktop, Mobile} {
		path := filepath.Join(s.dir, shotFile(v))
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		cfg, _, derr := image.DecodeConfig(f)
		f.Close()
		if derr != nil {
			return false, nil
		}
		if cfg.Width != v.Width || cfg.Height != v.Height {
			return false, nil
		}
	}
	return true, nil
}
