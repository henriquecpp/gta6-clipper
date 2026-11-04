package twitchapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSubscribeAndUnsubscribeChat(t *testing.T) {
	var gotBody map[string]any
	status := http.StatusAccepted
	mux := http.NewServeMux()
	mux.HandleFunc("POST /helix/eventsub/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-tok" || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "sub-1", "status": "enabled"}}, "total": 1, "total_cost": 0, "max_total_cost": 10})
		}
	})
	mux.HandleFunc("DELETE /helix/eventsub/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "sub-1":
			w.WriteHeader(http.StatusNoContent)
		case "gone":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New("cid", "secret", WithHelixURL(srv.URL+"/helix"), WithAuthURL(srv.URL+"/oauth2"), WithLogger(slog.New(slog.DiscardHandler)))
	ut := c.NewUserTokens(filepath.Join(t.TempDir(), "t.json"))
	ctx := context.Background()

	if _, err := ut.SubscribeChat(ctx, "sess", "b1"); !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("without user id: %v", err)
	}
	if err := ut.Store(&UserToken{AccessToken: "user-tok", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), UserID: "42"}); err != nil {
		t.Fatal(err)
	}
	id, err := ut.SubscribeChat(ctx, "sess", "b1")
	if err != nil || id != "sub-1" {
		t.Fatalf("subscribe = %q, %v", id, err)
	}
	cond := gotBody["condition"].(map[string]any)
	tr := gotBody["transport"].(map[string]any)
	if gotBody["type"] != "channel.chat.message" || gotBody["version"] != "1" || cond["broadcaster_user_id"] != "b1" ||
		cond["user_id"] != "42" || tr["method"] != "websocket" || tr["session_id"] != "sess" {
		t.Fatalf("body = %v", gotBody)
	}
	status = http.StatusConflict
	if id, err := ut.SubscribeChat(ctx, "sess", "b1"); err != nil || id != "" {
		t.Fatalf("409 = %q, %v", id, err)
	}
	status = http.StatusBadRequest
	if _, err := ut.SubscribeChat(ctx, "sess", "b1"); err == nil {
		t.Fatal("expected error for 400")
	}
	if err := ut.Unsubscribe(ctx, "sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := ut.Unsubscribe(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := ut.Unsubscribe(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := ut.Unsubscribe(ctx, "bad"); err == nil {
		t.Fatal("expected error for 400")
	}
}
