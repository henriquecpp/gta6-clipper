// Package claude implements the Editor port with the official Anthropic Go
// SDK (github.com/anthropics/anthropic-sdk-go/v2). It is only called after a
// human approved the clip. The system prompt is stable and cached; the
// variable data goes in the user message; the answer comes through the
// submit_edit_plan tool.
package claude

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/henriquecpp/gta6-clipper/internal/app"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

//go:embed prompts/editor.md
var systemPrompt string

// ToolName is the tool the model must call.
const ToolName = "submit_edit_plan"

// Tool choice modes (docs/decisions/0005, 0015).
const (
	ToolChoiceForced = "tool"
	ToolChoiceAuto   = "auto"
)

// Config configures the editor. Model and prices come from configuration,
// never from code.
type Config struct {
	APIKey     string
	Model      string
	MaxTokens  int64
	Timeout    time.Duration
	ToolChoice string
}

// Editor implements app.Editor.
type Editor struct {
	cfg    Config
	client anthropic.Client
}

var _ app.Editor = (*Editor)(nil)

// New builds the editor. Extra request options (base URL, HTTP client) are
// for tests.
func New(cfg Config, opts ...option.RequestOption) *Editor {
	if cfg.ToolChoice == "" {
		cfg.ToolChoice = ToolChoiceForced
	}
	base := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		// Retries with backoff are done by the resilience decorator.
		option.WithMaxRetries(0),
	}
	if cfg.Timeout > 0 {
		base = append(base, option.WithRequestTimeout(cfg.Timeout))
	}
	return &Editor{cfg: cfg, client: anthropic.NewClient(append(base, opts...)...)}
}

// APIError is a failed Messages API call.
type APIError struct {
	Status int
	Err    error
}

func (e *APIError) Error() string { return fmt.Sprintf("claude api status %d: %v", e.Status, e.Err) }
func (e *APIError) Unwrap() error { return e.Err }

// Temporary reports whether a retry may succeed (rate limit, overload, 5xx).
func (e *APIError) Temporary() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 529 || e.Status >= 500
}

func tool(strict bool) anthropic.ToolParam {
	num := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	t := anthropic.ToolParam{
		Name:        ToolName,
		Description: anthropic.String("Submit the chosen cut, the pt-BR headline and captions, the suggested post text and the YouTube title and search tags."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"start_s":  num("Início do corte, em segundos relativos à janela"),
				"end_s":    num("Fim do corte, em segundos relativos à janela"),
				"headline": str("Manchete curta em PT-BR"),
				"captions": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":                 "object",
						"required":             []string{"start_s", "end_s", "text"},
						"additionalProperties": false,
						"properties": map[string]any{
							"start_s": map[string]any{"type": "number"},
							"end_s":   map[string]any{"type": "number"},
							"text":    str("Texto em PT-BR (traduzido se a fala for em outro idioma)"),
						},
					},
				},
				"post_text":     str("Texto sugerido do post, com crédito ao streamer"),
				"youtube_title": str("Título do Short no YouTube em PT-BR, pensado para busca: GTA 6, o streamer e o gancho"),
				"search_tags": map[string]any{
					"type":        "array",
					"description": "Frases de busca específicas deste momento (PT-BR e inglês), sem hashtags nem vírgulas",
					"items":       map[string]any{"type": "string"},
				},
				"reasoning": str("Uma frase explicando a escolha do corte (para depuração)"),
			},
			Required:    []string{"start_s", "end_s", "headline", "captions", "post_text", "youtube_title", "search_tags", "reasoning"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
	if strict {
		t.Strict = anthropic.Bool(true)
	}
	return t
}

