// Package twitchapi is the Twitch Helix + OAuth adapter. It implements the
// Platform port (LiveStreams, RecentClips), manages the app access token
// (client credentials) and the user access token (Device Code Flow, refresh,
// hourly validation) and creates EventSub subscriptions for the chat.
//
// Endpoints, parameters and headers follow the official documentation
// (dev.twitch.tv/docs, consulted 2026-10-30); see docs/decisions/0006 and 0007.
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
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default endpoints.
const (
	DefaultHelixURL = "https://api.twitch.tv/helix"
	DefaultAuthURL  = "https://id.twitch.tv/oauth2"
)

// APIError is a non-2xx answer from Twitch.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("twitch api: status %d: %s", e.Status, e.Message)
}

// Temporary reports whether retrying may succeed (rate limit or server error).
func (e *APIError) Temporary() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// tokenSource provides bearer tokens and accepts invalidation after a 401.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(token string)
}

// Client talks to Helix and the OAuth endpoints.
type Client struct {
	clientID     string
	clientSecret string
	helixURL     string
	authURL      string
	http         *http.Client
	log          *slog.Logger
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error

	app *AppTokens
	rl  rateLimiter
}

// Option configures a Client (functional options).
type Option func(*Client)

// WithHTTPClient sets the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithHelixURL overrides the Helix base URL (tests).
func WithHelixURL(u string) Option { return func(c *Client) { c.helixURL = strings.TrimRight(u, "/") } }

// WithAuthURL overrides the OAuth base URL (tests).
func WithAuthURL(u string) Option { return func(c *Client) { c.authURL = strings.TrimRight(u, "/") } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

// withClock overrides time functions (tests).
func withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.now, c.sleep = now, sleep }
}

// New builds a client for the given application credentials.
func New(clientID, clientSecret string, opts ...Option) *Client {
	c := &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		helixURL:     DefaultHelixURL,
		authURL:      DefaultAuthURL,
		http:         &http.Client{Timeout: 20 * time.Second},
		log:          slog.Default(),
		now:          func() time.Time { return time.Now().UTC() },
		sleep:        sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	c.app = &AppTokens{c: c}
	return c
}

// AppTokens returns the app access token source.
func (c *Client) AppTokens() *AppTokens { return c.app }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rateLimiter tracks the token bucket reported in Ratelimit-* headers.
type rateLimiter struct {
	mu        sync.Mutex
	known     bool
	remaining int
	reset     time.Time
}

func (r *rateLimiter) update(h http.Header) {
	rem, err1 := strconv.Atoi(h.Get("Ratelimit-Remaining"))
	reset, err2 := strconv.ParseInt(h.Get("Ratelimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known, r.remaining, r.reset = true, rem, time.Unix(reset, 0).UTC()
}

// delay returns how long to wait before the next request.
func (r *rateLimiter) delay(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.known || r.remaining > 0 || !now.Before(r.reset) {
		return 0
	}
	return r.reset.Sub(now)
}

// maxAttempts bounds retries inside one call (401 refresh, 429 wait, one 503 retry).
const maxAttempts = 3

// getJSON performs a GET on Helix and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, ts tokenSource, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, q, nil, ts, out, http.StatusOK)
}

func (c *Client) doJSON(ctx context.Context, method, path string, q url.Values, body []byte, ts tokenSource, out any, wantStatus int) error {
	var lastErr error
	retried503 := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.sleep(ctx, c.rl.delay(c.now())); err != nil {
			return err
		}
		token, err := ts.Token(ctx)
		if err != nil {
			return err
		}
		u := c.helixURL + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		var rdr io.Reader
		if body != nil {
			rdr = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Client-Id", c.clientID)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("twitch %s %s: %w", method, path, err)
		}
		c.rl.update(resp.Header)
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close() // body fully read; close error carries no information
		if readErr != nil {
			return fmt.Errorf("twitch %s %s: read body: %w", method, path, readErr)
		}

		switch {
		case resp.StatusCode == wantStatus:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("twitch %s %s: decode: %w", method, path, err)
			}
			return nil
		case resp.StatusCode == http.StatusUnauthorized:
			ts.Invalidate(token)
			lastErr = apiError(resp.StatusCode, data)
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = apiError(resp.StatusCode, data)
			wait := c.rl.delay(c.now())
			if wait <= 0 {
				wait = time.Second
			}
			c.log.Warn("twitch rate limited", slog.String("path", path), slog.Duration("wait", wait))
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		case resp.StatusCode == http.StatusServiceUnavailable && !retried503:
			// The API guide says to retry a 503 once.
			retried503 = true
			lastErr = apiError(resp.StatusCode, data)
		default:
			return apiError(resp.StatusCode, data)
		}
	}
	return lastErr
}

func apiError(status int, body []byte) *APIError {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && (e.Message != "" || e.Error != "") {
		msg = strings.TrimSpace(e.Error + ": " + e.Message)
		msg = strings.TrimPrefix(msg, ": ")
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return &APIError{Status: status, Message: msg}
}

// postForm posts an OAuth form and decodes the JSON answer.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authURL+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("twitch oauth %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("twitch oauth %s: read body: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return apiError(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("twitch oauth %s: decode: %w", endpoint, err)
	}
	return nil
}

// ErrReauthRequired means the user token cannot be refreshed and the Device
// Code Flow must be run again (`task auth:twitch`).
var ErrReauthRequired = errors.New("twitch user authorization required: run `task auth:twitch`")
