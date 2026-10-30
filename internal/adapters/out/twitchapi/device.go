package twitchapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
}

// DeviceFlow runs the OAuth Device Code Grant Flow: it requests a device code,
// calls prompt with the URL and code for the user, and polls until the user
// authorizes, the code expires or ctx is cancelled.
func (c *Client) DeviceFlow(ctx context.Context, scopes []string, prompt func(verificationURI, userCode string)) (*UserToken, error) {
	scope := strings.Join(scopes, " ")
	var dr deviceResponse
	if err := c.postForm(ctx, "/device", url.Values{"client_id": {c.clientID}, "scopes": {scope}}, &dr); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if dr.DeviceCode == "" {
		return nil, errors.New("request device code: empty device_code")
	}
	prompt(dr.VerificationURI, dr.UserCode)

	interval := time.Duration(dr.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := c.now().Add(time.Duration(dr.ExpiresIn) * time.Second)
	form := url.Values{
		"client_id":   {c.clientID},
		"scopes":      {scope},
		"device_code": {dr.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		if dr.ExpiresIn > 0 && c.now().After(deadline) {
			return nil, errors.New("device code expired before authorization")
		}
		var tr tokenResponse
		err := c.postForm(ctx, "/token", form, &tr)
		var apiErr *APIError
		switch {
		case err == nil:
			return &UserToken{
				AccessToken:  tr.AccessToken,
				RefreshToken: tr.RefreshToken,
				ExpiresAt:    c.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
				Scopes:       tr.Scope,
			}, nil
		case errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "authorization_pending"):
			continue
		case errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "slow_down"):
			interval += 5 * time.Second
		default:
			return nil, fmt.Errorf("poll device token: %w", err)
		}
	}
}
