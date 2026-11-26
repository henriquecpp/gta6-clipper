package telegram

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/telegramtest"
)

func testClip() *domain.Clip {
	return &domain.Clip{
		ID: "0193f1c2-aaaa-7bbb-8ccc-123456789abc", Streamer: "streamer<x>", Game: "GTA VI & Co",
		State: domain.StatePreviewSent, LiveOffset: time.Hour + 23*time.Minute + 45*time.Second,
		Moment: domain.Moment{Score: 4.24, ViewerClips: 2, Values: map[domain.SignalKind]float64{domain.SignalVoice: 3.8, domain.SignalChat: 5.1}},
	}
}

func TestPreviewCaption(t *testing.T) {
	c := testClip()
	got := previewCaption(c, 6*time.Hour)
	want := "🎬 <b>streamer&lt;x&gt;</b> — GTA VI &amp; Co\nScore: 4.2 (voz 3.8 · chat 5.1 · clipes 2)\nPico: 01:23:45 da live\nExpira em 6h"
	if got != want {
		t.Fatalf("caption:\n%s\nwant:\n%s", got, want)
	}
	c.State = domain.StateDiscarded
	if got := previewCaption(c, 6*time.Hour); strings.Contains(got, "Expira") || !strings.HasSuffix(got, "🗑️ Descartado") {
		t.Fatalf("discarded caption = %q", got)
	}
	c.State, c.FailureReason, c.Moment.Late, c.Game = domain.StateFailed, "nvenc <broken>", true, ""
	got = previewCaption(c, 90*time.Minute)
	if !strings.Contains(got, "⚠️ Falhou: nvenc &lt;broken&gt;") || !strings.Contains(got, "tardio") || !strings.Contains(got, "sem categoria") {
		t.Fatalf("failed caption = %q", got)
	}
	for st, line := range map[domain.ClipState]string{
		domain.StateApproved: "✅ Aprovado — gerando…", domain.StateEditing: "✅ Aprovado — gerando…",
		domain.StateDelivered: "✅ Entregue", domain.StateExpired: "⌛ Expirado", domain.StatePreviewSent: "",
	} {
		c.State = st
		if statusLine(c) != line {
			t.Errorf("status(%s) = %q", st, statusLine(c))
		}
	}
	if humanDuration(90*time.Minute) != "90min" || hms(-time.Second) != "00:00:00" {
		t.Fatal("helpers")
	}
	if got := truncate(strings.Repeat("é", 2000), 1024); len([]rune(got)) != 1024 {
		t.Fatalf("truncate kept %d runes", len([]rune(got)))
	}
}

func TestKeyboardCallbackData(t *testing.T) {
	c := testClip()
	kb := keyboard(c)
	if len(kb.InlineKeyboard) != 1 || kb.InlineKeyboard[0][0].CallbackData != "a:"+string(c.ID) || kb.InlineKeyboard[0][1].CallbackData != "d:"+string(c.ID) {
		t.Fatalf("keyboard = %+v", kb)
	}
	for _, b := range kb.InlineKeyboard[0] {
		if len(b.CallbackData) > 64 {
			t.Fatalf("callback_data has %d bytes (> 64)", len(b.CallbackData))
		}
	}
	c.State = domain.StateFailed
	if keyboard(c).InlineKeyboard[0][0].CallbackData != "r:"+string(c.ID) {
		t.Fatal("retry button")
	}
	c.State = domain.StateDelivered
	if len(keyboard(c).InlineKeyboard) != 0 {
		t.Fatal("the preview of a delivered clip has no buttons")
	}
}

func TestRefsRoundTrip(t *testing.T) {
	refs := []msgRef{{chat: 123, msg: 4}, {chat: -1001, msg: 5}}
	enc := encodeRefs(refs)
	if enc != "123:4,-1001:5" {
		t.Fatalf("encoded = %q", enc)
	}
	got, err := decodeRefs(enc)
	if err != nil || len(got) != 2 || got[1] != refs[1] {
		t.Fatalf("decoded = %v, %v", got, err)
	}
	if r, err := decodeRefs(""); err != nil || r != nil {
		t.Fatal("empty ref")
	}
	if _, err := decodeRefs("x:1"); err == nil {
		t.Fatal("expected error")
	}
}

