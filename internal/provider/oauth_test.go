package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenSetSaveLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ESCAPE_OAUTH_FILE", filepath.Join(dir, "oauth.json"))
	tok := &TokenSet{
		AccessToken:  "acc_1",
		RefreshToken: "rt.1.abc",
		AccountID:    "acct_1",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := tok.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTokens()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "acc_1" || got.RefreshToken != "rt.1.abc" {
		t.Fatalf("round trip: %+v", got)
	}
	// Permissions must be owner-only.
	info, err := os.Stat(filepath.Join(dir, "oauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, want 0600", perm)
	}
}

func TestTokenSetValid(t *testing.T) {
	now := time.Now()
	tok := &TokenSet{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour)}
	if !tok.Valid() {
		t.Fatal("fresh token should be valid")
	}
	tok.ExpiresAt = now.Add(30 * time.Second) // inside the 60s margin
	if tok.Valid() {
		t.Fatal("token inside expiry margin should be considered invalid (needs refresh)")
	}
	tok.ExpiresAt = now.Add(time.Hour)
	tok.AccessToken = ""
	if tok.Valid() {
		t.Fatal("empty access token should be invalid")
	}
	// No refresh token: keep using whatever access token exists.
	tok = &TokenSet{AccessToken: "a", ExpiresAt: now.Add(-time.Hour)}
	if !tok.Valid() {
		t.Fatal("token without refresh should be used as-is")
	}
	if (&TokenSet{}).Valid() {
		t.Fatal("nil-ish token should be invalid")
	}
}

func TestOAuthPathEnv(t *testing.T) {
	t.Setenv("ESCAPE_OAUTH_FILE", "/tmp/x.json")
	if p := OAuthPath(); p != "/tmp/x.json" {
		t.Fatalf("OAuthPath = %s", p)
	}
}

func TestDeviceLoginFullFlow(t *testing.T) {
	// Mock the three device-flow endpoints exactly as the Codex server does
	// (see openai/codex login/tests/suite/device_code_login.rs).
	var pending = true
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_auth_id": "device-auth-123",
			"user_code":      "CODE-12345",
			"interval":       "0",
		})
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		if pending {
			pending = false
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "deviceauth_authorization_pending"},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"authorization_code": "auth-code-1",
			"code_challenge":     "challenge-1",
			"code_verifier":      "verifier-1",
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "auth-code-1" ||
			r.Form.Get("code_verifier") != "verifier-1" ||
			r.Form.Get("client_id") != ChatGPTClientID ||
			r.Form.Get("redirect_uri") == "" {
			t.Errorf("unexpected token exchange form: %v", r.Form)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"id_token":      "id-1",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("ESCAPE_OAUTH_ISSUER", srv.URL)
	t.Setenv("ESCAPE_OAUTH_FILE", filepath.Join(t.TempDir(), "oauth.json"))

	var lines []string
	tokens, err := DeviceLogin(context.Background(), ChatGPTClientID, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "CODE-12345") {
		t.Fatalf("progress lines = %v", lines)
	}
	if tokens.AccessToken != "at-1" || tokens.RefreshToken != "rt-1" || tokens.IDToken != "id-1" {
		t.Fatalf("tokens = %+v", tokens)
	}
	// Persisted.
	got, err := LoadTokens()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "at-1" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestDeviceLoginDeclined(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "d", "user_code": "C-1", "interval": "0"})
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "authorization_declined"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("ESCAPE_OAUTH_ISSUER", srv.URL)
	t.Setenv("ESCAPE_OAUTH_FILE", filepath.Join(t.TempDir(), "oauth.json"))

	if _, err := DeviceLogin(context.Background(), ChatGPTClientID, nil); err == nil {
		t.Fatal("expected declined error")
	}
}
