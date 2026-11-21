package twitchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// fakeTwitch is an httptest server emulating the Helix and OAuth endpoints.
type fakeTwitch struct {
	t  *testing.T
	mu sync.Mutex

	appTokens    int
	refreshes    int
	streamCalls  [][]string
	clipCalls    int
	validateResp map[string]int // token -> status
	scopes       []string

	streamsHandler func(w http.ResponseWriter, r *http.Request) bool // return true if handled
	refreshStatus  int
	devicePolls    int
	deviceScript   []string // messages for successive polls; "" = success
}

func (f *fakeTwitch) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "client_credentials":
			if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.appTokens++
			writeJSON(w, map[string]any{"access_token": fmt.Sprintf("app-%d", f.appTokens), "expires_in": 3600, "token_type": "bearer"})
		case "refresh_token":
			if f.refreshStatus != 0 {
				w.WriteHeader(f.refreshStatus)
				writeJSON(w, map[string]any{"status": f.refreshStatus, "message": "Invalid refresh token"})
				return
			}
			f.refreshes++
			writeJSON(w, map[string]any{
				"access_token": fmt.Sprintf("user-%d", f.refreshes), "refresh_token": fmt.Sprintf("refresh-%d", f.refreshes),
				"expires_in": 14400, "scope": []string{ChatScope}, "token_type": "bearer",
			})
		case "urn:ietf:params:oauth:grant-type:device_code":
			if r.Form.Get("device_code") != "dev-code" || r.Form.Get("scopes") != ChatScope {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			msg := ""
			if f.devicePolls < len(f.deviceScript) {
				msg = f.deviceScript[f.devicePolls]
			}
			f.devicePolls++
			if msg != "" {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"status": 400, "message": msg})
				return
			}
			writeJSON(w, map[string]any{"access_token": "dev-access", "refresh_token": "dev-refresh", "expires_in": 14400, "scope": []string{ChatScope}})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("POST /oauth2/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSON(w, map[string]any{"device_code": "dev-code", "expires_in": 1800, "interval": 5, "user_code": "ABCD-EFGH", "verification_uri": "https://www.twitch.tv/activate"})
	})
	mux.HandleFunc("GET /oauth2/validate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "OAuth ")
		if st := f.validateResp[tok]; st != 0 && st != http.StatusOK {
			w.WriteHeader(st)
			writeJSON(w, map[string]any{"status": st, "message": "invalid access token"})
			return
		}
		writeJSON(w, map[string]any{"client_id": "cid", "login": "me", "user_id": "42", "scopes": f.scopes, "expires_in": 5000})
	})
	mux.HandleFunc("GET /helix/streams", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Client-Id") != "cid" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer app-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.streamCalls = append(f.streamCalls, r.URL.Query()["user_login"])
		h := f.streamsHandler
		f.mu.Unlock()
		if h != nil && h(w, r) {
			return
		}
		var data []map[string]any
		for _, l := range r.URL.Query()["user_login"] {
			if strings.HasPrefix(l, "live") {
				data = append(data, map[string]any{
					"id": "s-" + l, "user_id": "u-" + l, "user_login": l, "user_name": strings.ToUpper(l),
					"game_id": "1867279146", "game_name": "Grand Theft Auto VI", "type": "live", "title": "t", "viewer_count": 123,
					"started_at": "2026-11-21T10:00:00Z", "language": "en",
				})
			}
		}
		writeJSON(w, map[string]any{"data": data, "pagination": map[string]any{}})
	})
	mux.HandleFunc("GET /helix/clips", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.clipCalls++
		f.mu.Unlock()
		q := r.URL.Query()
		if q.Get("broadcaster_id") != "b1" || q.Get("started_at") != "2026-11-21T11:59:30Z" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if q.Get("after") == "" {
			writeJSON(w, map[string]any{
				"data": []map[string]any{
					{"id": "early", "broadcaster_id": "b1", "created_at": "2026-11-21T11:59:10Z", "duration": 30.0, "vod_offset": nil},
					{"id": "c1", "broadcaster_id": "b1", "created_at": "2026-11-21T11:59:40Z", "duration": 29.9, "vod_offset": nil, "view_count": 3},
				},
				"pagination": map[string]any{"cursor": "page2"},
			})
			return
		}
		writeJSON(w, map[string]any{
			"data":       []map[string]any{{"id": "c2", "broadcaster_id": "b1", "created_at": "2026-11-21T11:59:50Z", "duration": 60.0, "vod_offset": 7200}},
			"pagination": map[string]any{},
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type sleeper struct {
	mu    sync.Mutex
	slept []time.Duration
	now   time.Time
}

func (s *sleeper) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.slept = append(s.slept, d)
		s.now = s.now.Add(d)
	}
	return nil
}

func (s *sleeper) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func newTestClient(t *testing.T, f *fakeTwitch) (*Client, *sleeper) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	sl := &sleeper{now: testNow}
	c := New("cid", "secret",
		WithHelixURL(srv.URL+"/helix"), WithAuthURL(srv.URL+"/oauth2"),
		WithHTTPClient(srv.Client()), WithLogger(slog.New(slog.DiscardHandler)),
		withClock(sl.clock, sl.sleep))
	return c, sl
}

