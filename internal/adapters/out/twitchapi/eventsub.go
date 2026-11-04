package twitchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// SubscribeChat creates a channel.chat.message (v1) EventSub subscription on
// a WebSocket session, reading chat as the authorized user. WebSocket
// transport requires a user access token with user:read:chat. It returns the
// subscription id; an existing identical subscription (409) is not an error.
func (u *UserTokens) SubscribeChat(ctx context.Context, sessionID, broadcasterID string) (string, error) {
	userID := u.UserID()
	if userID == "" {
		return "", fmt.Errorf("subscribe chat: user id unknown (validate the user token first): %w", ErrNoUserToken)
	}
	body, err := json.Marshal(map[string]any{
		"type":    "channel.chat.message",
		"version": "1",
		"condition": map[string]string{
			"broadcaster_user_id": broadcasterID,
			"user_id":             userID,
		},
		"transport": map[string]string{
			"method":     "websocket",
			"session_id": sessionID,
		},
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	err = u.c.doJSON(ctx, http.MethodPost, "/eventsub/subscriptions", nil, body, u, &resp, http.StatusAccepted)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return "", nil // already subscribed on this session
	}
	if err != nil {
		return "", fmt.Errorf("subscribe chat of %s: %w", broadcasterID, err)
	}
	if len(resp.Data) == 0 {
		return "", fmt.Errorf("subscribe chat of %s: empty response", broadcasterID)
	}
	return resp.Data[0].ID, nil
}

// Unsubscribe deletes an EventSub subscription. A missing subscription (404)
// is not an error.
func (u *UserTokens) Unsubscribe(ctx context.Context, subscriptionID string) error {
	if subscriptionID == "" {
		return nil
	}
	q := url.Values{"id": {subscriptionID}}
	err := u.c.doJSON(ctx, http.MethodDelete, "/eventsub/subscriptions", q, nil, u, nil, http.StatusNoContent)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete eventsub subscription %s: %w", subscriptionID, err)
	}
	return nil
}
