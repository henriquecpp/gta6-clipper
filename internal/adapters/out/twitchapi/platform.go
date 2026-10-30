package twitchapi

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

var _ app.Platform = (*Client)(nil)

// maxLoginsPerRequest is the user_login limit of Get Streams.
const maxLoginsPerRequest = 100

// maxClipPages bounds Get Clips pagination per call (100 clips per page).
const maxClipPages = 5

type streamJSON struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	UserLogin   string    `json:"user_login"`
	UserName    string    `json:"user_name"`
	GameName    string    `json:"game_name"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	ViewerCount int       `json:"viewer_count"`
	StartedAt   time.Time `json:"started_at"`
	Language    string    `json:"language"`
}

type pagination struct {
	Cursor string `json:"cursor"`
}

// LiveStreams queries Get Streams in batches of up to 100 logins.
func (c *Client) LiveStreams(ctx context.Context, logins []string) ([]domain.LiveStream, error) {
	var out []domain.LiveStream
	for start := 0; start < len(logins); start += maxLoginsPerRequest {
		end := min(start+maxLoginsPerRequest, len(logins))
		q := url.Values{"type": {"live"}, "first": {"100"}}
		for _, l := range logins[start:end] {
			q.Add("user_login", strings.ToLower(l))
		}
		var resp struct {
			Data []streamJSON `json:"data"`
		}
		if err := c.getJSON(ctx, "/streams", q, c.app, &resp); err != nil {
			return nil, fmt.Errorf("get streams: %w", err)
		}
		for _, s := range resp.Data {
			if s.Type != "" && s.Type != "live" {
				continue
			}
			out = append(out, domain.LiveStream{
				ID:            domain.StreamID(s.ID),
				BroadcasterID: s.UserID,
				Login:         strings.ToLower(s.UserLogin),
				DisplayName:   s.UserName,
				Title:         s.Title,
				Game:          s.GameName,
				Language:      s.Language,
				Viewers:       s.ViewerCount,
				StartedAt:     s.StartedAt.UTC(),
			})
		}
	}
	return out, nil
}

type clipJSON struct {
	ID            string    `json:"id"`
	BroadcasterID string    `json:"broadcaster_id"`
	Title         string    `json:"title"`
	ViewCount     int       `json:"view_count"`
	CreatedAt     time.Time `json:"created_at"`
	Duration      float64   `json:"duration"`
	VODOffset     *int64    `json:"vod_offset"`
}

// RecentClips queries Get Clips for clips created since the given time. The
// API truncates started_at to the minute, so results are filtered again here.
func (c *Client) RecentClips(ctx context.Context, broadcasterID string, since time.Time) ([]domain.ViewerClip, error) {
	q := url.Values{
		"broadcaster_id": {broadcasterID},
		"started_at":     {since.UTC().Format(time.RFC3339)},
		"first":          {"100"},
	}
	var out []domain.ViewerClip
	for page := 0; page < maxClipPages; page++ {
		var resp struct {
			Data       []clipJSON `json:"data"`
			Pagination pagination `json:"pagination"`
		}
		if err := c.getJSON(ctx, "/clips", q, c.app, &resp); err != nil {
			return nil, fmt.Errorf("get clips: %w", err)
		}
		for _, cl := range resp.Data {
			if cl.CreatedAt.Before(since) {
				continue
			}
			vc := domain.ViewerClip{
				ID:            cl.ID,
				BroadcasterID: cl.BroadcasterID,
				Title:         cl.Title,
				ViewCount:     cl.ViewCount,
				CreatedAt:     cl.CreatedAt.UTC(),
				Duration:      time.Duration(math.Round(cl.Duration*1000)) * time.Millisecond,
			}
			if cl.VODOffset != nil {
				vc.VODOffset, vc.HasVODOffset = time.Duration(*cl.VODOffset)*time.Second, true
			}
			out = append(out, vc)
		}
		if resp.Pagination.Cursor == "" {
			break
		}
		q.Set("after", resp.Pagination.Cursor)
	}
	return out, nil
}
