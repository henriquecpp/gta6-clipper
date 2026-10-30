package twitchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// refreshMargin renews tokens this long before they expire.
const refreshMargin = 5 * time.Minute

type tokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int64    `json:"expires_in"`
	Scope        []string `json:"scope"`
	TokenType    string   `json:"token_type"`
}

// AppTokens caches the app access token (client credentials grant). App
// tokens cannot be refreshed; a new one is requested when needed.
type AppTokens struct {
	c       *Client
	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a valid app token, requesting one if needed.
func (a *AppTokens) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.c.now().Before(a.expires.Add(-refreshMargin)) {
		return a.token, nil
	}
	var tr tokenResponse
	form := url.Values{
		"client_id":     {a.c.clientID},
		"client_secret": {a.c.clientSecret},
		"grant_type":    {"client_credentials"},
	}
	if err := a.c.postForm(ctx, "/token", form, &tr); err != nil {
		return "", fmt.Errorf("get app access token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("get app access token: empty access_token")
	}
	a.token = tr.AccessToken
	a.expires = a.c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return a.token, nil
}

// Invalidate drops the cached token if it is the one that failed.
func (a *AppTokens) Invalidate(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token = ""
	}
}

// UserToken is the persisted user access token.
type UserToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
	UserID       string    `json:"user_id,omitempty"`
	Login        string    `json:"login,omitempty"`
}

// ChatScope is the scope required to read chat via EventSub
// (channel.chat.message with a user access token).
const ChatScope = "user:read:chat"

// ErrNoUserToken means no user token has been stored yet.
var ErrNoUserToken = errors.New("no twitch user token: run `task auth:twitch`")

// UserTokens manages the user access token stored in a file with 0600
// permissions in the clipper-data volume. Refresh tokens rotate, so every
// renewal is written back atomically.
type UserTokens struct {
	c    *Client
	path string
	mu   sync.Mutex
	tok  *UserToken
}

// NewUserTokens binds a token file to the client.
func (c *Client) NewUserTokens(path string) *UserTokens {
	return &UserTokens{c: c, path: path}
}

// Load reads the token file. It returns ErrNoUserToken if it does not exist.
func (u *UserTokens) Load() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	b, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoUserToken
	}
	if err != nil {
		return fmt.Errorf("read twitch token file: %w", err)
	}
	var t UserToken
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("decode twitch token file: %w", err)
	}
	if t.AccessToken == "" || t.RefreshToken == "" {
		return ErrNoUserToken
	}
	u.tok = &t
	return nil
}

// Store saves a token (used by the Device Code Flow).
func (u *UserTokens) Store(t *UserToken) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.saveLocked(t); err != nil {
		return err
	}
	u.tok = t
	return nil
}

func (u *UserTokens) saveLocked(t *UserToken) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(u.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".twitch-token-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil && !isChmodUnsupported(err) {
		_ = tmp.Close() // the chmod error is the one worth reporting
		return fmt.Errorf("chmod token file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return fmt.Errorf("write token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(tmp.Name(), u.path); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}
	return nil
}

// isChmodUnsupported covers Windows, where Chmod on an open file is not supported.
func isChmodUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || strings.Contains(err.Error(), "not supported")
}

// UserID returns the id of the authorized user ("" if unknown).
func (u *UserTokens) UserID() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tok == nil {
		return ""
	}
	return u.tok.UserID
}

// Token returns a valid user access token, refreshing it if needed.
func (u *UserTokens) Token(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tok == nil {
		return "", ErrNoUserToken
	}
	if u.c.now().Before(u.tok.ExpiresAt.Add(-refreshMargin)) {
		return u.tok.AccessToken, nil
	}
	if err := u.refreshLocked(ctx); err != nil {
		return "", err
	}
	return u.tok.AccessToken, nil
}

// Invalidate forces a refresh on the next Token call after a 401.
func (u *UserTokens) Invalidate(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tok != nil && u.tok.AccessToken == token {
		u.tok.ExpiresAt = time.Time{}
	}
}

func (u *UserTokens) refreshLocked(ctx context.Context) error {
	form := url.Values{
		"client_id":     {u.c.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {u.tok.RefreshToken},
	}
	if u.c.clientSecret != "" { // not required for public clients
		form.Set("client_secret", u.c.clientSecret)
	}
	var tr tokenResponse
	if err := u.c.postForm(ctx, "/token", form, &tr); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusBadRequest || apiErr.Status == http.StatusUnauthorized) {
			return fmt.Errorf("refresh twitch user token: %w (%w)", ErrReauthRequired, err)
		}
		return fmt.Errorf("refresh twitch user token: %w", err)
	}
	if tr.AccessToken == "" {
		return errors.New("refresh twitch user token: empty access_token")
	}
	next := *u.tok
	next.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	next.ExpiresAt = u.c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if len(tr.Scope) > 0 {
		next.Scopes = tr.Scope
	}
	if err := u.saveLocked(&next); err != nil {
		return err
	}
	u.tok = &next
	u.c.log.Info("twitch user token refreshed", slog.Time("expires_at", next.ExpiresAt))
	return nil
}

// validateResponse is the body of GET /oauth2/validate.
type validateResponse struct {
	ClientID  string   `json:"client_id"`
	Login     string   `json:"login"`
	UserID    string   `json:"user_id"`
	Scopes    []string `json:"scopes"`
	ExpiresIn int64    `json:"expires_in"`
}

// validate calls GET /oauth2/validate ("Authorization: OAuth <token>").
func (c *Client) validate(ctx context.Context, token string) (*validateResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.authURL+"/validate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("validate token: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.StatusCode, data)
	}
	var v validateResponse
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("validate token: decode: %w", err)
	}
	return &v, nil
}

// ValidateApp validates the app token, discarding it if Twitch rejects it.
// Twitch requires validation at startup and hourly.
func (c *Client) ValidateApp(ctx context.Context) error {
	tok, err := c.app.Token(ctx)
	if err != nil {
		return err
	}
	if _, err := c.validate(ctx, tok); err != nil {
		c.app.Invalidate(tok)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			_, err = c.app.Token(ctx) // get a fresh one now
			return err
		}
		return err
	}
	return nil
}

// Validate validates the user token (refreshing on 401), records the user id
// and login, and checks the chat scope.
func (u *UserTokens) Validate(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := u.Token(ctx)
		if err != nil {
			return err
		}
		v, err := u.c.validate(ctx, tok)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized && attempt == 0 {
			u.Invalidate(tok)
			continue
		}
		if err != nil {
			return err
		}
		u.mu.Lock()
		changed := u.tok.UserID != v.UserID || u.tok.Login != v.Login
		u.tok.UserID, u.tok.Login, u.tok.Scopes = v.UserID, v.Login, v.Scopes
		var saveErr error
		if changed {
			saveErr = u.saveLocked(u.tok)
		}
		u.mu.Unlock()
		if saveErr != nil {
			return saveErr
		}
		if !slices.Contains(v.Scopes, ChatScope) {
			return fmt.Errorf("twitch user token lacks scope %s: %w", ChatScope, ErrReauthRequired)
		}
		return nil
	}
	return ErrReauthRequired
}
