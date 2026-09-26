package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OpenAI OAuth (ChatGPT subscription) endpoints, mirroring the flow the Codex
// CLI uses (verified against the openai/codex source): a device-code login
// against auth.openai.com (deviceauth endpoints), then calls to the ChatGPT
// backend's Codex Responses endpoint with the access token.
const (
	// ChatGPTClientID is the public client id of the Codex app.
	ChatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	ChatGPTChatURL  = "https://chatgpt.com/backend-api/codex/responses"

	// defaultIssuer is the OAuth authority. Override with ESCAPE_OAUTH_ISSUER.
	defaultIssuer = "https://auth.openai.com"
)

// OAuthIssuer returns the OAuth authority (default auth.openai.com).
func OAuthIssuer() string {
	if v := os.Getenv("ESCAPE_OAUTH_ISSUER"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return defaultIssuer
}

// OAuthTokenURL is the token endpoint for refresh + authorization_code.
func OAuthTokenURL() string { return OAuthIssuer() + "/oauth/token" }

// TokenSet is the stored OpenAI OAuth credential pair.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"` // access-token expiry
}

// Valid reports whether the access token exists and is not about to expire.
func (t *TokenSet) Valid() bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	if t.RefreshToken == "" {
		return true // cannot refresh, use whatever we have
	}
	return time.Now().Before(t.ExpiresAt.Add(-60 * time.Second))
}

// OAuthPath returns the token store path: ESCAPE_OAUTH_FILE, else
// ~/.escape/oauth.json.
func OAuthPath() string {
	if p := os.Getenv("ESCAPE_OAUTH_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".escape/oauth.json"
	}
	return filepath.Join(home, ".escape", "oauth.json")
}

// LoadTokens reads the OAuth token store.
func LoadTokens() (*TokenSet, error) {
	raw, err := os.ReadFile(OAuthPath())
	if err != nil {
		return nil, err
	}
	var t TokenSet
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, errors.New("empty access token in store")
	}
	return &t, nil
}

// Save persists the token store.
func (t *TokenSet) Save() error {
	path := OAuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	AccountID    string `json:"account_id"`
	Error        string `json:"error"`
	Message      string `json:"message"`
}

func (r *oauthTokenResponse) tokenSet() *TokenSet {
	t := &TokenSet{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		IDToken:      r.IDToken,
		AccountID:    r.AccountID,
	}
	if r.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return t
}

// postForm performs a form-encoded POST and decodes the JSON response.
func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e oauthTokenResponse
		_ = json.Unmarshal(body, &e)
		if e.Error != "" || e.Message != "" {
			return fmt.Errorf("oauth: %s %s", e.Error, e.Message)
		}
		return fmt.Errorf("oauth: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("oauth decode: %w", err)
	}
	return nil
}

// Refresh exchanges the stored refresh token for a fresh access token and
// saves the result. Safe to call repeatedly; rotates both tokens.
func (t *TokenSet) Refresh(ctx context.Context, clientID string) error {
	if t == nil || t.RefreshToken == "" {
		return errors.New("no refresh token to exchange")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
		"client_id":     {clientID},
	}
	var r oauthTokenResponse
	if err := postForm(ctx, OAuthTokenURL(), form, &r); err != nil {
		return err
	}
	*t = *r.tokenSet()
	return t.Save()
}

// DeviceLogin runs the Codex device-code flow (openai/codex
// login/src/device_code_auth.rs): request a one-time code from
// /api/accounts/deviceauth/usercode, poll /deviceauth/token while the user
// authorizes in the browser, then exchange the issued authorization code at
// /oauth/token (grant_type=authorization_code, server-issued PKCE). The
// returned TokenSet is already persisted.
func DeviceLogin(ctx context.Context, clientID string, progress func(line string)) (*TokenSet, error) {
	issuer := OAuthIssuer()

	// 1. Request a one-time user code.
	start := map[string]any{"client_id": clientID}
	var uc struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"` // server sends a string ("5")
	}
	if err := postJSON(ctx, issuer+"/api/accounts/deviceauth/usercode", start, &uc); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, errors.New("device flow: no device code returned")
	}
	if progress != nil {
		progress(fmt.Sprintf("Open %s and enter this code: %s", issuer+"/codex/device", uc.UserCode))
	}

	// 2. Poll until the user authorizes (403/404 while pending, per codex).
	interval, _ := strconv.Atoi(uc.Interval)
	if interval < 1 {
		interval = 1
	}
	var authCode struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeChallenge     string `json:"code_challenge"`
		CodeVerifier      string `json:"code_verifier"`
	}
	poll := map[string]any{
		"device_auth_id": uc.DeviceAuthID,
		"user_code":      uc.UserCode,
	}
	deadline := time.Now().Add(15 * time.Minute)
	for {
		err := postJSON(ctx, issuer+"/api/accounts/deviceauth/token", poll, &authCode)
		if err == nil && authCode.AuthorizationCode != "" {
			break
		}
		he, httpErr := err.(*oauthHTTPError)
		// Declined/other terminal errors surface as non-403/404 statuses with
		// an error payload (codex treats only 403/404 as "authorization
		// pending"); fail fast on everything else.
		if httpErr && !strings.Contains(he.Body, "authorization_declined") &&
			he.Status != 403 && he.Status != 404 {
			return nil, err
		}
		if httpErr && strings.Contains(he.Body, "authorization_declined") {
			return nil, errors.New("device flow: authorization declined")
		}
		if !httpErr && err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, errors.New("device flow: timed out waiting for authorization")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}

	// 3. Exchange the authorization code for tokens (server-issued PKCE).
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode.AuthorizationCode},
		"redirect_uri":  {issuer + "/deviceauth/callback"},
		"client_id":     {clientID},
		"code_verifier": {authCode.CodeVerifier},
	}
	var r oauthTokenResponse
	if err := postForm(ctx, OAuthTokenURL(), form, &r); err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	t := r.tokenSet()
	if t.RefreshToken == "" {
		return nil, errors.New("device flow: no refresh_token in response")
	}
	if err := t.Save(); err != nil {
		return nil, err
	}
	return t, nil
}

// oauthHTTPError carries the status + raw body of a failed OAuth HTTP call.
type oauthHTTPError struct {
	Status int
	Body   string
}

func (e *oauthHTTPError) Error() string {
	return fmt.Sprintf("oauth: HTTP %d", e.Status)
}

// postJSON performs a JSON POST and decodes the JSON response.
func postJSON(ctx context.Context, endpoint string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &oauthHTTPError{Status: resp.StatusCode, Body: string(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("oauth decode: %w", err)
	}
	return nil
}

// ImportCodexTokens reads the Codex CLI's OAuth store (~/.codex/auth.json) and
// returns an equivalent TokenSet. Useful right after `codex login`. Note: the
// subscription refresh token rotates on refresh, so escape and codex cannot
// refresh the same credential independently afterwards.
func ImportCodexTokens() (*TokenSet, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return nil, fmt.Errorf("read ~/.codex/auth.json: %w", err)
	}
	var store struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, fmt.Errorf("parse ~/.codex/auth.json: %w", err)
	}
	t := &TokenSet{
		AccessToken:  store.Tokens.AccessToken,
		RefreshToken: store.Tokens.RefreshToken,
		IDToken:      store.Tokens.IDToken,
		// Expiry unknown from codex's store; a zero ExpiresAt forces a
		// refresh on first use, which rotates the credential.
	}
	if t.AccessToken == "" || t.RefreshToken == "" {
		return nil, errors.New("codex auth store has no usable tokens; run `codex login` first")
	}
	return t, nil
}
