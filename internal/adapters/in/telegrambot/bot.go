// Package telegrambot is the inbound Telegram adapter: it long-polls the Bot
// API, accepts commands and button callbacks only from allowlisted chats and
// calls the use cases. Idempotency comes from the clip state transitions,
// not from the bot.
package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// RetryFunc reprocesses a failed clip (/retry and the retry button).
type RetryFunc func(ctx context.Context, id domain.ClipID) error

// Bot handles updates.
type Bot struct {
	bot     *telego.Bot
	allowed map[int64]bool
	dec     *app.Decisions
	status  *app.Status
	retry   RetryFunc
	log     *slog.Logger
	now     func() time.Time
}

// Option configures the bot.
type Option func(*Bot)

// WithRetry enables /retry and the retry button.
func WithRetry(f RetryFunc) Option { return func(b *Bot) { b.retry = f } }

// New builds the bot adapter.
func New(bot *telego.Bot, allowed []int64, dec *app.Decisions, status *app.Status, log *slog.Logger, opts ...Option) *Bot {
	b := &Bot{bot: bot, allowed: map[int64]bool{}, dec: dec, status: status, log: log, now: func() time.Time { return time.Now().UTC() }}
	for _, id := range allowed {
		b.allowed[id] = true
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Run long-polls until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	updates, err := b.bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Timeout:        30,
		AllowedUpdates: []string{"message", "callback_query"},
	})
	if err != nil {
		return fmt.Errorf("telegram long polling: %w", err)
	}
	b.log.Info("telegram bot started", slog.Int("allowed_chats", len(b.allowed)))
	for u := range updates { // closed by telego when ctx is cancelled
		b.handle(ctx, u)
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, u telego.Update) {
	switch {
	case u.CallbackQuery != nil:
		b.handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	}
}

func (b *Bot) handleCallback(ctx context.Context, q *telego.CallbackQuery) {
	chat := q.From.ID
	if q.Message != nil {
		chat = q.Message.GetChat().ID
	}
	if !b.allowed[chat] {
		b.log.Warn("telegram callback from a chat outside the allowlist ignored", slog.Int64("chat_id", chat), slog.Int64("user_id", q.From.ID))
		return
	}
	text := b.dispatch(ctx, q.Data)
	if err := b.bot.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: truncate(text, 200)}); err != nil {
		b.log.Warn("answer callback", slog.String("err", b.redact(err)))
	}
}

// dispatch runs the action of a button and returns the notification text.
func (b *Bot) dispatch(ctx context.Context, data string) string {
	action, rawID, ok := strings.Cut(data, ":")
	id := domain.ClipID(rawID)
	if !ok || id == "" {
		return "Ação inválida"
	}
	var err error
	var done string
	switch action {
	case "a":
		err, done = b.dec.ApproveClip(ctx, id), "✅ Aprovado — gerando…"
	case "d":
		err, done = b.dec.DiscardClip(ctx, id), "🗑️ Descartado"
	case "r":
		if b.retry == nil {
			return "Retentativa indisponível"
		}
		err, done = b.retry(ctx, id), "🔁 Reprocessando…"
	default:
		return "Ação inválida"
	}
	if err != nil {
		b.log.Info("telegram action not applied", slog.String("clip_id", rawID), slog.String("action", action), slog.String("err", err.Error()))
		return userError(err)
	}
	return done
}

// userError maps use case errors to short messages.
func userError(err error) string {
	switch {
	case errors.Is(err, domain.ErrExpired):
		return "⌛ Expirado"
	case errors.Is(err, domain.ErrInvalidTransition):
		return "Já processado"
	case errors.Is(err, domain.ErrNotFound):
		return "Clipe não encontrado"
	default:
		return "Erro: " + err.Error()
	}
}