func newTestBot(t *testing.T, srv *telegramtest.Server) *telego.Bot {
	t.Helper()
	bot, err := telego.NewBot(telegramtest.Token, telego.WithAPIServer(srv.URL), telego.WithHTTPClient(srv.Client()), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestNotifierAgainstFakeBotAPI(t *testing.T) {
	srv := telegramtest.New()
	defer srv.Close()
	n := NewNotifier(newTestBot(t, srv), []int64{111, 222}, 6*time.Hour, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	preview := filepath.Join(t.TempDir(), "p.mp4")
	if err := os.WriteFile(preview, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testClip()
	c.State = domain.StateDetected
	ref, err := n.SendPreview(ctx, c, domain.MediaRef(preview))
	if err != nil {
		t.Fatal(err)
	}
	sends := srv.Calls("sendVideo")
	if len(sends) != 2 || sends[0].Params["chat_id"] != "111" || sends[0].FileBytes != 2048 || sends[0].Params["parse_mode"] != "HTML" ||
		!strings.Contains(sends[0].Params["reply_markup"], "a:"+string(c.ID)) || !strings.Contains(sends[0].Params["caption"], "Expira em 6h") {
		t.Fatalf("sendVideo calls = %+v", sends)
	}
	refs, _ := decodeRefs(ref)
	if len(refs) != 2 || refs[0].chat != 111 || refs[1].chat != 222 {
		t.Fatalf("refs = %v", refs)
	}

	c.PreviewMessage = ref
	c.State = domain.StateApproved
	if err := n.UpdatePreview(ctx, c); err != nil {
		t.Fatal(err)
	}
	edits := srv.Calls("editMessageCaption")
	if len(edits) != 2 || !strings.Contains(edits[0].Params["caption"], "Aprovado") || !strings.Contains(edits[0].Params["reply_markup"], `"inline_keyboard":[]`) {
		t.Fatalf("edits = %+v", edits)
	}

	c.State, c.FailedFrom = domain.StateFailed, domain.StateEditing
	if err := n.SendFailure(ctx, c, "claude timeout"); err != nil {
		t.Fatal(err)
	}
	if msgs := srv.Calls("sendMessage"); len(msgs) != 2 || !strings.Contains(msgs[0].Params["text"], "claude timeout") || !strings.Contains(msgs[0].Params["reply_markup"], "r:") {
		t.Fatalf("failure alerts = %+v", msgs)
	}

	// All chats failing → error; missing file → error.
	srv.Fail("sendVideo", "Bad Request: file too large")
	if _, err := n.SendPreview(ctx, c, domain.MediaRef(preview)); err == nil {
		t.Fatal("expected error when no chat receives the preview")
	}
	if _, err := n.SendPreview(ctx, c, "/nope.mp4"); err == nil {
		t.Fatal("expected error for missing file")
	}
	srv.Fail("sendMessage", "Forbidden")
	if err := n.SendFailure(ctx, c, "x"); err == nil {
		t.Fatal("expected error when no chat receives the alert")
	}
	c.PreviewMessage = "garbage"
	if err := n.UpdatePreview(ctx, c); err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func TestRedactToken(t *testing.T) {
	srv := telegramtest.New()
	defer srv.Close()
	n := NewNotifier(newTestBot(t, srv), nil, time.Hour, slog.New(slog.DiscardHandler))
	err := n.redact(errors.New(`Post "https://api.telegram.org/bot` + telegramtest.Token + `/sendVideo": dial tcp: timeout`))
	if strings.Contains(err.Error(), telegramtest.Token) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("token not redacted: %v", err)
	}
	plain := errors.New("x")
	if !errors.Is(n.redact(plain), plain) || n.redact(nil) != nil {
		t.Fatal("redact must keep unrelated errors")
	}
}

func TestSendFinalAndAlert(t *testing.T) {
	srv := telegramtest.New()
	defer srv.Close()
	n := NewNotifier(newTestBot(t, srv), []int64{111}, time.Hour, slog.New(slog.DiscardHandler))
	video := filepath.Join(t.TempDir(), "final.mp4")
	if err := os.WriteFile(video, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testClip()
	post := domain.Post{Headline: "Ele pulou!", Text: "Insano #gta6 streamer<x>"}
	ref, err := n.SendFinal(context.Background(), c, domain.MediaRef(video), post)
	if err != nil || ref == "" {
		t.Fatalf("send final = %q, %v", ref, err)
	}
	vids, msgs := srv.Calls("sendVideo"), srv.Calls("sendMessage")
	if len(vids) != 1 || vids[0].FileBytes != 4096 || !strings.Contains(vids[0].Params["caption"], "Ele pulou!") ||
		!strings.Contains(vids[0].Params["caption"], "Nada foi publicado.") || vids[0].Params["reply_markup"] != "" {
		t.Fatalf("video = %+v", vids)
	}
	if len(msgs) != 1 || msgs[0].Params["text"] != post.Text || msgs[0].Params["parse_mode"] != "" {
		t.Fatalf("post text = %+v", msgs)
	}
	if err := n.SendAlert(context.Background(), c, "custo > limite"); err != nil {
		t.Fatal(err)
	}
	if a := srv.Calls("sendMessage"); len(a) != 2 || !strings.Contains(a[1].Params["text"], "custo &gt; limite") ||
		!strings.Contains(a[1].Params["text"], string(c.ID)) {
		t.Fatalf("alert = %+v", a)
	}
	// System alerts have no clip.
	if err := n.SendAlert(context.Background(), nil, "GPU indisponível"); err != nil {
		t.Fatal(err)
	}
	if a := srv.Calls("sendMessage"); len(a) != 3 || a[2].Params["text"] != "GPU indisponível" {
		t.Fatalf("system alert = %+v", a[len(a)-1])
	}
	srv.Fail("sendVideo", "Request Entity Too Large")
	if _, err := n.SendFinal(context.Background(), c, domain.MediaRef(video), post); err == nil {
		t.Fatal("expected error")
	}
	srv.Fail("sendMessage", "Forbidden")
	if err := n.SendAlert(context.Background(), c, "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFinalMessageWithPublishing(t *testing.T) {
	srv := telegramtest.New()
	defer srv.Close()
	n := NewNotifier(newTestBot(t, srv), []int64{111, 222}, time.Hour, slog.New(slog.DiscardHandler), WithPublishing())
	ctx := context.Background()
	video := filepath.Join(t.TempDir(), "final.mp4")
	if err := os.WriteFile(video, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	c := testClip()
	c.State, c.Headline = domain.StateRendered, "Ele pulou!"
	ref, err := n.SendFinal(ctx, c, domain.MediaRef(video), domain.Post{Headline: c.Headline, Text: "texto"})
	if err != nil {
		t.Fatal(err)
	}
	vids := srv.Calls("sendVideo")
	if len(vids) != 2 || !strings.Contains(vids[0].Params["reply_markup"], `"callback_data":"p:`+string(c.ID)+`"`) ||
		!strings.Contains(vids[0].Params["caption"], "toque em 📤") {
		t.Fatalf("final video = %+v", vids)
	}
	if refs, _ := decodeRefs(ref); len(refs) != 2 {
		t.Fatalf("refs = %q", ref)
	}
	c.FinalMessage = ref

	edit := func(state domain.ClipState) map[string]string {
		t.Helper()
		c.State = state
		before := len(srv.Calls("editMessageCaption"))
		if err := n.UpdateFinal(ctx, c); err != nil {
			t.Fatal(err)
		}
		edits := srv.Calls("editMessageCaption")
		if len(edits) != before+2 {
			t.Fatalf("%s: %d edits", state, len(edits)-before)
		}
		return edits[len(edits)-1].Params
	}
	if e := edit(domain.StatePublishing); !strings.Contains(e["caption"], "Enviando para o YouTube") ||
		!strings.Contains(e["reply_markup"], `"inline_keyboard":[]`) {
		t.Fatalf("publishing edit = %+v", e)
	}
	c.PublishError = "quota <exceeded>"
	if e := edit(domain.StateDelivered); !strings.Contains(e["caption"], "Falha ao publicar: quota &lt;exceeded&gt;") ||
		!strings.Contains(e["reply_markup"], "p:"+string(c.ID)) {
		t.Fatalf("failed edit = %+v", e)
	}
	c.PublishError, c.PublishedURL = "", "https://www.youtube.com/shorts/abc"
	if e := edit(domain.StatePublished); !strings.Contains(e["caption"], "Publicado no YouTube: https://www.youtube.com/shorts/abc") ||
		!strings.Contains(e["reply_markup"], `"url":"https://www.youtube.com/shorts/abc"`) {
		t.Fatalf("published edit = %+v", e)
	}
	if len(CallbackPublish+string(c.ID)) > 64 {
		t.Fatal("publish callback_data above 64 bytes")
	}
	c.FinalMessage = "garbage"
	if err := n.UpdateFinal(ctx, c); err == nil {
		t.Fatal("expected error for invalid ref")
	}
}
