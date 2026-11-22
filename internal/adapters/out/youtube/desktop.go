package youtube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DesktopFlow authorizes the channel with an OAuth client of type "Desktop
// app" (developers.google.com/identity/protocols/oauth2/native-app): it serves
// the loopback redirect on ln, calls prompt with the URL to open in the
// browser, waits for Google to send the browser back with the code, exchanges
// it (PKCE, S256) and stores the token. redirectURI is the address the
// browser uses to reach ln (http://127.0.0.1:PORT; with Docker the port is
// published to the host).
func (c *Client) DesktopFlow(ctx context.Context, ln net.Listener, redirectURI string, prompt func(authURL string)) error {
	verifier, err := randomToken(48) // 64 characters of [A-Za-z0-9_-]
	if err != nil {
		return err
	}
	state, err := randomToken(24)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(DesktopScopes, " ")},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		if r.URL.Path != "/" || v.Get("state") != state {
			// favicon, stray or forged requests: answer, but keep waiting.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var res result
		switch {
		case v.Get("error") != "":
			res.err = fmt.Errorf("authorization denied in the browser (%s)", v.Get("error"))
		case v.Get("code") == "":
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		default:
			res.code = v.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		msg := "✅ Canal autorizado. Pode fechar esta aba e voltar ao terminal."
		if res.err != nil {
			msg = "❌ Autorização não concluída: " + html.EscapeString(v.Get("error")) + ". Rode task auth:youtube de novo."
		}
		_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>gta6-clipper</title><p style=\"font:18px sans-serif\">%s</p>", msg) // best effort page
		select {
		case results <- res:
		default: // a result was already delivered
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveErr <- srv.Serve(ln)
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // the flow result is what matters
		<-done
	}()

	prompt(c.authURL + "?" + q.Encode())
	var res result
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		return fmt.Errorf("loopback server: %w", err)
	case res = <-results:
	}
	if res.err != nil {
		return res.err
	}

	var tr tokenResponse
	err = c.postForm(ctx, c.oauthURL+"/token", url.Values{
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"code":          {res.code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}, &tr)
	if err != nil {
		return fmt.Errorf("exchange authorization code: %w", err)
	}
	if tr.RefreshToken == "" {
		return errors.New("authorization returned no refresh token")
	}
	return c.store(&Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    c.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        tr.Scope,
	})
}

// randomToken returns n random bytes as unpadded base64url (PKCE verifier,
// state).
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
