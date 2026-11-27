package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// editScopes authorize videos.update
// (developers.google.com/youtube/v3/docs/videos/update).
var editScopes = []string{
	"https://www.googleapis.com/auth/youtube",
	"https://www.googleapis.com/auth/youtube.force-ssl",
	"https://www.googleapis.com/auth/youtubepartner",
}

// CanEdit reports whether the stored authorization may change published
// videos. Authorizations made before the edit scope was requested can only
// upload and read: `task auth:youtube` again grants it. A token without the
// scope recorded is let through (the API decides).
func (c *Client) CanEdit() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == nil {
		if err := c.loadLocked(); err != nil {
			return false, err
		}
	}
	if c.tok.Scope == "" {
		return true, nil
	}
	return slices.ContainsFunc(strings.Fields(c.tok.Scope), func(s string) bool { return slices.Contains(editScopes, s) }), nil
}

// Video reads the snippet of a video (videos.list, 1 quota unit).
func (c *Client) Video(ctx context.Context, id string) (Snippet, error) {
	var out struct {
		Items []struct {
			Snippet Snippet `json:"snippet"`
		} `json:"items"`
	}
	endpoint := c.apiURL + "/videos?" + url.Values{"part": {"snippet"}, "id": {id}}.Encode()
	if err := c.call(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return Snippet{}, fmt.Errorf("youtube: read video %s: %w", id, err)
	}
	if len(out.Items) == 0 {
		return Snippet{}, fmt.Errorf("youtube: video %s not found", id)
	}
	return out.Items[0].Snippet, nil
}

// UpdateText replaces the title, description and tags of a published video
// with meta (videos.update with part=snippet, 50 quota units) and returns the
// snippet YouTube saved. The API deletes the snippet fields a request leaves
// out, so the category and the languages are kept from cur, the video's
// current snippet.
func (c *Client) UpdateText(ctx context.Context, id string, cur Snippet, meta domain.UploadMeta) (Snippet, error) {
	s := Snippet{
		Title: meta.Title, Description: meta.Description, Tags: meta.Tags,
		CategoryID: cur.CategoryID, DefaultLanguage: cur.DefaultLanguage, DefaultAudioLanguage: cur.DefaultAudioLanguage,
	}
	if s.CategoryID == "" {
		s.CategoryID = c.cfg.CategoryID
	}
	if s.DefaultLanguage == "" {
		s.DefaultLanguage = meta.TextLanguage
	}
	if s.DefaultAudioLanguage == "" {
		s.DefaultAudioLanguage = meta.AudioLanguage
	}
	in := struct {
		ID      string  `json:"id"`
		Snippet Snippet `json:"snippet"`
	}{ID: id, Snippet: s}
	var out struct {
		Snippet Snippet `json:"snippet"`
	}
	if err := c.call(ctx, http.MethodPut, c.apiURL+"/videos?part=snippet", in, &out); err != nil {
		return Snippet{}, fmt.Errorf("youtube: update video %s: %w", id, err)
	}
	return out.Snippet, nil
}

// call sends a JSON request to the API and decodes the answer; after a 401 it
// tries once more with a fresh access token.
func (c *Client) call(ctx context.Context, method, endpoint string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 1; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		var rd io.Reader = http.NoBody
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 1:
			c.invalidate(tok)
			drain(resp)
			continue
		case resp.StatusCode != http.StatusOK:
			e := apiError(resp)
			drain(resp)
			return e
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
		drain(resp)
		if err != nil {
			return fmt.Errorf("decode answer: %w", err)
		}
		return nil
	}
}
