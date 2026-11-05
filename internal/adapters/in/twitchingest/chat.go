// Package twitchingest is the inbound Twitch adapter: a supervisor per live
// broadcast that runs the capture, the chat (EventSub WebSocket) and the viewer
// clip polling, converts everything to the stream timeline and drives the
// DetectMoment use case.
package twitchingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DefaultEventSubURL is the production EventSub WebSocket endpoint with a
// 30 s keepalive (allowed range 10–600 s).
const DefaultEventSubURL = "wss://eventsub.wss.twitch.tv/ws?keepalive_timeout_seconds=30"

// ChatSubscriber creates and deletes chat subscriptions bound to a WebSocket
// session (implemented by twitchapi.UserTokens, wired in cmd).
type ChatSubscriber interface {
	SubscribeChat(ctx context.Context, sessionID, broadcasterID string) (string, error)
	Unsubscribe(ctx context.Context, subscriptionID string) error
}

// ChatHandler receives every chat message with the local receive time.
type ChatHandler func(broadcasterID string, receivedAt time.Time, text string)

// ChatClient keeps one EventSub WebSocket session with a channel.chat.message
// subscription per joined broadcaster, handling welcome, keepalive,
// session_reconnect, revocation and reconnection with backoff.
type ChatClient struct {
	url        string
	sub        ChatSubscriber
	onMsg      ChatHandler
	log        *slog.Logger
	now        func() time.Time
	httpClient *http.Client
	backoffMin time.Duration
	backoffMax time.Duration
	// grace is added to the keepalive timeout before declaring the session dead.
	grace time.Duration

	mu        sync.Mutex
	wanted    map[string]bool
	subs      map[string]string // broadcaster id -> subscription id on the current session
	sessionID string
	changed   chan struct{}
	seen      *dedup
}

// ChatOption configures a ChatClient.
type ChatOption func(*ChatClient)

// WithEventSubURL overrides the WebSocket URL (tests).
func WithEventSubURL(u string) ChatOption { return func(c *ChatClient) { c.url = u } }

// WithChatLogger sets the logger.
func WithChatLogger(l *slog.Logger) ChatOption { return func(c *ChatClient) { c.log = l } }

// WithBackoff sets the reconnection backoff bounds.
func WithBackoff(minD, maxD time.Duration) ChatOption {
	return func(c *ChatClient) { c.backoffMin, c.backoffMax = minD, maxD }
}

// withKeepaliveGrace shortens the keepalive margin (tests).
func withKeepaliveGrace(d time.Duration) ChatOption { return func(c *ChatClient) { c.grace = d } }

// NewChatClient builds a chat client; call Run to connect.
func NewChatClient(sub ChatSubscriber, onMsg ChatHandler, opts ...ChatOption) *ChatClient {
	c := &ChatClient{
		url:        DefaultEventSubURL,
		sub:        sub,
		onMsg:      onMsg,
		log:        slog.Default(),
		now:        func() time.Time { return time.Now().UTC() },
		httpClient: &http.Client{Timeout: 15 * time.Second},
		backoffMin: time.Second,
		backoffMax: time.Minute,
		grace:      5 * time.Second,
		wanted:     make(map[string]bool),
		subs:       make(map[string]string),
		changed:    make(chan struct{}, 1),
		seen:       newDedup(2048),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Join starts reading the chat of a broadcaster.
func (c *ChatClient) Join(broadcasterID string) {
	c.mu.Lock()
	c.wanted[broadcasterID] = true
	c.mu.Unlock()
	c.signal()
}

// Leave stops reading the chat of a broadcaster.
func (c *ChatClient) Leave(broadcasterID string) {
	c.mu.Lock()
	delete(c.wanted, broadcasterID)
	c.mu.Unlock()
	c.signal()
}

func (c *ChatClient) signal() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *ChatClient) wantsAny() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.wanted) > 0
}

