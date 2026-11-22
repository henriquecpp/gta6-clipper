package youtube

import (
	"bytes"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// google is a fake of the OAuth and YouTube endpoints used by the client.
type google struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    []string // "METHOD path"
	forms    []map[string]string
	initBody []byte
	initHdr  http.Header
	puts     []http.Header
	putBody  [][]byte
	received int64 // bytes the session holds

	// Behaviour knobs.
	tokenAnswers []answer // /token answers, in order (last one repeats)
	initAnswers  []answer // POST /upload answers
	putAnswers   []answer // PUT with bytes; status 0 = accept and finish
	statusAnswer *answer  // PUT bytes */N; nil = 308 with the received range
	channels     string
}

type answer struct {
	status int
	body   string
	// keep is how many bytes of a PUT body are kept before answering status.
	keep int64
}

func newGoogle(t *testing.T) *google {
	g := &google{t: t, channels: `{"items":[{"id":"UC123","snippet":{"title":"Canal GTA"}}]}`}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

func pop(list *[]answer) answer {
	a := (*list)[0]
	if len(*list) > 1 {
		*list = (*list)[1:]
	}
	return a
}

func (g *google) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/device/code" || r.URL.Path == "/token":
		if err := r.ParseForm(); err != nil {
			g.t.Error(err)
		}
		form := map[string]string{"path": r.URL.Path}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		g.forms = append(g.forms, form)
		if r.URL.Path == "/device/code" {
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_url":"https://www.google.com/device","expires_in":1800,"interval":5}`)
			return
		}
		a := pop(&g.tokenAnswers)
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	case r.URL.Path == "/youtube/v3/channels":
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, g.channels)
	case r.Method == http.MethodPost && r.URL.Path == "/upload/youtube/v3/videos":
		g.initHdr = r.Header.Clone()
		g.initHdr.Set("X-Query", r.URL.RawQuery)
		g.initBody, _ = io.ReadAll(r.Body)
		a := answer{status: http.StatusOK}
		if len(g.initAnswers) > 0 {
			a = pop(&g.initAnswers)
		}
		if a.status == http.StatusOK {
			w.Header().Set("Location", g.srv.URL+"/session/1")
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	case r.Method == http.MethodPut && r.URL.Path == "/session/1":
		if strings.HasPrefix(r.Header.Get("Content-Range"), "bytes */") {
			if g.statusAnswer != nil {
				w.WriteHeader(g.statusAnswer.status)
				_, _ = io.WriteString(w, g.statusAnswer.body)
				return
			}
			if g.received > 0 {
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", g.received-1))
			}
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		body, _ := io.ReadAll(r.Body)
		g.puts = append(g.puts, r.Header.Clone())
		g.putBody = append(g.putBody, body)
		a := answer{}
		if len(g.putAnswers) > 0 {
			a = pop(&g.putAnswers)
		}
		if a.status != 0 {
			g.received += min(a.keep, int64(len(body)))
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, a.body)
			return
		}
		g.received += int64(len(body))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"vid123","snippet":{"title":"x"}}`)
	default:
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

type harness struct {
	g      *google
	c      *Client
	dir    string
	sleeps []time.Duration
}

func newHarness(t *testing.T, tok *Token) *harness {
	t.Helper()
	h := &harness{g: newGoogle(t), dir: t.TempDir()}
	cfg := Config{ClientID: "cid", ClientSecret: "csecret", TokenFile: filepath.Join(h.dir, "data", "youtube_token.json"),
		Privacy: "public", CategoryID: "20", NotifySubscribers: false, UploadTimeout: time.Minute}
	h.c = New(cfg, WithHTTPClient(h.g.srv.Client()), WithLogger(slog.New(slog.DiscardHandler)),
		withEndpoints(h.g.srv.URL, h.g.srv.URL+"/youtube/v3", h.g.srv.URL+"/upload/youtube/v3"),
		withNow(func() time.Time { return now }),
		withSleep(func(ctx context.Context, d time.Duration) error { h.sleeps = append(h.sleeps, d); return ctx.Err() }))
	if tok != nil {
		if err := h.c.store(tok); err != nil {
			t.Fatal(err)
		}
		h.c.tok = nil // force a load from the file
	}
	return h
}

var validToken = &Token{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: now.Add(time.Hour)}

func (h *harness) video(t *testing.T, size int) (domain.MediaRef, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("0123456789"), size/10)
	path := filepath.Join(h.dir, "final.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return domain.MediaRef(path), data
}

var meta = domain.UploadMeta{Title: "Ele pulou! #GTA6 #Shorts", Description: "Insano\n\n#GTA6", Tags: []string{"GTA 6", "alanzoka"},
	TextLanguage: "pt-BR", AudioLanguage: "pt"}

func TestPublishResumableUpload(t *testing.T) {
	h := newHarness(t, validToken)
	video, data := h.video(t, 1000)
	// The first PUT loses the connection after 400 bytes (503); the client asks
	// for the status (308, bytes=0-399) and resumes from byte 400.
	h.g.putAnswers = []answer{{status: http.StatusServiceUnavailable, keep: 400}, {}}
	pub, err := h.c.Publish(context.Background(), video, meta)
	if err != nil {
		t.Fatal(err)
	}
	if pub.ID != "vid123" || pub.URL != "https://www.youtube.com/shorts/vid123" {
		t.Fatalf("publication = %+v", pub)
	}
	hdr := h.g.initHdr
	if hdr.Get("Authorization") != "Bearer at-1" || hdr.Get("X-Upload-Content-Length") != "1000" || hdr.Get("X-Upload-Content-Type") != "video/mp4" ||
		!strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("init headers = %v", hdr)
	}
	if q := hdr.Get("X-Query"); !strings.Contains(q, "uploadType=resumable") || !strings.Contains(q, "part=snippet%2Cstatus") ||
		!strings.Contains(q, "notifySubscribers=false") {
		t.Fatalf("init query = %s", q)
	}
	var res videoResource
	if err := json.Unmarshal(h.g.initBody, &res); err != nil {
		t.Fatal(err)
	}
	if res.Snippet.Title != meta.Title || res.Snippet.CategoryID != "20" || res.Snippet.DefaultLanguage != "pt-BR" ||
		res.Snippet.DefaultAudioLanguage != "pt" || len(res.Snippet.Tags) != 2 || res.Status.PrivacyStatus != "public" || res.Status.SelfDeclaredMadeForKids {
		t.Fatalf("metadata = %+v", res)
	}
	if len(h.g.puts) != 2 || h.g.puts[0].Get("Content-Range") != "" || h.g.puts[1].Get("Content-Range") != "bytes 400-999/1000" ||
		!bytes.Equal(h.g.putBody[0], data) || !bytes.Equal(h.g.putBody[1], data[400:]) {
		t.Fatalf("puts = %v (bodies %d/%d bytes)", h.g.puts, len(h.g.putBody[0]), len(h.g.putBody[len(h.g.putBody)-1]))
	}
	if len(h.sleeps) != 1 || h.sleeps[0] != time.Second {
		t.Fatalf("sleeps = %v", h.sleeps)
	}
}

func TestPublishRefreshesTheAccessToken(t *testing.T) {
	expired := &Token{AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: now.Add(30 * time.Second)}
	h := newHarness(t, expired)
	h.g.tokenAnswers = []answer{{status: http.StatusOK, body: `{"access_token":"at-2","expires_in":3599,"token_type":"Bearer"}`}}
	video, _ := h.video(t, 100)
	if _, err := h.c.Publish(context.Background(), video, meta); err != nil {
		t.Fatal(err)
	}
	form := h.g.forms[0]
	if form["grant_type"] != "refresh_token" || form["refresh_token"] != "rt-1" || form["client_secret"] != "csecret" {
		t.Fatalf("refresh form = %v", form)
	}
	if h.g.initHdr.Get("Authorization") != "Bearer at-2" {
		t.Fatal("refreshed token not used")
	}
	var saved Token
	b, err := os.ReadFile(h.c.cfg.TokenFile)
	if err != nil || json.Unmarshal(b, &saved) != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "at-2" || saved.RefreshToken != "rt-1" || !saved.ExpiresAt.Equal(now.Add(3599*time.Second)) {
		t.Fatalf("saved token = %+v", saved)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(h.c.cfg.TokenFile); st.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %v", st.Mode().Perm())
		}
	}
}

func TestPublishRetriesSessionAfter401AndServerError(t *testing.T) {
	h := newHarness(t, validToken)
	h.g.tokenAnswers = []answer{{status: http.StatusOK, body: `{"access_token":"at-2","expires_in":3600}`}}
	h.g.initAnswers = []answer{{status: http.StatusUnauthorized, body: `{"error":{"code":401,"message":"Invalid Credentials"}}`},
		{status: http.StatusInternalServerError, body: `{"error":{"code":500,"message":"Backend Error","errors":[{"reason":"backendError"}]}}`},
		{status: http.StatusOK}}
	video, _ := h.video(t, 100)
	if _, err := h.c.Publish(context.Background(), video, meta); err != nil {
		t.Fatal(err)
	}
	if h.g.initHdr.Get("Authorization") != "Bearer at-2" || len(h.g.forms) != 1 {
		t.Fatalf("token not refreshed after 401 (forms %v)", h.g.forms)
	}
}

func TestPublishPermanentErrors(t *testing.T) {
	cases := map[string]struct {
		init, put []answer
		want      string
	}{
		"quota": {init: []answer{{status: http.StatusForbidden,
			body: `{"error":{"code":403,"message":"The request cannot be completed because you have exceeded your quota.","errors":[{"reason":"quotaExceeded"}]}}`}},
			want: "HTTP 403 quotaExceeded: The request cannot be completed"},
		"invalid title": {init: []answer{{status: http.StatusBadRequest, body: `{"error":{"code":400,"message":"bad title","errors":[{"reason":"invalidTitle"}]}}`}},
			want: "invalidTitle"},
		"session expired": {put: []answer{{status: http.StatusNotFound, body: "Not Found"}}, want: "upload session expired"},
		"rejected upload": {put: []answer{{status: http.StatusBadRequest, body: `{"error":{"code":400,"message":"uploadLimitExceeded","errors":[{"reason":"uploadLimitExceeded"}]}}`}},
			want: "uploadLimitExceeded"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, validToken)
			h.g.initAnswers, h.g.putAnswers = tc.init, tc.put
			video, _ := h.video(t, 100)
			_, err := h.c.Publish(context.Background(), video, meta)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(h.sleeps) != 0 {
				t.Fatalf("permanent error retried (sleeps %v)", h.sleeps)
			}
		})
	}
}

func TestPublishGivesUp(t *testing.T) {
	h := newHarness(t, validToken)
	h.g.putAnswers = []answer{{status: http.StatusServiceUnavailable}}
	h.g.statusAnswer = &answer{status: http.StatusServiceUnavailable}
	video, _ := h.video(t, 100)
	_, err := h.c.Publish(context.Background(), video, meta)
	if err == nil || !strings.Contains(err.Error(), "not completed after 5 attempts") {
		t.Fatalf("err = %v", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v", h.sleeps, want)
	}
}

func TestPublishFinishedWhileAskingStatus(t *testing.T) {
	h := newHarness(t, validToken)
	// The connection drops but the server got everything: the status query
	// answers with the created video, which must not be uploaded again.
	h.g.putAnswers = []answer{{status: http.StatusBadGateway, keep: 100}}
	h.g.statusAnswer = &answer{status: http.StatusCreated, body: `{"id":"vid999"}`}
	video, _ := h.video(t, 100)
	pub, err := h.c.Publish(context.Background(), video, meta)
	if err != nil || pub.ID != "vid999" || len(h.g.puts) != 1 {
		t.Fatalf("publication = %+v, %v (puts %d)", pub, err, len(h.g.puts))
	}
}

func TestTokenErrors(t *testing.T) {
	h := newHarness(t, nil)
	video, _ := h.video(t, 100)
	if _, err := h.c.Publish(context.Background(), video, meta); !errors.Is(err, ErrNoToken) {
		t.Fatalf("no token: %v", err)
	}
	if err := h.c.Load(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("load: %v", err)
	}
	h = newHarness(t, &Token{RefreshToken: "rt-revoked"})
	h.g.tokenAnswers = []answer{{status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`}}
	if _, err := h.c.Publish(context.Background(), video, meta); err == nil || !strings.Contains(err.Error(), "task auth:youtube") {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := h.c.Publish(context.Background(), "/nope.mp4", meta); err == nil {
		t.Fatal("missing video accepted")
	}
	empty := filepath.Join(h.dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Publish(context.Background(), domain.MediaRef(empty), meta); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty video: %v", err)
	}
}

