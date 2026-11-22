package youtube

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// get requests a URL and returns its status and body.
func get(target string) (int, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// browser follows the redirect Google would send to the loopback address.
func browser(t *testing.T, redirect, query string) (int, string) {
	t.Helper()
	code, body, err := get(redirect + query)
	if err != nil {
		t.Fatal(err)
	}
	return code, body
}

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, "http://" + ln.Addr().String()
}

func TestDesktopFlow(t *testing.T) {
	h := newHarness(t, nil)
	h.c.authURL = "https://accounts.example/auth"
	h.g.tokenAnswers = []answer{{status: http.StatusOK,
		body: `{"access_token":"at-d","expires_in":3599,"refresh_token":"rt-d","scope":"https://www.googleapis.com/auth/youtube.upload https://www.googleapis.com/auth/youtube.readonly","token_type":"Bearer"}`}}
	ln, redirect := listen(t)
	var auth url.Values
	err := h.c.DesktopFlow(context.Background(), ln, redirect, func(authURL string) {
		u, err := url.Parse(authURL)
		if err != nil || u.Host != "accounts.example" {
			t.Fatalf("auth url = %s", authURL)
		}
		auth = u.Query()
		// Stray requests do not end the flow.
		if code, _ := browser(t, redirect, "/favicon.ico"); code != http.StatusNotFound {
			t.Errorf("favicon status %d", code)
		}
		if code, _ := browser(t, redirect, "/?state=forged&code=evil"); code != http.StatusNotFound {
			t.Errorf("forged state status %d", code)
		}
		code, body := browser(t, redirect, "/?state="+url.QueryEscape(auth.Get("state"))+"&code=4%2Fcode-1&scope=x")
		if code != http.StatusOK || !strings.Contains(body, "Canal autorizado") {
			t.Errorf("callback = %d %q", code, body)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Get("client_id") != "cid" || auth.Get("redirect_uri") != redirect || auth.Get("response_type") != "code" ||
		auth.Get("code_challenge_method") != "S256" || len(auth.Get("state")) < 20 ||
		auth.Get("scope") != strings.Join(DesktopScopes, " ") {
		t.Fatalf("auth params = %v", auth)
	}
	form := h.g.forms[0]
	sum := sha256.Sum256([]byte(form["code_verifier"]))
	if form["grant_type"] != "authorization_code" || form["code"] != "4/code-1" || form["redirect_uri"] != redirect ||
		form["client_secret"] != "csecret" || len(form["code_verifier"]) < 43 || len(form["code_verifier"]) > 128 ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != auth.Get("code_challenge") {
		t.Fatalf("token form = %v (challenge %s)", form, auth.Get("code_challenge"))
	}
	if err := h.c.Load(); err != nil || h.c.tok.RefreshToken != "rt-d" || h.c.tok.AccessToken != "at-d" {
		t.Fatalf("stored token = %+v, %v", h.c.tok, err)
	}
	// The loopback server is closed after the flow.
	if _, _, err := get(redirect + "/"); err == nil {
		t.Fatal("loopback server still listening")
	}
}

func TestDesktopFlowDenied(t *testing.T) {
	h := newHarness(t, nil)
	ln, redirect := listen(t)
	err := h.c.DesktopFlow(context.Background(), ln, redirect, func(authURL string) {
		u, _ := url.Parse(authURL)
		code, body := browser(t, redirect, "/?state="+url.QueryEscape(u.Query().Get("state"))+"&error=access_denied")
		if code != http.StatusOK || !strings.Contains(body, "access_denied") {
			t.Errorf("callback = %d %q", code, body)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "denied") || len(h.g.forms) != 0 {
		t.Fatalf("err = %v (token calls %d)", err, len(h.g.forms))
	}
}

func TestDesktopFlowErrors(t *testing.T) {
	// Cancelled while waiting for the browser.
	h := newHarness(t, nil)
	ln, _ := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.c.DesktopFlow(ctx, ln, "http://127.0.0.1:1", func(string) { cancel() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled flow: %v", err)
	}
	// Google rejects the code exchange.
	h = newHarness(t, nil)
	h.g.tokenAnswers = []answer{{status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"Malformed auth code."}`}}
	ln, redirect := listen(t)
	err := h.c.DesktopFlow(context.Background(), ln, redirect, func(authURL string) {
		u, _ := url.Parse(authURL)
		browser(t, redirect, "/?state="+url.QueryEscape(u.Query().Get("state"))+"&code=bad")
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("bad code: %v", err)
	}
	// No refresh token in the answer.
	h = newHarness(t, nil)
	h.g.tokenAnswers = []answer{{status: http.StatusOK, body: `{"access_token":"at","expires_in":3600}`}}
	ln, redirect = listen(t)
	err = h.c.DesktopFlow(context.Background(), ln, redirect, func(authURL string) {
		u, _ := url.Parse(authURL)
		browser(t, redirect, "/?state="+url.QueryEscape(u.Query().Get("state"))+"&code=ok")
	})
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("missing refresh token: %v", err)
	}
}
