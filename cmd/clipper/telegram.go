package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mymmrac/telego"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/telegram"
	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
	"github.com/henriquecpp/gta6-clipper/internal/platform/config"
)

// telegramParts are the Telegram pieces shared by the in and out adapters.
type telegramParts struct {
	bot      *telego.Bot // nil when Telegram is not configured
	notifier app.Notifier
}

// newTelegram builds the shared bot, or a disabled notifier (degraded mode,
// ADR 0016) when the token or the chat allowlist is missing.
func newTelegram(cfg *config.Config, log *slog.Logger) (telegramParts, error) {
	if !cfg.Secrets.HasTelegram() {
		log.Warn("TELEGRAM_BOT_TOKEN/TELEGRAM_ALLOWED_CHAT_IDS missing: previews cannot be delivered (degraded mode)")
		return telegramParts{notifier: disabledNotifier{}}, nil
	}
	token := cfg.Secrets.TelegramBotToken
	bot, err := telego.NewBot(token, telego.WithLogger(telegoLogger{log: log, token: token}))
	if err != nil {
		return telegramParts{}, fmt.Errorf("telegram bot: %w", errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>")))
	}
	return telegramParts{
		bot:      bot,
		notifier: telegram.NewNotifier(bot, cfg.Secrets.TelegramAllowedChatIDs, cfg.Approval.TTL.D(), log),
	}, nil
}

// telegoLogger routes telego's logs to slog without the bot token.
type telegoLogger struct {
	log   *slog.Logger
	token string
}

func (l telegoLogger) Debugf(string, ...any) {}

func (l telegoLogger) Errorf(format string, args ...any) {
	l.log.Warn("telego", slog.String("err", strings.ReplaceAll(fmt.Sprintf(format, args...), l.token, "<redacted>")))
}

// errTelegramDisabled is the failure reason of previews without Telegram.
var errTelegramDisabled error = notConfigured("telegram is not configured (TELEGRAM_BOT_TOKEN / TELEGRAM_ALLOWED_CHAT_IDS)")

// notConfigured is the error of a port disabled in degraded mode (ADR 0016).
type notConfigured string

func (e notConfigured) Error() string { return string(e) }

// Temporary tells the resilience decorators that retrying cannot help.
func (notConfigured) Temporary() bool { return false }

// disabledNotifier keeps the pipeline running without Telegram.
type disabledNotifier struct{}

func (disabledNotifier) SendPreview(context.Context, *domain.Clip, domain.MediaRef) (domain.MessageRef, error) {
	return "", errTelegramDisabled
}
func (disabledNotifier) UpdatePreview(context.Context, *domain.Clip) error       { return nil }
func (disabledNotifier) SendFailure(context.Context, *domain.Clip, string) error { return nil }
func (disabledNotifier) SendFinal(context.Context, *domain.Clip, domain.MediaRef, domain.Post) error {
	return errTelegramDisabled
}
func (disabledNotifier) SendAlert(context.Context, *domain.Clip, string) error { return nil }
