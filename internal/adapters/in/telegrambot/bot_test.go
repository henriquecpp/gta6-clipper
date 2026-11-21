package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/app/contracttest"
	"github.com/henriquecpp/gta6-clipper/internal/app/fakes"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/telegramtest"
)

var now = time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)

const allowedChat = int64(111)

type env struct {
	srv     *telegramtest.Server
	repo    *fakes.ClipRepo
	bot     *Bot
	retried []domain.ClipID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{srv: telegramtest.New(), repo: fakes.NewClipRepo()}
	t.Cleanup(e.srv.Close)
	tb, err := telego.NewBot(telegramtest.Token, telego.WithAPIServer(e.srv.URL), telego.WithHTTPClient(e.srv.Client()), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	clock := fakes.NewClock(now)
	log := slog.New(slog.DiscardHandler)
	dec := app.NewDecisions(e.repo, fakes.NewMediaCapture(), &fakes.Notifier{}, clock, log, 6*time.Hour)
	errs := &app.ErrorLog{}
	errs.Record(errors.New("whisper <down>"), now.Add(-5*time.Minute))
	st := app.NewStatus(e.repo, clock, 6*time.Hour, func() []domain.LiveStream {
		return []domain.LiveStream{{Login: "alice", Game: "GTA VI", Viewers: 1200}}
	}, nil, errs)
	st.SetStrictness(func() map[string]float64 { return map[string]float64{"kaicenat": 0.75, "xqc": 0.25} })
	st.SetOffGame(func() []domain.LiveStream {
		return []domain.LiveStream{{Login: "bob", Game: "Just Chatting"}, {Login: "carol"}}
	})
	e.bot = New(tb, []int64{allowedChat}, dec, st, log, WithRetry(func(_ context.Context, id domain.ClipID) error {
		e.retried = append(e.retried, id)
		return nil
	}))
	e.bot.now = func() time.Time { return now }
	return e
}

func (e *env) pending(t *testing.T, id domain.ClipID) {
	t.Helper()
	c := contracttest.FullClip(id, now.Add(-time.Hour))
	if err := c.Transition(domain.StatePreviewSent, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func callback(id string, chat int64, data string) map[string]any {
	return map[string]any{"callback_query": map[string]any{
		"id": id, "from": map[string]any{"id": chat, "is_bot": false, "first_name": "me"}, "data": data,
		"message": map[string]any{"message_id": 5, "date": 1, "chat": map[string]any{"id": chat, "type": "private"}},
	}}
}

func message(chat int64, text string) map[string]any {
	return map[string]any{"message": map[string]any{"message_id": 9, "date": 1, "text": text,
		"chat": map[string]any{"id": chat, "type": "private"}, "from": map[string]any{"id": chat, "is_bot": false, "first_name": "me"}}}
}

// runUntil runs the bot until cond holds (or times out) and stops it.
func (e *env) runUntil(t *testing.T, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.bot.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bot did not stop")
	}
}

func answers(srv *telegramtest.Server) []string {
	var out []string
	for _, c := range srv.Calls("answerCallbackQuery") {
		out = append(out, c.Params["callback_query_id"]+"="+c.Params["text"])
	}
	return out
}

func TestCallbacksAreIdempotentAndAllowlisted(t *testing.T) {
	e := newEnv(t)
	e.pending(t, "c1")
	e.pending(t, "c2")
	e.srv.PushUpdate(callback("q1", allowedChat, "a:c1"))
	e.srv.PushUpdate(callback("q2", allowedChat, "a:c1")) // double click
	e.srv.PushUpdate(callback("q3", 999, "d:c2"))         // stranger: ignored
	e.srv.PushUpdate(callback("q4", allowedChat, "d:c2"))
	e.srv.PushUpdate(callback("q5", allowedChat, "x:c2"))
	e.srv.PushUpdate(callback("q6", allowedChat, "a:missing"))
	e.srv.PushUpdate(callback("q7", allowedChat, "r:c9"))
	e.runUntil(t, func() bool { return len(e.srv.Calls("answerCallbackQuery")) >= 6 })

	want := []string{"q1=✅ Aprovado — gerando…", "q2=Já processado", "q4=🗑️ Descartado", "q5=Ação inválida", "q6=Clipe não encontrado", "q7=🔁 Reprocessando…"}
	if got := answers(e.srv); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("answers = %v\nwant %v", got, want)
	}
	c1, _ := e.repo.Get(context.Background(), "c1")
	c2, _ := e.repo.Get(context.Background(), "c2")
	if c1.State != domain.StateApproved || c2.State != domain.StateDiscarded || len(e.retried) != 1 {
		t.Fatalf("c1=%s c2=%s retried=%v", c1.State, c2.State, e.retried)
	}
}

func TestCommands(t *testing.T) {
	e := newEnv(t)
	e.pending(t, "c1")
	e.srv.PushUpdate(message(allowedChat, "/status@clipper_bot"))
	e.srv.PushUpdate(message(allowedChat, "/pending"))
	e.srv.PushUpdate(message(allowedChat, "/retry c7"))
	e.srv.PushUpdate(message(allowedChat, "/retry"))
	e.srv.PushUpdate(message(allowedChat, "/help"))
	e.srv.PushUpdate(message(allowedChat, "hello")) // not a command: no reply
	e.srv.PushUpdate(message(999, "/status"))       // stranger: ignored
	e.runUntil(t, func() bool { return len(e.srv.Calls("sendMessage")) >= 5 })

	msgs := e.srv.Calls("sendMessage")
	if len(msgs) != 5 {
		t.Fatalf("replies = %d", len(msgs))
	}
	status := msgs[0].Params["text"]
	for _, want := range []string{"Lives monitoradas: 1", "alice — GTA VI (1200 viewers)", "Aguardando aprovação: 1", "Custo hoje: $0.000000", "há 5min", "whisper &lt;down&gt;",
		"Exigência extra (aprendida dos seus descartes): kaicenat +0.75 · xqc +0.25",
		"Ao vivo em outro jogo (não monitoradas): bob (Just Chatting) · carol (sem categoria)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	if p := msgs[1].Params["text"]; !strings.Contains(p, "Aguardando aprovação (1)") || !strings.Contains(p, "<code>c1</code>") || !strings.Contains(p, "expira em 5h00min") {
		t.Errorf("pending = %s", p)
	}
	if msgs[2].Params["text"] != "🔁 Reprocessando…" || len(e.retried) != 1 || e.retried[0] != "c7" {
		t.Errorf("retry reply = %q retried=%v", msgs[2].Params["text"], e.retried)
	}
	if !strings.Contains(msgs[3].Params["text"], "Uso: /retry") || !strings.Contains(msgs[4].Params["text"], "/pending") {
		t.Errorf("usage/help = %q / %q", msgs[3].Params["text"], msgs[4].Params["text"])
	}
	for _, m := range msgs {
		if m.Params["chat_id"] != "111" || m.Params["parse_mode"] != "HTML" {
			t.Fatalf("reply = %+v", m)
		}
	}
}

func TestHelpers(t *testing.T) {
	if ago(-time.Minute) != "0min" || ago(125*time.Minute) != "2h05min" {
		t.Fatal("ago")
	}
	if userError(errors.New("x")) != "Erro: x" {
		t.Fatal("userError")
	}
	e := newEnv(t)
	e.bot.retry = nil
	if e.bot.dispatch(context.Background(), "r:x") != "Retentativa indisponível" || e.bot.dispatch(context.Background(), "a:") != "Ação inválida" {
		t.Fatal("dispatch edge cases")
	}
	if !strings.Contains(e.bot.pendingText(context.Background()), "Nenhum clipe") {
		t.Fatal("empty pending")
	}
}

func TestBotRedactsToken(t *testing.T) {
	e := newEnv(t)
	got := e.bot.redact(errors.New("Post https://x/bot" + telegramtest.Token + "/sendMessage: EOF"))
	if strings.Contains(got, telegramtest.Token) {
		t.Fatalf("token leaked: %s", got)
	}
}
