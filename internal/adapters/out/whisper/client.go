package whisper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal HTTP client for whisper-server's /inference and /health.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client for the server at baseURL (e.g. http://whisper:8080).
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: hc}
}

// ErrUnavailable means the server is down or still loading the model.
var ErrUnavailable = errors.New("whisper server unavailable")

// Health checks GET /health (200 {"status":"ok"} when the model is loaded).
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for connection reuse
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: health status %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}

// InferenceOptions are the multipart form fields sent with the audio.
type InferenceOptions struct {
	// Language is an ISO code or "auto".
	Language string
}

// Inference uploads WAV audio and returns the raw verbose_json body.
func (c *Client) Inference(ctx context.Context, wav io.Reader, opts InferenceOptions) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, wav); err != nil {
		return nil, fmt.Errorf("buffer audio: %w", err)
	}
	lang := opts.Language
	if lang == "" {
		lang = "auto"
	}
	fields := map[string]string{
		"response_format": "verbose_json",
		"temperature":     "0.0",
		"temperature_inc": "0.2",
		"language":        lang,
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/inference", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read whisper response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: status 503", ErrUnavailable)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("whisper inference status %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	return b, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
