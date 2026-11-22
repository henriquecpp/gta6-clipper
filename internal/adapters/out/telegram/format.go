// Package telegram implements the Notifier port with telego: previews with
// inline Approve/Discard buttons, edits of the preview message as the clip
// state changes, failure alerts and (Phase 5) the final video delivery.
package telegram

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// Callback data prefixes (callback_data is limited to 64 bytes; a UUID is 36).
const (
	CallbackApprove = "a:"
	CallbackDiscard = "d:"
	CallbackRetry   = "r:"
	CallbackPublish = "p:"
)

// maxCaption is the Telegram caption limit (characters after entity parsing).
const maxCaption = 1024

func esc(s string) string { return html.EscapeString(s) }

// hms formats a duration as HH:MM:SS.
func hms(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second))
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// humanDuration renders a TTL like "6h" or "90min".
func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dmin", int(d.Round(time.Minute)/time.Minute))
}

// previewCaption is the caption of the preview message (CLAUDE.md §13).
func previewCaption(c *domain.Clip, ttl time.Duration) string {
	var b strings.Builder
	game := c.Game
	if game == "" {
		game = "sem categoria"
	}
	fmt.Fprintf(&b, "🎬 <b>%s</b> — %s\n", esc(c.Streamer), esc(game))
	v := c.Moment.Values
	fmt.Fprintf(&b, "Score: %s (voz %s · chat %s · clipes %d)", num(c.Moment.Score),
		num(v[domain.SignalVoice]), num(v[domain.SignalChat]), c.Moment.ViewerClips)
	if c.Moment.Late {
		b.WriteString(" · tardio")
	}
	fmt.Fprintf(&b, "\nPico: %s da live", hms(c.LiveOffset))
	if c.State == domain.StatePreviewSent || c.State == domain.StateDetected {
		fmt.Fprintf(&b, "\nExpira em %s", humanDuration(ttl))
	}
	if line := statusLine(c); line != "" {
		b.WriteString("\n\n" + line)
	}
	return truncate(b.String(), maxCaption)
}

// statusLine describes the state after a decision.
func statusLine(c *domain.Clip) string {
	switch c.State {
	case domain.StateApproved, domain.StateEditing, domain.StateRendered:
		return "✅ Aprovado — gerando…"
	case domain.StateDelivered, domain.StatePublishing:
		return "✅ Entregue"
	case domain.StatePublished:
		return "✅ Entregue e publicado no YouTube"
	case domain.StateDiscarded:
		return "🗑️ Descartado"
	case domain.StateExpired:
		return "⌛ Expirado"
	case domain.StateFailed:
		return "⚠️ Falhou: " + esc(truncate(c.FailureReason, 300))
	}
	return ""
}

// finalCaption is the caption of the delivered video, with the publication
// status when publishing is enabled.
func finalCaption(c *domain.Clip, headline string, publish bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ <b>%s</b>\n%s — %s\nTexto do post na próxima mensagem.", esc(headline), esc(c.Streamer), esc(c.Game))
	switch {
	case c.State == domain.StatePublished:
		fmt.Fprintf(&b, "\n\n▶️ Publicado no YouTube: %s", esc(c.PublishedURL))
	case c.State == domain.StatePublishing:
		b.WriteString("\n\n⏫ Enviando para o YouTube…")
	case !publish:
		b.WriteString(" Nada foi publicado.")
	default:
		if c.PublishError != "" {
			fmt.Fprintf(&b, "\n\n⚠️ Falha ao publicar: %s", esc(truncate(c.PublishError, 300)))
		}
		b.WriteString("\n\nNada foi publicado ainda: toque em 📤 para publicar no YouTube.")
	}
	return truncate(b.String(), maxCaption)
}

// failureText is the alert sent when a clip fails.
func failureText(c *domain.Clip, reason string) string {
	return truncate(fmt.Sprintf("⚠️ <b>Falha no clipe de %s</b>\nClipe: <code>%s</code>\nEtapa: %s\nMotivo: %s",
		esc(c.Streamer), esc(string(c.ID)), esc(string(c.FailedFrom)), esc(truncate(reason, 500))), 4000)
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// msgRef is one sent message.
type msgRef struct {
	chat int64
	msg  int
}

// encodeRefs joins message references as "chat:msg,chat:msg".
func encodeRefs(refs []msgRef) domain.MessageRef {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = strconv.FormatInt(r.chat, 10) + ":" + strconv.Itoa(r.msg)
	}
	return domain.MessageRef(strings.Join(parts, ","))
}

// decodeRefs parses the output of encodeRefs.
func decodeRefs(ref domain.MessageRef) ([]msgRef, error) {
	if ref == "" {
		return nil, nil
	}
	var out []msgRef
	for _, p := range strings.Split(string(ref), ",") {
		chat, msg, ok := strings.Cut(p, ":")
		c, err1 := strconv.ParseInt(chat, 10, 64)
		m, err2 := strconv.Atoi(msg)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid message ref %q", ref)
		}
		out = append(out, msgRef{chat: c, msg: m})
	}
	return out, nil
}
