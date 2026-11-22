// Package youtube implements the Publisher port with the YouTube Data API v3
// over net/http (ADR 0025): OAuth 2.0 for limited-input devices to authorize
// the channel once, refresh of the access token, and the resumable upload
// protocol for videos.insert. Vertical videos up to 3 minutes are shown as
// Shorts by YouTube itself; there is no Shorts flag in the API.
package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
)

// Scope is the OAuth scope of the device flow, which accepts only the youtube
// and youtube.readonly YouTube scopes (not youtube.upload); videos.insert
// accepts youtube (developers.google.com/identity/protocols/oauth2/limited-input-device).
const Scope = "https://www.googleapis.com/auth/youtube"

// DesktopScopes are the narrower scopes of the desktop (loopback) flow:
// upload videos and read the channel, to confirm which one was authorized.
var DesktopScopes = []string{
	"https://www.googleapis.com/auth/youtube.upload",
	"https://www.googleapis.com/auth/youtube.readonly",
}

// ShortsURL is the prefix of the link of a published Short.
const ShortsURL = "https://www.youtube.com/shorts/"

const (
	defaultAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultOAuthURL  = "https://oauth2.googleapis.com"
	defaultAPIURL    = "https://www.googleapis.com/youtube/v3"
	defaultUploadURL = "https://www.googleapis.com/upload/youtube/v3"
	// maxAttempts bounds the retries of one upload (session start and resumes).
	maxAttempts = 5
)

// Config holds the OAuth client and the settings of every upload.
type Config struct {
	ClientID     string
	ClientSecret string
	// TokenFile stores the refresh token (clipper-data volume, mode 0600).
	TokenFile string
	// Privacy is public, unlisted or private.
	Privacy           string
	CategoryID        string
	NotifySubscribers bool
	MadeForKids       bool
	// UploadTimeout bounds one whole upload, retries included.
	UploadTimeout time.Duration
}

// Client talks to Google OAuth and the YouTube Data API.
type Client struct {
	cfg       Config
	http      *http.Client // OAuth and API calls (with a timeout)
	upload    *http.Client // uploads, bounded by the context instead
	authURL   string       // browser authorization page (desktop flow)
	oauthURL  string
	apiURL    string
	uploadURL string
	log       *slog.Logger
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error

	mu  sync.Mutex
	tok *Token
}

var _ app.Publisher = (*Client)(nil)

// Option configures the client.
type Option func(*Client)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

// WithHTTPClient replaces the HTTP client of every call (tests).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http, c.upload = h, h } }

// withEndpoints points the client at a fake server (tests).
func withEndpoints(oauth, api, upload string) Option {
	return func(c *Client) { c.oauthURL, c.apiURL, c.uploadURL = oauth, api, upload }
}

// withSleep replaces the wait between attempts (tests).
func withSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// withNow replaces the clock (tests).
func withNow(f func() time.Time) Option { return func(c *Client) { c.now = f } }

// New builds the client.
func New(cfg Config, opts ...Option) *Client {
	c := &Client{
		cfg:       cfg,
		http:      &http.Client{Timeout: 30 * time.Second},
		upload:    &http.Client{},
		authURL:   defaultAuthURL,
		oauthURL:  defaultOAuthURL,
		apiURL:    defaultAPIURL,
		uploadURL: defaultUploadURL,
		log:       slog.Default(),
		now:       time.Now,
		sleep:     sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoff is the wait before retry n (1, 2, 4, 8 s… up to 30 s).
func backoff(n int) time.Duration {
	d := time.Second << min(n-1, 5)
	return min(d, 30*time.Second)
}

// APIError is an error answer from Google (OAuth or YouTube).
type APIError struct {
	Status int
	// Reason is the machine-readable cause: an OAuth error code
	// (authorization_pending, invalid_grant…) or a YouTube reason
	// (quotaExceeded, uploadLimitExceeded…).
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("youtube: HTTP %d", e.Status)
	if e.Reason != "" {
		msg += " " + e.Reason
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Temporary reports whether retrying may help (server errors).
func (e *APIError) Temporary() bool { return e.Status >= 500 }

// transientErr marks a failure worth another attempt (lost connection,
// expired access token).
type transientErr struct{ err error }

func (e transientErr) Error() string { return e.err.Error() }
func (e transientErr) Unwrap() error { return e.err }
func (transientErr) Temporary() bool { return true }

func transient(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// apiError reads a Google error body. OAuth answers {"error": "code",
// "error_description": "..."}; the API answers {"error": {"message": "...",
// "errors": [{"reason": "..."}]}}.
func apiError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // a truncated body still gives the status
	e := &APIError{Status: resp.StatusCode}
	var env struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(body, &env) == nil && len(env.Error) > 0 {
		var code string
		if json.Unmarshal(env.Error, &code) == nil {
			e.Reason, e.Message = code, env.Description
		} else {
			var obj struct {
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			}
			if json.Unmarshal(env.Error, &obj) == nil {
				e.Message = obj.Message
				if len(obj.Errors) > 0 {
					e.Reason = obj.Errors[0].Reason
				}
			}
		}
	}
	if e.Reason == "" && e.Message == "" {
		e.Message = cut(strings.TrimSpace(string(body)), 300)
	}
	return e
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// postForm posts a form to an OAuth endpoint and decodes the JSON answer.
// The client secret travels in the body, never in the URL.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return transientErr{fmt.Errorf("youtube oauth: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }() // read fully below
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("youtube oauth: decode answer: %w", err)
	}
	return nil
}

// Channel returns the title and id of the channel the stored authorization
// uploads to (channels.list with mine=true), to confirm the right channel.
func (c *Client) Channel(ctx context.Context) (title, id string, err error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/channels?part=snippet&mine=true", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("youtube: list channels: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read fully below
	if resp.StatusCode != http.StatusOK {
		return "", "", apiError(resp)
	}
	var out struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("youtube: decode channels: %w", err)
	}
	if len(out.Items) == 0 {
		return "", "", errors.New("youtube: the authorized Google account has no YouTube channel")
	}
	return out.Items[0].Snippet.Title, out.Items[0].ID, nil
}