func TestLiveStreamsBatchesAndMaps(t *testing.T) {
	f := &fakeTwitch{t: t}
	c, _ := newTestClient(t, f)
	var logins []string
	for i := range 150 {
		logins = append(logins, fmt.Sprintf("user%d", i))
	}
	logins = append(logins, "LiveOne")
	got, err := c.LiveStreams(context.Background(), logins)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.streamCalls) != 2 || len(f.streamCalls[0]) != 100 || len(f.streamCalls[1]) != 51 {
		t.Fatalf("batches = %d (%d, %d)", len(f.streamCalls), len(f.streamCalls[0]), len(f.streamCalls[1]))
	}
	if len(got) != 1 {
		t.Fatalf("streams = %+v", got)
	}
	s := got[0]
	if s.ID != "s-liveone" || s.BroadcasterID != "u-liveone" || s.Login != "liveone" || s.Viewers != 123 ||
		s.GameID != "1867279146" || s.Game != "Grand Theft Auto VI" || !s.StartedAt.Equal(time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("mapping = %+v", s)
	}
	if f.appTokens != 1 {
		t.Fatalf("app token requested %d times, want 1 (cached)", f.appTokens)
	}
}

func TestUnauthorizedRefetchesAppToken(t *testing.T) {
	f := &fakeTwitch{t: t}
	first := true
	f.streamsHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if first {
			first = false
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		return false
	}
	c, _ := newTestClient(t, f)
	if _, err := c.LiveStreams(context.Background(), []string{"live1"}); err != nil {
		t.Fatal(err)
	}
	if f.appTokens != 2 {
		t.Fatalf("app tokens = %d, want 2", f.appTokens)
	}
}

