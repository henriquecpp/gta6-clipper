package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// fakeAPI imitates POST /v1/messages and records the request bodies.
type fakeAPI struct {
	mu      sync.Mutex
	raw     []string
	bodies  []map[string]any
	replies []string // raw JSON message bodies, in order (last repeats)
	status  int
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authed := r.Header.Get("X-Api-Key") == "sk-test" || r.Header.Get("Authorization") == "Bearer sk-test"
		if r.URL.Path != "/v1/messages" || !authed || r.Header.Get("Anthropic-Version") == "" {
			t.Errorf("unexpected request %s %s %v", r.Method, r.URL.Path, r.Header)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.raw = append(f.raw, string(body))
		f.bodies = append(f.bodies, m)
		i := min(len(f.bodies)-1, len(f.replies)-1)
		status, reply := f.status, ""
		if i >= 0 {
			reply = f.replies[i]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		_, _ = io.WriteString(w, reply)
	}
}

func message(content string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":` + content +
		`,"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":6000,"output_tokens":900,"cache_creation_input_tokens":2000,"cache_read_input_tokens":150}}`
}

const goodInput = `{"start_s":40,"end_s":70.5,"headline":"Ele pulou da ponte!","captions":[{"start_s":41,"end_s":44,"text":"Vocês viram isso?"}],"post_text":"Insano! Clipe da live de streamer_a #gta6","youtube_title":"streamer_a pulou da ponte no GTA 6","search_tags":["gta 6 pulo da ponte","gta 6 bridge jump"],"reasoning":"setup e reação"}`

func request() domain.EditRequest {
	return domain.EditRequest{
		ClipID: "c1", Streamer: "streamer_a", Game: "GTA VI", StreamTitle: "dia 1", Language: "en", OutputLanguage: "pt-BR",
		Transcript: domain.Transcript{Language: "en", Words: []domain.Word{
			{Text: "no", Start: 40 * time.Second, End: 40400 * time.Millisecond},
			{Text: "way", Start: 40500 * time.Millisecond, End: 41 * time.Second},
		}},
		Signals: []domain.SignalPoint{{Pos: 0, Voice: 0.5, Chat: 1.25}, {Pos: domain.StreamPos(time.Second), Voice: 4, Chat: 3, Clips: 2}},
		Constraints: domain.EditConstraints{WindowDuration: 90 * time.Second, Peak: 60 * time.Second, MinDuration: 15 * time.Second,
			MaxDuration: 60 * time.Second, PeakTolerance: 5 * time.Second, HeadlineMaxChars: 60, Streamer: "streamer_a"},
	}
}

func newTestEditor(t *testing.T, f *fakeAPI, choice string) *Editor {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return New(Config{APIKey: "sk-test", Model: "claude-haiku-5-5", MaxTokens: 2000, Timeout: 10 * time.Second, ToolChoice: choice},
		option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client()))
}

func toolUseContent(id string) string {
	return `[{"type":"tool_use","id":"` + id + `","name":"submit_edit_plan","input":` + goodInput + `}]`
}