func TestDeviceFlow(t *testing.T) {
	h := newHarness(t, nil)
	h.g.tokenAnswers = []answer{
		{status: http.StatusPreconditionRequired, body: `{"error":"authorization_pending","error_description":"Precondition Required"}`},
		{status: http.StatusForbidden, body: `{"error":"slow_down","error_description":"Forbidden"}`},
		{status: http.StatusOK, body: `{"access_token":"at-new","expires_in":3600,"refresh_token":"rt-new","scope":"https://www.googleapis.com/auth/youtube","token_type":"Bearer"}`},
	}
	var gotURL, gotCode string
	err := h.c.DeviceFlow(context.Background(), func(u, code string) { gotURL, gotCode = u, code })
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "https://www.google.com/device" || gotCode != "ABCD-EFGH" {
		t.Fatalf("prompt = %q %q", gotURL, gotCode)
	}
	if f := h.g.forms[0]; f["path"] != "/device/code" || f["client_id"] != "cid" || f["scope"] != Scope {
		t.Fatalf("device code form = %v", f)
	}
	if f := h.g.forms[1]; f["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || f["device_code"] != "dev-1" || f["client_secret"] != "csecret" {
		t.Fatalf("poll form = %v", f)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}; fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Fatalf("poll intervals = %v, want %v", h.sleeps, want)
	}
	if err := h.c.Load(); err != nil || h.c.tok.RefreshToken != "rt-new" {
		t.Fatalf("stored token = %+v, %v", h.c.tok, err)
	}
	title, id, err := h.c.Channel(context.Background())
	if err != nil || title != "Canal GTA" || id != "UC123" {
		t.Fatalf("channel = %q %q %v", title, id, err)
	}
	h.g.channels = `{"items":[]}`
	if _, _, err := h.c.Channel(context.Background()); err == nil {
		t.Fatal("account without channel accepted")
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	h := newHarness(t, nil)
	h.g.tokenAnswers = []answer{{status: http.StatusForbidden, body: `{"error":"access_denied","error_description":"Forbidden"}`}}
	if err := h.c.DeviceFlow(context.Background(), func(string, string) {}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v", err)
	}
	h.g.tokenAnswers = []answer{{status: http.StatusUnauthorized, body: `{"error":"invalid_client","error_description":"Unauthorized"}`}}
	if err := h.c.DeviceFlow(context.Background(), func(string, string) {}); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("err = %v", err)
	}
}

func TestReceivedBytes(t *testing.T) {
	cases := map[string]int64{"bytes=0-999999": 1_000_000, "": 0, "bytes=0-": 0, "garbage": 0, "bytes=0-0": 1}
	for in, want := range cases {
		if got := receivedBytes(in); got != want {
			t.Errorf("receivedBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAPIErrorTemporary(t *testing.T) {
	if !(&APIError{Status: 503}).Temporary() || (&APIError{Status: 403}).Temporary() {
		t.Fatal("only server errors are temporary")
	}
	if got := (&APIError{Status: 400}).Error(); got != "youtube: HTTP 400" {
		t.Fatalf("error = %q", got)
	}
}
