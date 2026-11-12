// Package telegramtest provides a fake Telegram Bot API server for tests of
// the Telegram adapters (like net/http/httptest for HTTP).
package telegramtest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Token is a syntactically valid bot token for tests.
const Token = "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// Call is one API request received by the server.
type Call struct {
	Method string
	// Params holds every scalar parameter as a string; objects as raw JSON.
	Params map[string]string
	// FileBytes is the size of uploaded files (multipart).
	FileBytes int
}

// Server is a fake Bot API.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []Call
	nextMsg int
	updates []json.RawMessage
	nextUpd int
	fail    map[string]string
}

// New starts a fake server; call Close when done.
func New() *Server {
	s := &Server{nextMsg: 100, nextUpd: 1, fail: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Fail makes a method answer {"ok":false} with the description.
func (s *Server) Fail(method, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = description
}

// PushUpdate queues an update (as JSON object without update_id) for getUpdates.
func (s *Server) PushUpdate(update map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update["update_id"] = s.nextUpd
	s.nextUpd++
	b, _ := json.Marshal(update)
	s.updates = append(s.updates, b)
}

// Calls returns the calls of a method (all calls when method is "").
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "bot"+Token {
		http.NotFound(w, r)
		return
	}
	call := Call{Method: parts[1], Params: map[string]string{}}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mt {
	case "multipart/form-data":
		if err := r.ParseMultipartForm(64 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				call.Params[k] = v[0]
			}
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					call.FileBytes += int(fh.Size)
				}
			}
		}
	default:
		body, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		if len(body) > 0 && json.Unmarshal(body, &m) == nil {
			for k, v := range m {
				var str string
				if json.Unmarshal(v, &str) == nil {
					call.Params[k] = str
				} else {
					call.Params[k] = string(v)
				}
			}
		}
	}

	s.mu.Lock()
	s.calls = append(s.calls, call)
	failDesc, failing := s.fail[call.Method]
	var result any
	switch call.Method {
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "first_name": "clipper", "username": "clipper_bot"}
	case "sendVideo", "sendMessage", "editMessageCaption":
		s.nextMsg++
		var chat int64
		_, _ = fmt.Sscan(call.Params["chat_id"], &chat)
		result = map[string]any{"message_id": s.nextMsg, "date": 0, "chat": map[string]any{"id": chat, "type": "private"}}
	case "getUpdates":
		ups := s.updates
		s.updates = nil
		s.mu.Unlock()
		if len(ups) == 0 {
			time.Sleep(20 * time.Millisecond) // a short "long poll"
			ups = []json.RawMessage{}
		}
		writeOK(w, ups)
		return
	default:
		result = true
	}
	s.mu.Unlock()
	if failing {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": failDesc})
		return
	}
	writeOK(w, result)
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}