func (b *Bot) handleMessage(ctx context.Context, m *telego.Message) {
	chat := m.Chat.ID
	if !b.allowed[chat] {
		b.log.Warn("telegram message from a chat outside the allowlist ignored", slog.Int64("chat_id", chat))
		return
	}
	fields := strings.Fields(m.Text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return
	}
	cmd, _, _ := strings.Cut(strings.ToLower(fields[0]), "@") // "/status@clipper_bot"
	var reply string
	switch cmd {
	case "/status":
		reply = b.statusText(ctx)
	case "/pending":
		reply = b.pendingText(ctx)
	case "/retry":
		if len(fields) != 2 {
			reply = "Uso: /retry &lt;clip_id&gt;"
		} else {
			reply = b.dispatch(ctx, "r:"+fields[1])
		}
	default:
		reply = helpText
	}
	if _, err := b.bot.SendMessage(ctx, &telego.SendMessageParams{ChatID: telego.ChatID{ID: chat}, Text: truncate(reply, 4000), ParseMode: telego.ModeHTML}); err != nil {
		b.log.Warn("telegram reply", slog.String("err", b.redact(err)))
	}
}

const helpText = "🤖 <b>gta6-clipper</b>\n/status — lives, filas, último erro, custo do dia\n/pending — clipes aguardando aprovação\n/retry &lt;clip_id&gt; — reprocessa um clipe que falhou"

func (b *Bot) statusText(ctx context.Context) string {
	r, err := b.status.Report(ctx)
	if err != nil {
		return "Erro ao gerar o status: " + html.EscapeString(err.Error())
	}
	var s strings.Builder
	s.WriteString("📊 <b>Status</b>\n")
	fmt.Fprintf(&s, "Lives monitoradas: %d", len(r.Lives))
	for _, l := range r.Lives {
		fmt.Fprintf(&s, "\n• %s — %s (%d viewers)", html.EscapeString(l.Login), html.EscapeString(l.Game), l.Viewers)
	}
	if len(r.OffGame) > 0 {
		parts := make([]string, 0, len(r.OffGame))
		for _, l := range r.OffGame {
			game := l.Game
			if game == "" {
				game = "sem categoria"
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", html.EscapeString(l.Login), html.EscapeString(game)))
		}
		s.WriteString("\nAo vivo em outro jogo (não monitoradas): " + strings.Join(parts, " · "))
	}
	fmt.Fprintf(&s, "\nAguardando aprovação: %d\nFila de produção: %d\nCusto hoje: %s", r.Pending, r.Queue, r.CostToday)
	if len(r.ClipsToday) > 0 {
		states := make([]string, 0, len(r.ClipsToday))
		for st, n := range r.ClipsToday {
			states = append(states, fmt.Sprintf("%s %d", st, n))
		}
		sort.Strings(states)
		s.WriteString("\nClipes hoje: " + strings.Join(states, " · "))
	}
	if len(r.Strictness) > 0 {
		parts := make([]string, 0, len(r.Strictness))
		for login, off := range r.Strictness {
			parts = append(parts, fmt.Sprintf("%s +%.2f", html.EscapeString(login), off))
		}
		sort.Strings(parts)
		s.WriteString("\nExigência extra (aprendida dos seus descartes): " + strings.Join(parts, " · "))
	}
	if r.LastError != "" {
		fmt.Fprintf(&s, "\nÚltimo erro (há %s): %s", ago(b.now().Sub(r.LastErrorAt)), html.EscapeString(truncate(r.LastError, 500)))
	} else {
		s.WriteString("\nÚltimo erro: nenhum")
	}
	return s.String()
}

func (b *Bot) pendingText(ctx context.Context) string {
	pending, err := b.status.Pending(ctx)
	if err != nil {
		return "Erro: " + html.EscapeString(err.Error())
	}
	if len(pending) == 0 {
		return "Nenhum clipe aguardando aprovação."
	}
	var s strings.Builder
	fmt.Fprintf(&s, "⏳ <b>Aguardando aprovação (%d)</b>", len(pending))
	for _, p := range pending {
		fmt.Fprintf(&s, "\n• %s — %s — score %.1f — expira em %s\n  <code>%s</code>", html.EscapeString(p.Clip.Streamer),
			html.EscapeString(p.Clip.Game), p.Clip.Moment.Score, ago(p.Deadline.Sub(b.now())), p.Clip.ID)
	}
	return s.String()
}

// redact removes the bot token from an error message (net/http errors carry the URL).
func (b *Bot) redact(err error) string {
	if tok := b.bot.Token(); tok != "" {
		return strings.ReplaceAll(err.Error(), tok, "<redacted>")
	}
	return err.Error()
}

// ago renders a duration as "2h05min" / "7min".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dmin", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dmin", int(d/time.Minute))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
