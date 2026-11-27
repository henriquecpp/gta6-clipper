package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
)

const videoJSON = `{"items":[{"id":"vid1","snippet":{"title":"Velho #GTA6","description":"texto velho","tags":["GTA 6"],` +
	`"categoryId":"20","defaultLanguage":"pt-BR","defaultAudioLanguage":"pt","channelTitle":"Canal GTA"}}]}`

func TestVideoAndUpdateText(t *testing.T) {
	h := newHarness(t, validToken)
	h.g.videoAnswers = []answer{
		{status: http.StatusOK, body: videoJSON},
		{status: http.StatusOK, body: `{"id":"vid1","snippet":{"title":"Ele pulou! #GTA6 #Shorts","description":"Insano\n\n#GTA6","tags":["GTA 6","alanzoka"],"categoryId":"20"}}`},
	}
	ctx := context.Background()
	cur, err := h.c.Video(ctx, "vid1")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Title != "Velho #GTA6" || cur.CategoryID != "20" || cur.DefaultAudioLanguage != "pt" || !slices.Equal(cur.Tags, []string{"GTA 6"}) {
		t.Fatalf("snippet = %+v", cur)
	}
	saved, err := h.c.UpdateText(ctx, "vid1", cur, meta)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Title != meta.Title {
		t.Errorf("saved = %+v", saved)
	}
	if !slices.Equal(h.g.videoCalls, []string{"GET id=vid1&part=snippet", "PUT part=snippet"}) {
		t.Errorf("calls = %q", h.g.videoCalls)
	}
	// The whole snippet goes back: the API deletes the fields left out.
	var sent struct {
		ID      string  `json:"id"`
		Snippet Snippet `json:"snippet"`
	}
	if err := json.Unmarshal(h.g.updateBody, &sent); err != nil {
		t.Fatal(err)
	}
	want := Snippet{Title: meta.Title, Description: meta.Description, Tags: meta.Tags, CategoryID: "20",
		DefaultLanguage: "pt-BR", DefaultAudioLanguage: "pt"}
	if sent.ID != "vid1" || sent.Snippet.Title != want.Title || sent.Snippet.Description != want.Description ||
		!slices.Equal(sent.Snippet.Tags, want.Tags) || sent.Snippet.CategoryID != want.CategoryID ||
		sent.Snippet.DefaultLanguage != want.DefaultLanguage || sent.Snippet.DefaultAudioLanguage != want.DefaultAudioLanguage {
		t.Errorf("sent = %+v\nwant %+v", sent, want)
	}
}

func TestUpdateTextFillsMissingFieldsAndRetries401(t *testing.T) {
	h := newHarness(t, validToken)
	h.g.tokenAnswers = []answer{{status: http.StatusOK, body: `{"access_token":"at-2","expires_in":3600}`}}
	h.g.videoAnswers = []answer{{status: http.StatusUnauthorized, body: `{}`}, {status: http.StatusOK, body: `{"snippet":{}}`}}
	if _, err := h.c.UpdateText(context.Background(), "vid1", Snippet{}, meta); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Snippet Snippet `json:"snippet"`
	}
	if err := json.Unmarshal(h.g.updateBody, &sent); err != nil {
		t.Fatal(err)
	}
	if s := sent.Snippet; s.CategoryID != "20" || s.DefaultLanguage != "pt-BR" || s.DefaultAudioLanguage != "pt" {
		t.Errorf("sent = %+v", s)
	}
	if len(h.g.videoCalls) != 2 {
		t.Errorf("calls = %q", h.g.videoCalls)
	}
}

func TestVideoErrors(t *testing.T) {
	h := newHarness(t, validToken)
	h.g.videoAnswers = []answer{{status: http.StatusOK, body: `{"items":[]}`}}
	if _, err := h.c.Video(context.Background(), "gone"); err == nil {
		t.Fatal("expected not found")
	}
	h.g.videoAnswers = []answer{{status: http.StatusForbidden,
		body: `{"error":{"message":"Request had insufficient authentication scopes.","errors":[{"reason":"insufficientPermissions"}]}}`}}
	_, err := h.c.UpdateText(context.Background(), "vid1", Snippet{}, meta)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Reason != "insufficientPermissions" {
		t.Fatalf("err = %v", err)
	}
}

func TestCanEdit(t *testing.T) {
	cases := map[string]bool{
		"": true, // not recorded: the API decides
		"https://www.googleapis.com/auth/youtube.readonly https://www.googleapis.com/auth/youtube.upload":  false,
		"https://www.googleapis.com/auth/youtube.upload https://www.googleapis.com/auth/youtube.force-ssl": true,
		"https://www.googleapis.com/auth/youtube":                                                          true,
	}
	for scope, want := range cases {
		tok := *validToken
		tok.Scope = scope
		h := newHarness(t, &tok)
		if got, err := h.c.CanEdit(); err != nil || got != want {
			t.Errorf("CanEdit(%q) = %v, %v; want %v", scope, got, err, want)
		}
	}
	if _, err := newHarness(t, nil).c.CanEdit(); !errors.Is(err, ErrNoToken) {
		t.Errorf("without token: %v", err)
	}
}