func TestPlanForcedToolChoice(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("toolu_1"))}}
	e := newTestEditor(t, f, ToolChoiceForced)
	p, usage, err := e.Plan(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if p.StartS != 40 || p.EndS != 70.5 || p.Headline != "Ele pulou da ponte!" || len(p.Captions) != 1 || p.ToolUseID != "toolu_1" ||
		p.YouTubeTitle != "streamer_a pulou da ponte no GTA 6" || len(p.SearchTags) != 2 {
		t.Fatalf("proposal = %+v", p)
	}
	if usage != (domain.Usage{InputTokens: 6000, OutputTokens: 900, CacheWriteTokens: 2000, CacheReadTokens: 150}) {
		t.Fatalf("usage = %+v", usage)
	}
	b := f.bodies[0]
	if b["model"] != "claude-haiku-5-5" || b["max_tokens"].(float64) != 2000 {
		t.Fatalf("model/max_tokens = %v %v", b["model"], b["max_tokens"])
	}
	system := b["system"].([]any)[0].(map[string]any)
	if system["cache_control"].(map[string]any)["type"] != "ephemeral" || !strings.Contains(system["text"].(string), "still frames") {
		t.Fatalf("system = %v", system["cache_control"])
	}
	choice := b["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "submit_edit_plan" {
		t.Fatalf("tool_choice = %v", choice)
	}
	toolDef := b["tools"].([]any)[0].(map[string]any)
	schema := toolDef["input_schema"].(map[string]any)
	if toolDef["name"] != "submit_edit_plan" || len(schema["required"].([]any)) != 8 || schema["additionalProperties"] != false {
		t.Fatalf("tool = %v", toolDef)
	}
	if _, strict := toolDef["strict"]; strict {
		t.Fatal("strict is only for auto tool_choice")
	}
	user := b["messages"].([]any)[0].(map[string]any)
	text := user["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"streamer: streamer_a", "peak: 60.00s", "40.00-40.40 no | 40.50-41.00 way", "1 4.0 3.0 2", "between 15.00s and 60.00s",
		"youtube_title: at most 85 characters", "search_tags: at most 12"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
}

func TestPlanAutoToolChoiceIsStrict(t *testing.T) {
	// The model thinks first: the answer starts with a thinking block.
	reply := message(`[{"type":"thinking","thinking":"the angle is...","signature":"sig"},` +
		`{"type":"tool_use","id":"t","name":"submit_edit_plan","input":` + goodInput + `}]`)
	f := &fakeAPI{replies: []string{reply}}
	e := newTestEditor(t, f, ToolChoiceAuto)
	p, _, err := e.Plan(context.Background(), request())
	if err != nil || p.Headline != "Ele pulou da ponte!" {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	if f.bodies[0]["tool_choice"].(map[string]any)["type"] != "auto" || f.bodies[0]["tools"].([]any)[0].(map[string]any)["strict"] != true {
		t.Fatalf("auto/strict not set: %v", f.bodies[0]["tool_choice"])
	}
	if _, set := f.bodies[0]["output_config"]; set {
		t.Fatalf("effort sent without being configured: %v", f.bodies[0]["output_config"])
	}
}

func TestPlanDefaultsToThinkingWithEffort(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("t"))}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	e := New(Config{APIKey: "sk-test", Model: "claude-haiku-5-5", MaxTokens: 2000, Effort: "high"},
		option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client()))
	if _, _, err := e.Plan(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	b := f.bodies[0]
	if b["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Fatalf("default tool_choice = %v, want auto (the model thinks before the tool call)", b["tool_choice"])
	}
	if oc, _ := b["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Fatalf("output_config = %v", b["output_config"])
	}
}

func TestPlanMalformedAnswers(t *testing.T) {
	cases := map[string]string{
		"text only":     `[{"type":"text","text":"Aqui está o corte..."}]`,
		"invalid json":  `[{"type":"tool_use","id":"t","name":"submit_edit_plan","input":{"start_s":"quarenta"}}]`,
		"missing times": `[{"type":"tool_use","id":"t","name":"submit_edit_plan","input":{"headline":"x"}}]`,
	}
	for name, content := range cases {
		f := &fakeAPI{replies: []string{message(content)}}
		e := newTestEditor(t, f, ToolChoiceForced)
		p, usage, err := e.Plan(context.Background(), request())
		if !errors.Is(err, domain.ErrInvalidEditPlan) || usage.InputTokens != 6000 {
			t.Errorf("%s: err = %v usage = %+v", name, err, usage)
		}
		if name == "text only" && p.RawText == "" {
			t.Errorf("raw text not kept")
		}
	}
}

