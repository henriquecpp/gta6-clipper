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

const goodInput = `{"start_s":40,"end_s":70.5,"headline":"Ele pulou da ponte!","captions":[{"start_s":41,"end_s":44,"text":"Vocês viram isso?"}],"post_text":"Insano! Clipe da live de streamer_a #gta6","reasoning":"setup e reação"}`

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
	if p.StartS != 40 || p.EndS != 70.5 || p.Headline != "Ele pulou da ponte!" || len(p.Captions) != 1 || p.ToolUseID != "toolu_1" {
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
	if system["cache_control"].(map[string]any)["type"] != "ephemeral" || !strings.Contains(system["text"].(string), "You do NOT see the video") {
		t.Fatalf("system = %v", system["cache_control"])
	}
	choice := b["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "submit_edit_plan" {
		t.Fatalf("tool_choice = %v", choice)
	}
	toolDef := b["tools"].([]any)[0].(map[string]any)
	schema := toolDef["input_schema"].(map[string]any)
	if toolDef["name"] != "submit_edit_plan" || len(schema["required"].([]any)) != 6 || schema["additionalProperties"] != false {
		t.Fatalf("tool = %v", toolDef)
	}
	if _, strict := toolDef["strict"]; strict {
		t.Fatal("strict is only for auto tool_choice")
	}
	user := b["messages"].([]any)[0].(map[string]any)
	text := user["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"streamer: streamer_a", "peak: 60.00s", "40.00-40.40 no | 40.50-41.00 way", "1 4.0 3.0 2", "between 15.00s and 60.00s"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
}

func TestPlanAutoToolChoiceIsStrict(t *testing.T) {
	f := &fakeAPI{replies: []string{message(toolUseContent("t"))}}
	e := newTestEditor(t, f, ToolChoiceAuto)
	if _, _, err := e.Plan(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if f.bodies[0]["tool_choice"].(map[string]any)["type"] != "auto" || f.bodies[0]["tools"].([]any)[0].(map[string]any)["strict"] != true {
		t.Fatalf("auto/strict not set: %v", f.bodies[0]["tool_choice"])
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
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want user/assistant/user", len(msgs))
	}
	toolUse := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	result := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" || result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" || result["is_error"] != true {
		t.Fatalf("retry messages = %v / %v", toolUse, result)
	}
	if !strings.Contains(asJSON(result), "headline is empty") {
		t.Fatalf("problem not sent: %v", result)
	}

	// Previous answer was plain text: the text is replayed.
	f2 := &fakeAPI{replies: f.replies}
	e2 := newTestEditor(t, f2, ToolChoiceForced)
	req.Retry = &domain.EditRetry{Previous: domain.EditProposal{RawText: "ok"}, Problem: "no tool call"}
	_, _, _ = e2.Plan(context.Background(), req)
	if len(f2.bodies[0]["messages"].([]any)) != 3 {
		t.Fatal("text retry should replay the assistant text")
	}
	// Nothing to replay: the problem is appended to the user message.
	f3 := &fakeAPI{replies: f.replies}
	e3 := newTestEditor(t, f3, ToolChoiceForced)
	req.Retry = &domain.EditRetry{Problem: "empty"}
	_, _, _ = e3.Plan(context.Background(), req)
	if msgs := f3.bodies[0]["messages"].([]any); len(msgs) != 1 || !strings.Contains(asJSON(msgs[0]), "rejected") {
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
