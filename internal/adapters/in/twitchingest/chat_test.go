package twitchingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeSubscriber struct {
	mu      sync.Mutex
	subs    []string // "session/broadcaster"
	unsubs  []string
	newSub  chan string
	counter int
}

func (f *fakeSubscriber) SubscribeChat(_ context.Context, sessionID, broadcasterID string) (string, error) {
	f.mu.Lock()
	f.counter++
	id := fmt.Sprintf("sub-%d", f.counter)
	f.subs = append(f.subs, sessionID+"/"+broadcasterID)
	f.mu.Unlock()
	f.newSub <- sessionID + "/" + broadcasterID
	return id, nil
}

func (f *fakeSubscriber) Unsubscribe(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubs = append(f.unsubs, id)
	return nil
}

func msg(typ string, id string, payload any) []byte {
	m := map[string]any{"metadata": map[string]any{"message_id": id, "message_type": typ, "message_timestamp": "2026-11-21T12:00:00Z"}, "payload": payload}
	if typ == "notification" || typ == "revocation" {
		m["metadata"].(map[string]any)["subscription_type"] = "channel.chat.message"
	}
	b, _ := json.Marshal(m)
	return b
}

func welcome(id string, keepalive int) []byte {
	return msg("session_welcome", "w-"+id, map[string]any{"session": map[string]any{"id": id, "status": "connected", "keepalive_timeout_seconds": keepalive, "reconnect_url": nil}})
}

func chatNotification(id, broadcaster, text string) []byte {
	return msg("notification", id, map[string]any{
		"subscription": map[string]any{"type": "channel.chat.message", "condition": map[string]any{"broadcaster_user_id": broadcaster}},
		"event":        map[string]any{"broadcaster_user_id": broadcaster, "chatter_user_login": "viewer", "message": map[string]any{"text": text}},
	})
}

type received struct {
	broadcaster, text string
}

func TestChatClientSessionLifecycle(t *testing.T) {
	sub := &fakeSubscriber{newSub: make(chan string, 10)}
	got := make(chan received, 10)
	var srvURL string
	conns := make(chan string, 10)

	mux := http.NewServeMux()
	var mainCalls int
	var mu sync.Mutex
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		mu.Lock()
		mainCalls++
		n := mainCalls
		mu.Unlock()
		ctx := r.Context()
		session := fmt.Sprintf("s%d", n)
		conns <- session
		_ = c.Write(ctx, websocket.MessageText, welcome(session, 1))
		if n == 1 {
			// Wait for the subscription, deliver messages (one duplicated), then ask to reconnect.
			<-time.After(50 * time.Millisecond)
			_ = c.Write(ctx, websocket.MessageText, chatNotification("m1", "b1", "KEKW"))
			_ = c.Write(ctx, websocket.MessageText, chatNotification("m1", "b1", "KEKW")) // redelivery
			_ = c.Write(ctx, websocket.MessageText, msg("session_keepalive", "k1", map[string]any{}))
			_ = c.Write(ctx, websocket.MessageText, msg("session_reconnect", "r1", map[string]any{"session": map[string]any{
				"id": session, "status": "reconnecting", "reconnect_url": "ws" + strings.TrimPrefix(srvURL, "http") + "/ws2",
			}}))
		}
		// Keep the connection open until the client closes it (or it times out).
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/ws2", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		conns <- "reconnected"
		_ = c.Write(r.Context(), websocket.MessageText, welcome("s-new", 1))
		_ = c.Write(r.Context(), websocket.MessageText, chatNotification("m2", "b1", "after reconnect"))
		_ = c.Write(r.Context(), websocket.MessageText, msg("revocation", "rv1", map[string]any{"subscription": map[string]any{
			"id": "sub-1", "status": "authorization_revoked", "type": "channel.chat.message", "condition": map[string]any{"broadcaster_user_id": "b1"},
		}}))
		// Then go silent: the client must detect the keepalive timeout.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	client := NewChatClient(sub, func(b string, at time.Time, text string) { got <- received{b, text} },
		WithEventSubURL("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws"),
		WithChatLogger(slog.New(slog.DiscardHandler)),
		WithBackoff(10*time.Millisecond, 20*time.Millisecond),
		withKeepaliveGrace(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	// Nothing connects before a broadcaster is joined.
	select {
	case s := <-conns:
		t.Fatalf("connected without subscriptions: %s", s)
	case <-time.After(100 * time.Millisecond):
	}
	client.Join("b1")

	expect := func(what string, ch <-chan string, want string) {
		t.Helper()
		select {
		case v := <-ch:
			if v != want {
				t.Fatalf("%s = %q, want %q", what, v, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %s", what)
		}
	}
	expectMsg := func(want string) {
		t.Helper()
		select {
		case r := <-got:
			if r.broadcaster != "b1" || r.text != want {
				t.Fatalf("message = %+v, want %q", r, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for message %q", want)
		}
	}

	expect("connection", conns, "s1")
	expect("subscription", sub.newSub, "s1/b1")
	expectMsg("KEKW")
	expect("reconnect", conns, "reconnected")
	expectMsg("after reconnect")
	// Keepalive timeout on the new session → new connection and resubscription.
	expect("connection after timeout", conns, "s2")
	expect("resubscription", sub.newSub, "s2/b1")

	client.Leave("b1")
	deadline := time.Now().Add(5 * time.Second)
	for {
		sub.mu.Lock()
		n := len(sub.unsubs)
		sub.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unsubscribe not called after Leave")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case r := <-got:
		t.Fatalf("duplicate or unexpected message delivered: %+v", r)
	default:
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestDedup(t *testing.T) {
	d := newDedup(2)
	if !d.add("a") || !d.add("b") || d.add("a") {
		t.Fatal("dedup basic")
	}
	d.add("c") // evicts a
	if !d.add("a") {
		t.Fatal("evicted id should be accepted again")
	}
}
