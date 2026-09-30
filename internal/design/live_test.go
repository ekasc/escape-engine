package design

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	_ "image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture writes a page with a live animation and no font preload, the two
// things that made captures differ before the settle step existed.
func fixture(t *testing.T, dir string) {
	t.Helper()
	page := `<!doctype html><html><head><meta charset="utf-8"><style>
body{margin:0;background:#fff;font-family:Georgia,serif}
@keyframes spin{from{transform:rotate(0)}to{transform:rotate(360deg)}}
.s{width:60px;height:60px;border:4px solid #4285f4;border-top-color:#ea4335;border-radius:50%;animation:spin 1.1s linear infinite}
h1{font-size:40px}</style></head><body>
<h1>Escape design capture</h1><div class="s"></div><p style="font-size:28px">Below the fold.</p>
</body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
}

func digest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func pngSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func requireAgentBrowser(t *testing.T) {
	t.Helper()
	if _, err := NewCapturer("probe"); err != nil {
		t.Skipf("capture unavailable: %v", err)
	}
}

// captureOrSkip takes two shots, treating a browser that will not start as no
// result rather than as a failure.
//
// The property under test is that two captures of an unchanged page are
// byte-identical. If the browser cannot launch there are no captures to
// compare, and failing the run would report a determinism regression that was
// never measured — which is how a flaky environment test erodes trust in the
// ones that matter. A step that ran and produced different bytes still fails.
func captureOrSkip(t *testing.T, c *Capturer, url, dir string) []Shot {
	t.Helper()
	shots, err := c.Capture(context.Background(), url, dir)
	if err != nil {
		if BrowserUnavailable(err) {
			t.Skipf("browser unavailable: %v", err)
		}
		t.Fatal(err)
	}
	return shots
}

// The review loop compares a repaired page against the previous one, so a
// capture that varies between identical runs turns every review into noise.
func TestCaptureIsByteIdenticalAcrossRuns(t *testing.T) {
	requireAgentBrowser(t)
	workspace := t.TempDir()
	site := filepath.Join(workspace, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture(t, site)

	pv, err := StartPreview(context.Background(), workspace, "site", ToolStatic)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Stop(context.Background())

	first, err := os.MkdirTemp(workspace, "shots-a-")
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.MkdirTemp(workspace, "shots-b-")
	if err != nil {
		t.Fatal(err)
	}

	cap, err := NewCapturer("determinism")
	if err != nil {
		t.Fatal(err)
	}
	runA := captureOrSkip(t, cap, pv.URL(), first)
	runB := captureOrSkip(t, cap, pv.URL(), second)

	if len(runA) != 2 || len(runB) != 2 {
		t.Fatalf("captured %d and %d shots, want 2 each", len(runA), len(runB))
	}
	for i, v := range []Viewport{Desktop, Mobile} {
		if runA[i].Viewport != v || runB[i].Viewport != v {
			t.Fatalf("shot %d viewport = %v/%v, want %v", i, runA[i].Viewport, runB[i].Viewport, v)
		}
		if w, h := pngSize(t, runA[i].Path); w != v.Width || h != v.Height {
			t.Errorf("%s is %dx%d, want %dx%d", v, w, h, v.Width, v.Height)
		}
		a, b := digest(t, runA[i].Path), digest(t, runB[i].Path)
		if a != b {
			t.Errorf("%s differed between identical runs: %s vs %s", v, a[:16], b[:16])
		}
	}
}

func TestCaptureFeedsTheStoresCapturePhase(t *testing.T) {
	requireAgentBrowser(t)
	workspace := t.TempDir()
	site := filepath.Join(workspace, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture(t, site)

	store, err := NewStore(workspace, "captured")
	if err != nil {
		t.Fatal(err)
	}
	finish(store, t)
	if err := store.Drop(PhasePreview); err != nil {
		t.Fatal(err)
	}
	if pos, _ := store.Position(); pos != PhasePreview {
		t.Fatalf("position = %q, want %q", pos, PhasePreview)
	}

	pv, err := StartPreview(context.Background(), workspace, "site", ToolStatic)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Stop(context.Background())

	cap, err := NewCapturer("captured")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cap.Capture(context.Background(), pv.URL(), store.Dir()); err != nil {
		t.Fatal(err)
	}

	pos, err := store.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseReview {
		t.Errorf("position after capture = %q, want %q", pos, PhaseReview)
	}
	if err := store.Save(Review{Pass: 0, Verdict: VerdictPass}); err != nil {
		t.Fatal(err)
	}
	pos, err = store.Position()
	if err != nil {
		t.Fatal(err)
	}
	if pos != PhaseDone {
		t.Errorf("position after a passing review = %q, want %q", pos, PhaseDone)
	}
}

// Cancelling mid-capture is the path that leaks a browser process if teardown
// runs on the cancelled context instead of one that survives it.
func TestCaptureCleansUpWhenCancelled(t *testing.T) {
	requireAgentBrowser(t)
	workspace := t.TempDir()
	site := filepath.Join(workspace, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture(t, site)

	pv, err := StartPreview(context.Background(), workspace, "site", ToolStatic)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Stop(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cap, err := NewCapturer("cancelled")
	if err != nil {
		t.Fatal(err)
	}

	// Cancel only once the namespace is demonstrably holding the page, so a
	// leaked session would have something to leak. Cancelling on a timer before
	// the browser has navigated makes the assertion vacuous: there is no page to
	// survive, with or without teardown.
	held := make(chan struct{})
	go func() {
		defer close(held)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(currentURL(t, "cancelled"), "127.0.0.1") {
				cancel()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()

	_, err = cap.Capture(ctx, pv.URL(), filepath.Join(workspace, "shots"))
	<-held
	if err == nil {
		t.Fatal("capture finished before it could be cancelled; the window under test closed")
	}

	// A leaked session is observable as page state: a namespace that still holds
	// the captured page never went through teardown. A closed namespace reports
	// about:blank because the next command relaunches a browser.
	//
	// The daemon itself is expected to outlive any one capture; it idles for an
	// hour by design. Counting daemons or sessions would report a healthy runner
	// as leaking, which is the opposite of the signal wanted here.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(currentURL(t, "cancelled"), "127.0.0.1") {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Errorf("namespace design-cancelled still holds %s after a cancelled capture", currentURL(t, "cancelled"))
}

// currentURL asks a namespace what page it is on. The namespace is derived here
// exactly as Capturer derives it, so the test cannot pass by checking a
// different one than the runner used.
func currentURL(t *testing.T, sessionID string) string {
	t.Helper()
	out, err := runAgentBrowser(t, "--namespace", "design-"+sessionID, "get", "url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func TestPreviewRefusesPathsOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "index.html"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, dir := range []string{
		"..",
		"../" + filepath.Base(outside),
		"escape",
		"escape/../..",
		"/etc",
		"",
	} {
		if pv, err := StartPreview(context.Background(), root, dir, ToolStatic); err == nil {
			// Stop it even though it should not exist. Asserting and then
			// dropping the handle left a live server behind for the rest of the
			// run, which is how this test leaked two per invocation while it was
			// still failing to catch the confinement bug it exists to catch.
			pv.Stop(context.Background())
			t.Errorf("StartPreview served %q from outside the workspace", dir)
		}
	}
}

func TestPreviewStartsServesAndStops(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture(t, site)

	pv, err := StartPreview(context.Background(), root, "site", ToolStatic)
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, pv.URL())
	if !serves(t, pv.URL()) {
		t.Fatalf("preview did not answer at %s", pv.URL())
	}

	if err := pv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := pv.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !serves(t, pv.URL()) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("preview still answering on port %d after Stop", port)
}

// A start that cannot serve must say so, and the server's own output is the
// only useful diagnosis available.
func TestPreviewReportsAnUnknownTool(t *testing.T) {
	if _, err := StartPreview(context.Background(), t.TempDir(), ".", Tool("yarn")); err == nil {
		t.Error("started a tool that is not in the allowlist")
	}
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(rawURL[len("http://"):])
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range port {
		n = n*10 + int(r-'0')
	}
	return n
}

func serves(t *testing.T, url string) bool {
	t.Helper()
	c, err := net.DialTimeout("tcp", url[len("http://"):], 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func runAgentBrowser(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin, err := exec.LookPath("agent-browser")
	if err != nil {
		t.Skipf("agent-browser unavailable: %v", err)
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}
