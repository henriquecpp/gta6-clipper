package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Notifier sends previews and alerts to every allowed chat.
type Notifier struct {
	bot   *telego.Bot
	chats []int64
	ttl   time.Duration
	log   *slog.Logger
	// publish shows the "publish on YouTube" button on final videos.
	publish bool
}

var _ app.Notifier = (*Notifier)(nil)

// Option configures the notifier.
type Option func(*Notifier)

// WithPublishing shows the "publish on YouTube" button on final videos.
func WithPublishing() Option { return func(n *Notifier) { n.publish = true } }

// NewNotifier builds the notifier. The bot is shared with the inbound bot adapter.
func NewNotifier(bot *telego.Bot, chats []int64, ttl time.Duration, log *slog.Logger, opts ...Option) *Notifier {
	n := &Notifier{bot: bot, chats: chats, ttl: ttl, log: log}
	for _, o := range opts {
		o(n)
	}
	return n
}

// redact removes the bot token from errors: transport errors from net/http
// include the request URL, which embeds the token.
func (n *Notifier) redact(err error) error {
	if err == nil {
		return nil
	}
	tok := n.bot.Token()
	if tok == "" || !strings.Contains(err.Error(), tok) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), tok, "<redacted>"))
}

// keyboard returns the inline buttons for the clip state (empty = remove).
func keyboard(c *domain.Clip) *telego.InlineKeyboardMarkup {
	id := string(c.ID)
	switch c.State {
	case domain.StateDetected, domain.StatePreviewSent:
		return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{{
			{Text: "✅ Aprovar", CallbackData: CallbackApprove + id},
			{Text: "🗑️ Descartar", CallbackData: CallbackDiscard + id},
		}}}
	case domain.StateFailed:
		return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{{
			{Text: "🔁 Tentar de novo", CallbackData: CallbackRetry + id},
		}}}
	}
	return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}}
}

// finalKeyboard returns the button of the final video message (nil = none):
// publish while delivered, a link once published.
func finalKeyboard(c *domain.Clip, publish bool) *telego.InlineKeyboardMarkup {
	switch {
	case c.State == domain.StatePublished && c.PublishedURL != "":
		return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{{
			{Text: "▶️ Ver no YouTube", URL: c.PublishedURL},
		}}}
	case publish && (c.State == domain.StateRendered || c.State == domain.StateDelivered):
		return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{{
			{Text: "📤 Publicar no YouTube", CallbackData: CallbackPublish + string(c.ID)},
		}}}
	}
	return nil
}

// SendPreview uploads the preview to every chat. It fails only if no chat
// received it.
func (n *Notifier) SendPreview(ctx context.Context, c *domain.Clip, preview domain.MediaRef) (domain.MessageRef, error) {
	view := c.Clone()
	view.State = domain.StatePreviewSent // the buttons and "expires in" of a pending preview
	var refs []msgRef
	var errs []error
	for _, chat := range n.chats {
		msg, err := n.sendVideo(ctx, chat, string(preview), previewCaption(view, n.ttl), keyboard(view))
		if err != nil {
			errs = append(errs, fmt.Errorf("chat %d: %w", chat, n.redact(err)))
			continue
		}
		refs = append(refs, msgRef{chat: chat, msg: msg})
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("send preview: %w", errors.Join(errs...))
	}
	if len(errs) > 0 {
		n.log.Warn("preview not delivered to every chat", slog.String("clip_id", string(c.ID)), slog.String("err", errors.Join(errs...).Error()))
	}
	return encodeRefs(refs), nil
}

