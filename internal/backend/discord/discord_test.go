package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neguse/ag-share/internal/backend"
)

type call struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func testServer(t *testing.T, calls *[]call, status int, responses map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*calls = append(*calls, call{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if res, ok := responses[r.URL.Path]; ok {
			_, _ = io.WriteString(w, res)
		} else {
			_, _ = io.WriteString(w, `{}`)
		}
	}))
}

func newTestBackend(t *testing.T, server *httptest.Server) *Backend {
	t.Helper()
	b, err := New("bot-token", "C1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	b.BaseURL = server.URL
	b.HTTPClient = server.Client()
	return b
}

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := New("", "C1"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("New(no token) error = %v, want token error", err)
	}
	if _, err := New("tok", ""); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Errorf("New(no channel) error = %v, want channel error", err)
	}
}

func TestCreateThreadPostTurnUpdateThread(t *testing.T) {
	var calls []call
	server := testServer(t, &calls, http.StatusOK, map[string]string{
		"/channels/C1/messages": `{"id":"M1","channel_id":"C1"}`,
	})
	defer server.Close()
	b := newTestBackend(t, server)

	info := backend.SessionInfo{Agent: "codex", Repo: "github.com/acme/product", User: "alice", Host: "ws", Topic: "fix the login bug"}
	ref, err := b.CreateThread(info)
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if ref != "M1" {
		t.Fatalf("CreateThread() = %q, want M1 (thread id == parent message id)", ref)
	}

	if err := b.PostTurn(ref, backend.Turn{
		UserPrompts: []string{"line1\nline2"},
		Texts:       []string{"answer"},
		ToolCalls:   3,
	}); err != nil {
		t.Fatalf("PostTurn() error = %v", err)
	}

	info.Topic = "login bug: constant-time compare"
	if err := b.UpdateThread(ref, info); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4 (message, thread start, reply, rename)", len(calls))
	}
	parent, start, reply, rename := calls[0], calls[1], calls[2], calls[3]

	for _, c := range calls {
		if c.Auth != "Bot bot-token" {
			t.Errorf("Authorization = %q, want Bot bot-token", c.Auth)
		}
	}
	if parent.Method != "POST" || parent.Path != "/channels/C1/messages" {
		t.Errorf("parent call = %s %s", parent.Method, parent.Path)
	}
	if got := parent.Body["content"]; got != "🤖 [codex] github.com/acme/product — session by alice@ws" {
		t.Errorf("parent content = %q", got)
	}
	if start.Method != "POST" || start.Path != "/channels/C1/messages/M1/threads" {
		t.Errorf("thread start call = %s %s", start.Method, start.Path)
	}
	if got := start.Body["name"]; got != "fix the login bug" {
		t.Errorf("thread name = %q", got)
	}
	if reply.Method != "POST" || reply.Path != "/channels/M1/messages" {
		t.Errorf("reply call = %s %s", reply.Method, reply.Path)
	}
	wantReply := "> line1\n> line2\n\nanswer\n\n_(3 tool calls)_"
	if got := reply.Body["content"]; got != wantReply {
		t.Errorf("reply content = %q, want %q", got, wantReply)
	}
	if rename.Method != "PATCH" || rename.Path != "/channels/M1" {
		t.Errorf("rename call = %s %s", rename.Method, rename.Path)
	}
	if got := rename.Body["name"]; got != "login bug: constant-time compare" {
		t.Errorf("rename name = %q", got)
	}
}

func TestThreadNameFallbackAndLimit(t *testing.T) {
	t.Parallel()
	if got := threadName(backend.SessionInfo{}); got != "session" {
		t.Errorf("threadName(empty) = %q, want session", got)
	}
	long := strings.Repeat("あ", 150)
	got := threadName(backend.SessionInfo{Topic: long})
	if runes := []rune(got); len(runes) != maxThreadNameRunes {
		t.Errorf("threadName(long) runes = %d, want %d", len(runes), maxThreadNameRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("threadName(long) = %q, want … suffix", got)
	}
}

func TestAPIError(t *testing.T) {
	var calls []call
	server := testServer(t, &calls, http.StatusForbidden, map[string]string{
		"/channels/C1/messages": `{"message":"Missing Permissions","code":50013}`,
	})
	defer server.Close()
	b := newTestBackend(t, server)

	_, err := b.CreateThread(backend.SessionInfo{Repo: "r", User: "u", Host: "h"})
	if err == nil || !strings.Contains(err.Error(), "Missing Permissions") || !strings.Contains(err.Error(), "50013") {
		t.Errorf("CreateThread() error = %v, want Missing Permissions (code 50013)", err)
	}
}

func TestPostTurnTruncates(t *testing.T) {
	var calls []call
	server := testServer(t, &calls, http.StatusOK, nil)
	defer server.Close()
	b := newTestBackend(t, server)

	if err := b.PostTurn("M1", backend.Turn{Texts: []string{strings.Repeat("x", 5000)}}); err != nil {
		t.Fatalf("PostTurn() error = %v", err)
	}
	content, _ := calls[0].Body["content"].(string)
	if len(content) > maxMessageLen {
		t.Errorf("content length = %d, want <= %d", len(content), maxMessageLen)
	}
	if !strings.Contains(content, "…(truncated)") {
		t.Errorf("content missing truncation marker")
	}
}