// params builds the request (exported shape tested in editor_test.go).
func (e *Editor) params(req domain.EditRequest) anthropic.MessageNewParams {
	forced := e.cfg.ToolChoice != ToolChoiceAuto
	p := anthropic.MessageNewParams{
		Model:     e.cfg.Model,
		MaxTokens: e.cfg.MaxTokens,
		// Stable prefix (tools + system) is cached; only the user message varies.
		System: []anthropic.TextBlockParam{{Text: systemPrompt, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		Tools:  []anthropic.ToolUnionParam{{OfTool: ptr(tool(!forced))}},
	}
	if forced {
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: ToolName, DisableParallelToolUse: anthropic.Bool(true)}}
	} else {
		p.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(true)}}
	}
	p.Messages = []anthropic.MessageParam{anthropic.NewUserMessage(userContent(req, "")...)}
	if r := req.Retry; r != nil {
		problem := "Your previous answer was rejected: " + r.Problem + ". Fix every problem and call " + ToolName + " again."
		switch {
		case r.Previous.ToolUseID != "":
			input := json.RawMessage(r.Previous.RawInput)
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			p.Messages = append(p.Messages,
				anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(r.Previous.ToolUseID, input, ToolName)),
				anthropic.NewUserMessage(anthropic.NewToolResultBlock(r.Previous.ToolUseID, problem, true)))
		case strings.TrimSpace(r.Previous.RawText) != "":
			p.Messages = append(p.Messages,
				anthropic.NewAssistantMessage(anthropic.NewTextBlock(r.Previous.RawText)),
				anthropic.NewUserMessage(anthropic.NewTextBlock(problem)))
		default:
			p.Messages[0] = anthropic.NewUserMessage(userContent(req, "\n\n"+problem)...)
		}
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// userContent is the user turn: the frames first (images work best before
// the text), each preceded by its time, then the text of userMessage plus
// extra.
func userContent(req domain.EditRequest, extra string) []anthropic.ContentBlockParamUnion {
	var blocks []anthropic.ContentBlockParamUnion
	if len(req.Frames) > 0 {
		step := ""
		if len(req.Frames) > 1 {
			step = fmt.Sprintf(", one every %ss", f2(req.Frames[1].At-req.Frames[0].At))
		}
		blocks = append(blocks, anthropic.NewTextBlock(fmt.Sprintf(
			"## Frames\n%d still frames of the window in order%s, each after its time in seconds (same time base as the transcript).",
			len(req.Frames), step)))
		for _, f := range req.Frames {
			blocks = append(blocks, anthropic.NewTextBlock(f2(f.At)+"s"),
				anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(f.JPEG)))
		}
	}
	return append(blocks, anthropic.NewTextBlock(userMessage(req)+extra))
}

func f2(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) }