func (n *Notifier) sendVideo(ctx context.Context, chat int64, path, caption string, kb *telego.InlineKeyboardMarkup) (int, error) {
	var markup telego.ReplyMarkup
	if kb != nil {
		markup = kb
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only
	msg, err := n.bot.SendVideo(ctx, &telego.SendVideoParams{
		ChatID:            telego.ChatID{ID: chat},
		Video:             telego.InputFile{File: f},
		Caption:           caption,
		ParseMode:         telego.ModeHTML,
		SupportsStreaming: true,
		ReplyMarkup:       markup,
	})
	if err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// UpdatePreview edits the caption and buttons of the preview message(s).
func (n *Notifier) UpdatePreview(ctx context.Context, c *domain.Clip) error {
	refs, err := decodeRefs(c.PreviewMessage)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range refs {
		_, err := n.bot.EditMessageCaption(ctx, &telego.EditMessageCaptionParams{
			ChatID:      telego.ChatID{ID: r.chat},
			MessageID:   r.msg,
			Caption:     previewCaption(c, n.ttl),
			ParseMode:   telego.ModeHTML,
			ReplyMarkup: keyboard(c),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("edit message %d in chat %d: %w", r.msg, r.chat, n.redact(err)))
		}
	}
	return errors.Join(errs...)
}

// SendFailure alerts every chat, with a retry button.
func (n *Notifier) SendFailure(ctx context.Context, c *domain.Clip, reason string) error {
	var errs []error
	for _, chat := range n.chats {
		_, err := n.bot.SendMessage(ctx, &telego.SendMessageParams{
			ChatID:      telego.ChatID{ID: chat},
			Text:        failureText(c, reason),
			ParseMode:   telego.ModeHTML,
			ReplyMarkup: keyboard(c),
		})
		if err != nil {
			errs = append(errs, n.redact(err))
		}
	}
	if len(errs) > 0 && len(errs) == len(n.chats) {
		return fmt.Errorf("send failure alert: %w", errors.Join(errs...))
	}
	return nil
}

// SendFinal sends the final video (headline as caption, with the publish
// button when publishing is enabled) and then the post text as a plain
// message, easy to copy. Nothing is published until the button is tapped.
func (n *Notifier) SendFinal(ctx context.Context, c *domain.Clip, video domain.MediaRef, post domain.Post) (domain.MessageRef, error) {
	view := c.Clone()
	view.State = domain.StateDelivered // the caption and button once delivered
	var refs []msgRef
	var errs []error
	for _, chat := range n.chats {
		msg, err := n.sendVideo(ctx, chat, string(video), finalCaption(view, post.Headline, n.publish), finalKeyboard(view, n.publish))
		if err != nil {
			errs = append(errs, fmt.Errorf("chat %d: %w", chat, n.redact(err)))
			continue
		}
		refs = append(refs, msgRef{chat: chat, msg: msg})
		if _, err := n.bot.SendMessage(ctx, &telego.SendMessageParams{ChatID: telego.ChatID{ID: chat}, Text: truncate(post.Text, 4000)}); err != nil {
			errs = append(errs, fmt.Errorf("chat %d post text: %w", chat, n.redact(err)))
		}
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("send final video: %w", errors.Join(errs...))
	}
	if len(errs) > 0 {
		n.log.Warn("final video not fully delivered", slog.String("clip_id", string(c.ID)), slog.String("err", errors.Join(errs...).Error()))
	}
	return encodeRefs(refs), nil
}

// UpdateFinal edits the caption and button of the final video message(s) as
// the publication progresses.
func (n *Notifier) UpdateFinal(ctx context.Context, c *domain.Clip) error {
	refs, err := decodeRefs(c.FinalMessage)
	if err != nil {
		return err
	}
	kb := finalKeyboard(c, n.publish)
	if kb == nil {
		kb = &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}}
	}
	var errs []error
	for _, r := range refs {
		_, err := n.bot.EditMessageCaption(ctx, &telego.EditMessageCaptionParams{
			ChatID:      telego.ChatID{ID: r.chat},
			MessageID:   r.msg,
			Caption:     finalCaption(c, c.Headline, n.publish),
			ParseMode:   telego.ModeHTML,
			ReplyMarkup: kb,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("edit message %d in chat %d: %w", r.msg, r.chat, n.redact(err)))
		}
	}
	return errors.Join(errs...)
}

// SendAlert sends an informational message about a clip.
func (n *Notifier) SendAlert(ctx context.Context, c *domain.Clip, text string) error {
	var errs []error
	for _, chat := range n.chats {
		_, err := n.bot.SendMessage(ctx, &telego.SendMessageParams{
			ChatID: telego.ChatID{ID: chat}, ParseMode: telego.ModeHTML,
			Text: truncate(fmt.Sprintf("%s\nClipe: <code>%s</code> (%s)", esc(text), esc(string(c.ID)), esc(c.Streamer)), 4000),
		})
		if err != nil {
			errs = append(errs, n.redact(err))
		}
	}
	if len(errs) > 0 && len(errs) == len(n.chats) {
		return fmt.Errorf("send alert: %w", errors.Join(errs...))
	}
	return nil
}
