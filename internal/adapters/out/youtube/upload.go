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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// videoResource is the metadata sent with videos.insert
// (developers.google.com/youtube/v3/docs/videos).
type videoResource struct {
	Snippet videoSnippet `json:"snippet"`
	Status  videoStatus  `json:"status"`
}

type videoSnippet struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// Tags is omitted when empty: the API rejects an empty tags value.
	Tags                 []string `json:"tags,omitempty"`
	CategoryID           string   `json:"categoryId"`
	DefaultLanguage      string   `json:"defaultLanguage,omitempty"`
	DefaultAudioLanguage string   `json:"defaultAudioLanguage,omitempty"`
}

type videoStatus struct {
	PrivacyStatus           string `json:"privacyStatus"`
	SelfDeclaredMadeForKids bool   `json:"selfDeclaredMadeForKids"`
}

func (c *Client) resource(meta domain.UploadMeta) videoResource {
	return videoResource{
		Snippet: videoSnippet{
			Title: meta.Title, Description: meta.Description, Tags: meta.Tags, CategoryID: c.cfg.CategoryID,
			DefaultLanguage: meta.TextLanguage, DefaultAudioLanguage: meta.AudioLanguage,
		},
		Status: videoStatus{PrivacyStatus: c.cfg.Privacy, SelfDeclaredMadeForKids: c.cfg.MadeForKids},
	}
}

// Publish uploads the video with the resumable upload protocol: it starts a
// session with the metadata, sends the file and, after a lost connection or a
// server error, asks how much arrived and resumes from there. A failed call
// is never retried from scratch here (it could publish the video twice).
func (c *Client) Publish(ctx context.Context, video domain.MediaRef, meta domain.UploadMeta) (domain.Publication, error) {
	if c.cfg.UploadTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.UploadTimeout)
		defer cancel()
	}
	f, err := os.Open(string(video))
	if err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: open video: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only
	st, err := f.Stat()
	if err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: stat video: %w", err)
	}
	size := st.Size()
	if size == 0 {
		return domain.Publication{}, fmt.Errorf("youtube: video %s is empty", filepath.Base(string(video)))
	}
	session, err := c.startSession(ctx, c.resource(meta), size)
	if err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: start upload: %w", err)
	}
	c.log.Info("youtube upload started", slog.String("video", filepath.Base(string(video))), slog.Int64("bytes", size),
		slog.String("privacy", c.cfg.Privacy))
	id, err := c.send(ctx, session, f, size)
	if err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: upload %s: %w", filepath.Base(string(video)), err)
	}
	return domain.Publication{ID: id, URL: ShortsURL + id}, nil
}

// startSession creates the upload session and returns its URI. Starting a
// session again is safe: no video exists until the bytes are uploaded.
func (c *Client) startSession(ctx context.Context, res videoResource, size int64) (string, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"uploadType":        {"resumable"},
		"part":              {"snippet,status"},
		"notifySubscribers": {strconv.FormatBool(c.cfg.NotifySubscribers)},
	}
	endpoint := c.uploadURL + "/videos?" + q.Encode()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, backoff(attempt-1)); err != nil {
				return "", errors.Join(lastErr, err)
			}
		}
		tok, err := c.accessToken(ctx)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
		req.Header.Set("X-Upload-Content-Type", "video/mp4")
		resp, err := c.upload.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = err
			continue
		}
		loc := resp.Header.Get("Location")
		switch {
		case resp.StatusCode == http.StatusOK && loc != "":
			drain(resp)
			return loc, nil
		case resp.StatusCode == http.StatusUnauthorized:
			c.invalidate(tok)
			lastErr = apiError(resp)
			drain(resp)
		case resp.StatusCode >= 500:
			lastErr = apiError(resp)
			drain(resp)
		default:
			e := apiError(resp)
			drain(resp)
			if resp.StatusCode == http.StatusOK {
				return "", errors.New("upload session started without a Location header")
			}
			return "", e
		}
		c.log.Warn("youtube upload session not started; retrying", slog.Int("attempt", attempt), slog.String("err", lastErr.Error()))
	}
	return "", fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// send uploads the file to the session, resuming after transient failures.
func (c *Client) send(ctx context.Context, session string, f *os.File, size int64) (string, error) {
	var offset int64
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, backoff(attempt-1)); err != nil {
				return "", errors.Join(lastErr, err)
			}
			// After a failure, ask how much the server has before resuming.
			off, id, err := c.uploadStatus(ctx, session, size)
			if id != "" {
				return id, nil
			}
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if !transient(err) {
					return "", err
				}
				lastErr = err
				continue
			}
			offset = off
		}
		id, err := c.put(ctx, session, f, offset, size)
		if err == nil {
			return id, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !transient(err) {
			return "", err
		}
		lastErr = err
		c.log.Warn("youtube upload interrupted; resuming", slog.Int("attempt", attempt), slog.Int64("offset", offset),
			slog.String("err", err.Error()))
	}
	return "", fmt.Errorf("not completed after %d attempts: %w", maxAttempts, lastErr)
}

// put sends the bytes from offset to the end of the file.
func (c *Client) put(ctx context.Context, session string, f *os.File, offset, size int64) (string, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	n := size - offset
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session, io.NewSectionReader(f, offset, n))
	if err != nil {
		return "", err
	}
	req.ContentLength = n
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "video/mp4")
	if offset > 0 {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size))
	}
	resp, err := c.upload.Do(req)
	if err != nil {
		return "", transientErr{err}
	}
	defer drain(resp)
	_, id, incomplete, err := c.uploadAnswer(resp, tok)
	if incomplete {
		return "", transientErr{errors.New("upload incomplete (308)")}
	}
	return id, err
}

// uploadStatus asks how many bytes the session has received.
func (c *Client) uploadStatus(ctx context.Context, session string, size int64) (offset int64, id string, err error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session, http.NoBody)
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = 0
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	resp, err := c.upload.Do(req)
	if err != nil {
		return 0, "", transientErr{err}
	}
	defer drain(resp)
	offset, id, _, err = c.uploadAnswer(resp, tok)
	return offset, id, err
}

// uploadAnswer interprets an answer of the upload session: 200/201 with the
// video (done), 308 with the received range (incomplete), 404 (session
// expired), anything else an error.
func (c *Client) uploadAnswer(resp *http.Response, tok string) (offset int64, id string, incomplete bool, err error) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var v struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
			return 0, "", false, fmt.Errorf("upload finished but the answer could not be read: %w", err)
		}
		if v.ID == "" {
			return 0, "", false, errors.New("upload finished but the answer has no video id")
		}
		return 0, v.ID, false, nil
	case http.StatusPermanentRedirect: // 308 Resume Incomplete
		return receivedBytes(resp.Header.Get("Range")), "", true, nil
	case http.StatusUnauthorized:
		c.invalidate(tok)
		return 0, "", false, transientErr{apiError(resp)}
	case http.StatusNotFound:
		return 0, "", false, fmt.Errorf("upload session expired; ask again to start a new upload: %w", apiError(resp))
	default:
		return 0, "", false, apiError(resp)
	}
}

// receivedBytes parses "bytes=0-999999" into the next offset (1000000); no
// header means nothing was received.
func receivedBytes(rng string) int64 {
	_, span, ok := strings.Cut(rng, "=")
	if !ok {
		return 0
	}
	_, last, ok := strings.Cut(span, "-")
	n, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if !ok || err != nil || n < 0 {
		return 0
	}
	return n + 1
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // reuse the connection; nothing to report
	_ = resp.Body.Close()                                         // read-only body
}