// Run keeps a session alive until ctx is cancelled. It connects only while at
// least one broadcaster is joined (Twitch closes sessions without
// subscriptions after 10 s).
func (c *ChatClient) Run(ctx context.Context) error {
	backoff := c.backoffMin
	for {
		for !c.wantsAny() {
			select {
			case <-ctx.Done():
				return nil
			case <-c.changed:
			}
		}
		started := c.now()
		err := c.session(ctx, c.url)
		if ctx.Err() != nil {
			return nil
		}
		if c.now().Sub(started) > 2*time.Minute {
			backoff = c.backoffMin // the session was healthy; start over
		}
		wait := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
		c.log.Warn("eventsub session ended; reconnecting", slog.String("err", errString(err)), slog.Duration("backoff", wait))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		backoff = min(2*backoff, c.backoffMax)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type wsMessage struct {
	Metadata struct {
		MessageID        string `json:"message_id"`
		MessageType      string `json:"message_type"`
		SubscriptionType string `json:"subscription_type"`
	} `json:"metadata"`
	Payload json.RawMessage `json:"payload"`
}

type sessionPayload struct {
	Session struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		KeepaliveTimeoutSeconds *int   `json:"keepalive_timeout_seconds"`
		ReconnectURL            string `json:"reconnect_url"`
	} `json:"session"`
}

type notificationPayload struct {
	Subscription struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Condition struct {
			BroadcasterUserID string `json:"broadcaster_user_id"`
		} `json:"condition"`
	} `json:"subscription"`
	Event struct {
		BroadcasterUserID string `json:"broadcaster_user_id"`
		ChatterUserLogin  string `json:"chatter_user_login"`
		Message           struct {
			Text string `json:"text"`
		} `json:"message"`
	} `json:"event"`
}

// dial connects and waits for session_welcome.
func (c *ChatClient) dial(ctx context.Context, url string) (*websocket.Conn, string, time.Duration, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPClient: c.httpClient})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close() // nil on success; a NopCloser over the buffered body on failure
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("dial eventsub: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	_, data, err := conn.Read(dctx)
	if err != nil {
		_ = conn.CloseNow() // already failing; the read error is the one to report
		return nil, "", 0, fmt.Errorf("read welcome: %w", err)
	}
	var m wsMessage
	var p sessionPayload
	if err := json.Unmarshal(data, &m); err != nil {
		_ = conn.CloseNow() // protocol violation; report it
		return nil, "", 0, fmt.Errorf("decode welcome: %w", err)
	}
	if m.Metadata.MessageType != "session_welcome" {
		_ = conn.CloseNow() // protocol violation; report it
		return nil, "", 0, fmt.Errorf("expected session_welcome, got %q", m.Metadata.MessageType)
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		_ = conn.CloseNow() // protocol violation; report it
		return nil, "", 0, fmt.Errorf("decode welcome payload: %w", err)
	}
	if p.Session.ID == "" {
		_ = conn.CloseNow() // protocol violation; report it
		return nil, "", 0, errors.New("session_welcome without session id")
	}
	keepalive := 30 * time.Second
	if p.Session.KeepaliveTimeoutSeconds != nil && *p.Session.KeepaliveTimeoutSeconds > 0 {
		keepalive = time.Duration(*p.Session.KeepaliveTimeoutSeconds) * time.Second
	}
	return conn, p.Session.ID, keepalive, nil
}