func TestRateLimitHeadersAnd429(t *testing.T) {
	f := &fakeTwitch{t: t}
	calls := 0
	f.streamsHandler = func(w http.ResponseWriter, r *http.Request) bool {
		calls++
		reset := strconv.FormatInt(testNow.Add(10*time.Second).Unix(), 10)
		switch calls {
		case 1: // bucket empty after this one
			w.Header().Set("Ratelimit-Remaining", "0")
			w.Header().Set("Ratelimit-Reset", reset)
			return false
		case 2: // rate limited anyway
			w.Header().Set("Ratelimit-Remaining", "0")
			w.Header().Set("Ratelimit-Reset", strconv.FormatInt(testNow.Add(15*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		w.Header().Set("Ratelimit-Remaining", "799")
		w.Header().Set("Ratelimit-Reset", reset)
		return false
	}
	c, sl := newTestClient(t, f)
	ctx := context.Background()
	if _, err := c.LiveStreams(ctx, []string{"live1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LiveStreams(ctx, []string{"live1"}); err != nil {
		t.Fatal(err)
	}
	// Waited for the empty bucket (10 s), then for the 429 reset (5 s more).
	if len(sl.slept) != 2 || sl.slept[0] != 10*time.Second || sl.slept[1] != 5*time.Second {
		t.Fatalf("slept = %v", sl.slept)
	}
}

func TestServerErrors(t *testing.T) {
	f := &fakeTwitch{t: t}
	calls := 0
	f.streamsHandler = func(w http.ResponseWriter, r *http.Request) bool {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		if calls == 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"Internal Server Error","status":500,"message":"oops"}`)
			return true
		}
		return false
	}
	c, _ := newTestClient(t, f)
	if _, err := c.LiveStreams(context.Background(), []string{"live1"}); err != nil {
		t.Fatalf("503 should be retried once: %v", err)
	}
	_, err := c.LiveStreams(context.Background(), []string{"live1"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || !apiErr.Temporary() || !strings.Contains(apiErr.Error(), "oops") {
		t.Fatalf("err = %v", err)
	}
	if (&APIError{Status: 400}).Temporary() {
		t.Fatal("400 must not be temporary")
	}
}

func TestRecentClipsPaginatesAndFilters(t *testing.T) {
	f := &fakeTwitch{t: t}
	c, _ := newTestClient(t, f)
	since := testNow.Add(-30 * time.Second)
	got, err := c.RecentClips(context.Background(), "b1", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c1" || got[1].ID != "c2" {
		t.Fatalf("clips = %+v", got)
	}
	if got[0].HasVODOffset || got[0].Duration != 29900*time.Millisecond || got[0].ViewCount != 3 {
		t.Fatalf("c1 = %+v", got[0])
	}
	if !got[1].HasVODOffset || got[1].VODOffset != 2*time.Hour {
		t.Fatalf("c2 = %+v", got[1])
	}
	if f.clipCalls != 2 {
		t.Fatalf("clip calls = %d", f.clipCalls)
	}
}

func TestUserTokensLifecycle(t *testing.T) {
	f := &fakeTwitch{t: t, scopes: []string{ChatScope}}
	c, sl := newTestClient(t, f)
	path := filepath.Join(t.TempDir(), "data", "token.json")
	ut := c.NewUserTokens(path)
	ctx := context.Background()

	if err := ut.Load(); !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("Load missing = %v", err)
	}
	if _, err := ut.Token(ctx); !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("Token without file = %v", err)
	}
	if err := ut.Store(&UserToken{AccessToken: "user-0", RefreshToken: "refresh-0", ExpiresAt: testNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("token file perms = %v, %v", st.Mode().Perm(), err)
		}
	}
	tok, err := ut.Token(ctx)
	if err != nil || tok != "user-0" {
		t.Fatalf("Token = %q, %v", tok, err)
	}

	// Validate records user id and login.
	if err := ut.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if ut.UserID() != "42" {
		t.Fatalf("user id = %q", ut.UserID())
	}

	// Close to expiry: refresh rotates both tokens and persists them.
	sl.now = testNow.Add(58 * time.Minute)
	tok, err = ut.Token(ctx)
	if err != nil || tok != "user-1" {
		t.Fatalf("refreshed token = %q, %v", tok, err)
	}
	reloaded := c.NewUserTokens(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if reloaded.tok.RefreshToken != "refresh-1" || reloaded.tok.UserID != "42" {
		t.Fatalf("persisted token = %+v", reloaded.tok)
	}

	// Invalid refresh token → re-authorization required.
	f.refreshStatus = http.StatusBadRequest
	ut.Invalidate("user-1")
	if _, err := ut.Token(ctx); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("err = %v, want ErrReauthRequired", err)
	}
}

func TestUserValidateRefreshesOn401AndChecksScope(t *testing.T) {
	f := &fakeTwitch{t: t, scopes: []string{"other"}, validateResp: map[string]int{"user-0": http.StatusUnauthorized}}
	c, _ := newTestClient(t, f)
	ut := c.NewUserTokens(filepath.Join(t.TempDir(), "token.json"))
	if err := ut.Store(&UserToken{AccessToken: "user-0", RefreshToken: "r0", ExpiresAt: testNow.Add(4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	err := ut.Validate(context.Background())
	if !errors.Is(err, ErrReauthRequired) || !strings.Contains(err.Error(), ChatScope) {
		t.Fatalf("err = %v", err)
	}
	if f.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 after 401", f.refreshes)
	}
}

func TestValidateApp(t *testing.T) {
	f := &fakeTwitch{t: t, validateResp: map[string]int{"app-1": http.StatusUnauthorized}}
	c, _ := newTestClient(t, f)
	if err := c.ValidateApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.appTokens != 2 {
		t.Fatalf("app tokens = %d, want a new token after 401", f.appTokens)
	}
	if err := c.ValidateApp(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceFlow(t *testing.T) {
	f := &fakeTwitch{t: t, deviceScript: []string{"authorization_pending", "slow_down", "authorization_pending"}}
	c, sl := newTestClient(t, f)
	var uri, code string
	tok, err := c.DeviceFlow(context.Background(), []string{ChatScope}, func(u, cd string) { uri, code = u, cd })
	if err != nil {
		t.Fatal(err)
	}
	if uri != "https://www.twitch.tv/activate" || code != "ABCD-EFGH" {
		t.Fatalf("prompt = %q %q", uri, code)
	}
	if tok.AccessToken != "dev-access" || tok.RefreshToken != "dev-refresh" || f.devicePolls != 4 {
		t.Fatalf("token = %+v polls=%d", tok, f.devicePolls)
	}
	// 5 s interval, +5 s after slow_down.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	if fmt.Sprint(sl.slept) != fmt.Sprint(want) {
		t.Fatalf("slept = %v, want %v", sl.slept, want)
	}

	f2 := &fakeTwitch{t: t, deviceScript: []string{"access_denied"}}
	c2, _ := newTestClient(t, f2)
	if _, err := c2.DeviceFlow(context.Background(), []string{ChatScope}, func(string, string) {}); err == nil {
		t.Fatal("expected error for denied authorization")
	}
}