func TestPlanRetryConversation(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("t2"))}}
	e := newTestEditor(t, f, ToolChoiceForced)
	req := request()
	req.Retry = &domain.EditRetry{Previous: domain.EditProposal{ToolUseID: "toolu_1", RawInput: `{"start_s":10}`}, Problem: "headline is empty"}
	if _, _, err := e.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	msgs := f.bodies[0]["messages"].([]any)
	// A fresh single turn: replaying the assistant tool call would need its
	// thinking blocks back unmodified.
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want one user turn", len(msgs))
	}
	text := asJSON(msgs[0])
	for _, want := range []string{"previous answer was rejected", `{\"start_s\":10}`, "Problem: headline is empty"} {
		if !strings.Contains(text, want) {
			t.Errorf("retry turn lacks %q: %s", want, text)
		}
	}

	// Previous answer was plain text: it is quoted.
	f2 := &fakeAPI{replies: f.replies}
	e2 := newTestEditor(t, f2, ToolChoiceForced)
	req.Retry = &domain.EditRetry{Previous: domain.EditProposal{RawText: "ok"}, Problem: "no tool call"}
	_, _, _ = e2.Plan(context.Background(), req)
	if msgs := f2.bodies[0]["messages"].([]any); len(msgs) != 1 || !strings.Contains(asJSON(msgs[0]), "Previous answer (text, no tool call): ok") {
		t.Fatalf("text retry = %v", msgs)
	}
	// Nothing to quote: only the problem.
	f3 := &fakeAPI{replies: f.replies}
	e3 := newTestEditor(t, f3, ToolChoiceForced)
	req.Retry = &domain.EditRetry{Problem: "empty"}
	_, _, _ = e3.Plan(context.Background(), req)
	if msgs := f3.bodies[0]["messages"].([]any); len(msgs) != 1 || !strings.Contains(asJSON(msgs[0]), "Problem: empty") {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestPlanAPIError(t *testing.T) {
	f := &fakeAPI{status: 529}
	e := newTestEditor(t, f, ToolChoiceForced)
	_, _, err := e.Plan(context.Background(), request())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 529 || !apiErr.Temporary() || !strings.Contains(apiErr.Error(), "529") {
		t.Fatalf("err = %v", err)
	}
	if (&APIError{Status: 400}).Temporary() {
		t.Fatal("400 is permanent")
	}
	if msg := userMessage(domain.EditRequest{}); !strings.Contains(msg, "(no speech detected)") || !strings.Contains(msg, "## Viewer clip titles\n(none)") {
		t.Fatalf("empty request message:\n%s", msg)
	}
	req := request()
	req.ViewerClipTitles = []string{"caiu do prédio kkkk", "fuga épica"}
	if msg := userMessage(req); !strings.Contains(msg, "## Viewer clip titles\n- caiu do prédio kkkk\n- fuga épica\n") {
		t.Fatalf("titles missing from the message:\n%s", msg)
	}
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestUserMessageGameTranscript(t *testing.T) {
	req := request()
	if strings.Contains(userMessage(req), "game's language") {
		t.Fatal("game transcript section without a game transcript")
	}
	w := func(text string, start, end float64) domain.Word {
		return domain.Word{Text: text, Start: time.Duration(start * float64(time.Second)), End: time.Duration(end * float64(time.Second))}
	}
	req.GameTranscript = domain.Transcript{Language: "en", Words: []domain.Word{
		w("Let's", 1, 1.2), w("make", 1.2, 1.4), w("some", 1.4, 1.6), w("money.", 1.6, 2), w("Love", 2.1, 2.3), w("the", 2.3, 2.4),
		w("hustle,", 2.4, 2.8), w("Lucia", 2.8, 3.2), w("Sit", 9, 9.2), w("down.", 9.2, 9.6),
	}}
	text := userMessage(req)
	for _, want := range []string{"## Same audio transcribed in the game's language (en)", "\n1.00 Let's make some money.\n",
		"\n2.10 Love the hustle, Lucia\n", "\n9.00 Sit down.\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
}

func TestPlanShowsFramesBeforeTheText(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("t"))}}
	e := newTestEditor(t, f, ToolChoiceForced)
	req := request()
	req.Frames = []domain.Frame{{At: 0, JPEG: []byte{0xff, 0xd8, 1}}, {At: 3 * time.Second, JPEG: []byte{0xff, 0xd8, 2}}}
	req.Retry = &domain.EditRetry{Problem: "empty"} // nothing to replay: the problem goes in the same turn
	if _, _, err := e.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	msgs := f.bodies[0]["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(msgs) != 1 || len(content) != 6 {
		t.Fatalf("messages = %d, blocks = %d: %s", len(msgs), len(content), asJSON(content))
	}
	block := func(i int) map[string]any { return content[i].(map[string]any) }
	if !strings.Contains(block(0)["text"].(string), "2 still frames of the window in order, one every 3.00s") ||
		block(1)["text"] != "0.00s" || block(3)["text"] != "3.00s" {
		t.Fatalf("frame labels = %s", asJSON(content))
	}
	src := block(4)["source"].(map[string]any)
	if block(4)["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != "/9gC" {
		t.Fatalf("image block = %v", block(4))
	}
	if last := block(5)["text"].(string); !strings.HasPrefix(last, "## Metadata") || !strings.Contains(last, "rejected") {
		t.Fatalf("text block = %s", last)
	}
}

func TestToolPropertiesKeepTheirOrder(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("t"))}}
	e := newTestEditor(t, f, ToolChoiceAuto)
	if _, _, err := e.Plan(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	// The cut comes before the captions, and the captions before the texts.
	raw := f.raw[0]
	props := raw[strings.Index(raw, `"properties":{"start_s"`):]
	last := -1
	for _, name := range []string{"start_s", "end_s", "captions", "headline", "youtube_title", "post_text", "search_tags", "reasoning"} {
		i := strings.Index(props, `"`+name+`":{`)
		if i <= last {
			t.Fatalf("property %s out of order in %s", name, props[:min(len(props), 400)])
		}
		last = i
	}
}
