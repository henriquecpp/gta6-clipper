package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// refreshMargin renews the access token this long before it expires.
const refreshMargin = time.Minute

// ErrNoToken means the channel has not been authorized yet.
var ErrNoToken = errors.New("youtube channel not authorized: run `task auth:youtube`")

// Token is the stored authorization. Only the refresh token is long-lived;
// Google does not rotate it on refresh.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

// Load reads the token file; it returns ErrNoToken when there is none.
func (c *Client) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked()
}

func (c *Client) loadLocked() error {
	b, err := os.ReadFile(c.cfg.TokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoToken
	}
	if err != nil {
		return fmt.Errorf("read youtube token file: %w", err)
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("decode youtube token file: %w", err)
	}
	if t.RefreshToken == "" {
		return ErrNoToken
	}
	c.tok = &t
	return nil
}

// saveLocked writes the token atomically with mode 0600.
func (c *Client) saveLocked(t *Token) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.cfg.TokenFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".youtube-token-*")
	if err != nil {
		return fmt.Errorf("write youtube token: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() // the chmod error is the one reported
		return fmt.Errorf("write youtube token: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close() // the write error is the one reported
		return fmt.Errorf("write youtube token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write youtube token: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.cfg.TokenFile); err != nil {
		return fmt.Errorf("write youtube token: %w", err)
	}
	c.tok = t
	return nil
}

func (c *Client) store(t *Token) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked(t)
}

// accessToken returns a valid access token, loading the token file and
// refreshing the access token when needed.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == nil {
		if err := c.loadLocked(); err != nil {
			return "", err
		}
	}
	if c.tok.AccessToken != "" && c.now().Before(c.tok.ExpiresAt.Add(-refreshMargin)) {
		return c.tok.AccessToken, nil
	}
	var tr tokenResponse
	err := c.postForm(ctx, c.oauthURL+"/token", url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.tok.RefreshToken},
	}, &tr)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Reason == "invalid_grant" {
		return "", fmt.Errorf("youtube authorization expired or revoked (with the consent screen in Testing it lasts 7 days): run `task auth:youtube`: %w", err)
	}
	if err != nil {
		return "", fmt.Errorf("refresh youtube access token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("refresh youtube access token: empty access_token")
	}
	t := *c.tok
	t.AccessToken = tr.AccessToken
	t.ExpiresAt = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.RefreshToken != "" { // not documented for this flow, but keep a rotated one
		t.RefreshToken = tr.RefreshToken
	}
	if err := c.saveLocked(&t); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// invalidate forces a refresh on the next call if token is still the cached
// one (the API answered 401).
func (c *Client) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != nil && c.tok.AccessToken == token {
		c.tok.AccessToken = ""
	}
}

type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}

// DeviceFlow authorizes the channel once (OAuth 2.0 for TV and limited-input
// devices): it asks for a code, calls prompt with the URL and the code for the
// user, polls until the user authorizes and stores the token.
func (c *Client) DeviceFlow(ctx context.Context, prompt func(verificationURL, userCode string)) error {
	var dr deviceResponse
	if err := c.postForm(ctx, c.oauthURL+"/device/code", url.Values{"client_id": {c.cfg.ClientID}, "scope": {Scope}}, &dr); err != nil {
		return fmt.Errorf("request device code: %w", err)
	}
	if dr.DeviceCode == "" || dr.UserCode == "" {
		return errors.New("request device code: empty device_code or user_code")
	}
	prompt(dr.VerificationURL, dr.UserCode)

	interval := time.Duration(max(dr.Interval, 5)) * time.Second
	deadline := c.now().Add(time.Duration(dr.ExpiresIn) * time.Second)
	form := url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"device_code":   {dr.DeviceCode},
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return err
		}
		if dr.ExpiresIn > 0 && c.now().After(deadline) {
			return errors.New("the code expired before the authorization; run the command again")
		}
		var tr tokenResponse
		err := c.postForm(ctx, c.oauthURL+"/token", form, &tr)
		var apiErr *APIError
		switch {
		case err == nil:
			if tr.RefreshToken == "" {
				return errors.New("authorization returned no refresh token")
			}
			return c.store(&Token{
				AccessToken:  tr.AccessToken,
				RefreshToken: tr.RefreshToken,
				ExpiresAt:    c.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
				Scope:        tr.Scope,
			})
		case errors.As(err, &apiErr) && apiErr.Reason == "authorization_pending":
			continue
		case errors.As(err, &apiErr) && apiErr.Reason == "slow_down":
			interval += 5 * time.Second
		case errors.As(err, &apiErr) && apiErr.Reason == "access_denied":
			return errors.New("authorization denied in the browser")
		case transient(err):
			c.log.Warn("youtube device flow poll failed; retrying", slog.String("err", err.Error()))
		default:
			return fmt.Errorf("poll device token: %w", err)
		}
	}
}