// session runs one connection (following session_reconnect) until it fails.
func (c *ChatClient) session(ctx context.Context, url string) error {
	conn, sessionID, keepalive, err := c.dial(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }() // closing a dead connection has nothing to report

	c.mu.Lock()
	c.sessionID = sessionID
	c.subs = make(map[string]string) // subscriptions die with the previous session
	c.mu.Unlock()
	c.log.Info("eventsub session started", slog.String("session", sessionID), slog.Duration("keepalive", keepalive))
	c.reconcile(ctx) // must subscribe within 10 s of the welcome

	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-sctx.Done():
				return
			case <-c.changed:
				c.reconcile(sctx)
			}
		}
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	for {
		rctx, rcancel := context.WithTimeout(ctx, keepalive+keepalive/4+c.grace)
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return errors.New("keepalive timeout")
			}
			return fmt.Errorf("read eventsub: %w (close status %d)", err, websocket.CloseStatus(err))
		}
		receivedAt := c.now()
		var m wsMessage
		if err := json.Unmarshal(data, &m); err != nil {
			c.log.Warn("eventsub: undecodable message", slog.String("err", err.Error()))
			continue
		}
		if m.Metadata.MessageID != "" && !c.seen.add(m.Metadata.MessageID) {
			continue // Twitch may redeliver; process once
		}
		switch m.Metadata.MessageType {
		case "session_keepalive":
		case "notification":
			c.handleNotification(m, receivedAt)
		case "session_reconnect":
			var p sessionPayload
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				return fmt.Errorf("decode session_reconnect: %w", err)
			}
			if p.Session.ReconnectURL == "" {
				return errors.New("session_reconnect without reconnect_url")
			}
			// Connect to the new URL before closing the old connection; the
			// subscriptions move with the session.
			newConn, newID, newKeepalive, err := c.dial(ctx, p.Session.ReconnectURL)
			if err != nil {
				return fmt.Errorf("follow session_reconnect: %w", err)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "reconnecting") // old session is being replaced
			conn, keepalive = newConn, newKeepalive
			c.mu.Lock()
			c.sessionID = newID
			c.mu.Unlock()
			c.log.Info("eventsub session reconnected", slog.String("session", newID))
		case "revocation":
			var p notificationPayload
			_ = json.Unmarshal(m.Payload, &p) // best effort: only used for logging
			b := p.Subscription.Condition.BroadcasterUserID
			c.mu.Lock()
			delete(c.subs, b)
			c.mu.Unlock()
			c.log.Error("eventsub subscription revoked", slog.String("broadcaster_id", b),
				slog.String("status", p.Subscription.Status), slog.String("type", p.Subscription.Type))
		default:
			c.log.Debug("eventsub: ignored message", slog.String("type", m.Metadata.MessageType))
		}
	}
}

func (c *ChatClient) handleNotification(m wsMessage, receivedAt time.Time) {
	if m.Metadata.SubscriptionType != "channel.chat.message" {
		return
	}
	var p notificationPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		c.log.Warn("eventsub: bad chat notification", slog.String("err", err.Error()))
		return
	}
	b := p.Event.BroadcasterUserID
	if b == "" {
		b = p.Subscription.Condition.BroadcasterUserID
	}
	if c.onMsg != nil {
		c.onMsg(b, receivedAt, p.Event.Message.Text)
	}
}

// reconcile subscribes joined broadcasters and unsubscribes left ones.
func (c *ChatClient) reconcile(ctx context.Context) {
	c.mu.Lock()
	sessionID := c.sessionID
	var toAdd []string
	for b := range c.wanted {
		if _, ok := c.subs[b]; !ok {
			toAdd = append(toAdd, b)
		}
	}
	toRemove := map[string]string{}
	for b, id := range c.subs {
		if !c.wanted[b] {
			toRemove[b] = id
		}
	}
	c.mu.Unlock()

	for _, b := range toAdd {
		id, err := c.sub.SubscribeChat(ctx, sessionID, b)
		if err != nil {
			c.log.Error("subscribe chat failed", slog.String("broadcaster_id", b), slog.String("err", err.Error()))
			continue
		}
		c.mu.Lock()
		if c.sessionID == sessionID {
			c.subs[b] = id
		}
		c.mu.Unlock()
		c.log.Info("chat subscribed", slog.String("broadcaster_id", b))
	}
	for b, id := range toRemove {
		if err := c.sub.Unsubscribe(ctx, id); err != nil {
			c.log.Warn("unsubscribe chat failed", slog.String("broadcaster_id", b), slog.String("err", err.Error()))
		}
		c.mu.Lock()
		if !c.wanted[b] {
			delete(c.subs, b)
		}
		c.mu.Unlock()
	}
}

// dedup remembers the last n message ids.
type dedup struct {
	ids  map[string]struct{}
	ring []string
	next int
}

func newDedup(n int) *dedup {
	return &dedup{ids: make(map[string]struct{}, n), ring: make([]string, n)}
}

// add returns false if id was already seen.
func (d *dedup) add(id string) bool {
	if _, ok := d.ids[id]; ok {
		return false
	}
	if old := d.ring[d.next]; old != "" {
		delete(d.ids, old)
	}
	d.ring[d.next] = id
	d.ids[id] = struct{}{}
	d.next = (d.next + 1) % len(d.ring)
	return true
}