// userMessage renders the variable part of the request.
func userMessage(req domain.EditRequest) string {
	c := req.Constraints
	var b strings.Builder
	fmt.Fprintf(&b, "## Metadata\nstreamer: %s\ngame: %s\nstream title: %s\nspoken language: %s\ncaption language: %s\n\n",
		req.Streamer, orNone(req.Game), orNone(req.StreamTitle), orNone(req.Language), req.OutputLanguage)
	fmt.Fprintf(&b, "## Constraints\nwindow duration: %ss\npeak: %ss\ncut duration: between %ss and %ss\npeak tolerance: %ss\nheadline: at most %d characters\ncredit in post_text: %s\nyoutube_title: at most %d characters\nsearch_tags: at most %d, each up to %d characters\n\n",
		f2(c.WindowDuration), f2(c.Peak), f2(c.MinDuration), f2(c.MaxDuration), f2(c.PeakTolerance), c.HeadlineMaxChars, c.Streamer,
		domain.MaxYouTubeTitleChars, domain.MaxSearchTags, domain.MaxSearchTagChars)
	b.WriteString("## Transcript (start-end word)\n")
	if len(req.Transcript.Words) == 0 {
		b.WriteString("(no speech detected)\n")
	}
	for i, w := range req.Transcript.Words {
		fmt.Fprintf(&b, "%s-%s %s", f2(w.Start), f2(w.End), w.Text)
		if i%8 == 7 || i == len(req.Transcript.Words)-1 {
			b.WriteByte('\n')
		} else {
			b.WriteString(" | ")
		}
	}
	if g := req.GameTranscript; len(g.Words) > 0 {
		// Compact lines (start time + text): it only tells who is speaking.
		fmt.Fprintf(&b, "\n## Same audio transcribed in the game's language (%s), to spot game dialogue (start text)\n", orNone(g.Language))
		var line []string
		var lineStart time.Duration
		for i, w := range g.Words {
			if len(line) == 0 {
				lineStart = w.Start
			}
			line = append(line, w.Text)
			last := i == len(g.Words)-1
			gap := !last && g.Words[i+1].Start-w.End > 1500*time.Millisecond
			sentence := strings.HasSuffix(w.Text, ".") || strings.HasSuffix(w.Text, "?") || strings.HasSuffix(w.Text, "!")
			if last || gap || len(line) == 14 || sentence {
				fmt.Fprintf(&b, "%s %s\n", f2(lineStart), strings.Join(line, " "))
				line = line[:0]
			}
		}
	}
	b.WriteString("\n## Signals per second (second voice chat clips)\n")
	for _, s := range req.Signals {
		fmt.Fprintf(&b, "%d %.1f %.1f %.0f\n", int(time.Duration(s.Pos)/time.Second), s.Voice, s.Chat, s.Clips)
	}
	b.WriteString("\n## Viewer clip titles\n")
	if len(req.ViewerClipTitles) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range req.ViewerClipTitles {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

type toolInput struct {
	StartS   *float64 `json:"start_s"`
	EndS     *float64 `json:"end_s"`
	Headline string   `json:"headline"`
	Captions []struct {
		StartS float64 `json:"start_s"`
		EndS   float64 `json:"end_s"`
		Text   string  `json:"text"`
	} `json:"captions"`
	PostText     string   `json:"post_text"`
	YouTubeTitle string   `json:"youtube_title"`
	SearchTags   []string `json:"search_tags"`
	Reasoning    string   `json:"reasoning"`
}

// Plan calls the Messages API and decodes the tool call.
func (e *Editor) Plan(ctx context.Context, req domain.EditRequest) (domain.EditProposal, domain.Usage, error) {
	msg, err := e.client.Messages.New(ctx, e.params(req))
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return domain.EditProposal{}, domain.Usage{}, &APIError{Status: apiErr.StatusCode, Err: err}
		}
		return domain.EditProposal{}, domain.Usage{}, fmt.Errorf("claude messages: %w", err)
	}
	usage := domain.Usage{
		InputTokens:      msg.Usage.InputTokens,
		OutputTokens:     msg.Usage.OutputTokens,
		CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadTokens:  msg.Usage.CacheReadInputTokens,
	}
	var p domain.EditProposal
	var texts []string
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			if block.Name == ToolName && p.ToolUseID == "" {
				p.ToolUseID, p.RawInput = block.ID, string(block.Input)
			}
		}
	}
	p.RawText = strings.Join(texts, "\n")
	if p.ToolUseID == "" {
		return p, usage, fmt.Errorf("%w: the answer has no %s call (stop_reason %s)", domain.ErrInvalidEditPlan, ToolName, msg.StopReason)
	}
	var in toolInput
	if err := json.Unmarshal([]byte(p.RawInput), &in); err != nil {
		return p, usage, fmt.Errorf("%w: invalid tool input JSON (stop_reason %s): %w", domain.ErrInvalidEditPlan, msg.StopReason, err)
	}
	if in.StartS == nil || in.EndS == nil {
		return p, usage, fmt.Errorf("%w: start_s and end_s are required", domain.ErrInvalidEditPlan)
	}
	p.StartS, p.EndS = *in.StartS, *in.EndS
	p.Headline, p.PostText, p.Reasoning = in.Headline, in.PostText, in.Reasoning
	p.YouTubeTitle, p.SearchTags = in.YouTubeTitle, in.SearchTags
	for _, c := range in.Captions {
		p.Captions = append(p.Captions, domain.ProposedCaption{StartS: c.StartS, EndS: c.EndS, Text: c.Text})
	}
	return p, usage, nil
}
