package slack

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/neguse/ag-share/internal/backend"
)

type capturedRequest struct {
	Authorization string
	ContentType   string
	Payload       postMessageRequest
}

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	clone.Host = r.target.Host
	return r.base.RoundTrip(clone)
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		channel string
		wantErr string
	}{
		{name: "valid", token: "xoxb-token", channel: "C1"},
		{name: "missing token", channel: "C1", wantErr: "token"},
		{name: "missing channel", token: "xoxb-token", wantErr: "channel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.token, tt.channel)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.wantErr) {
					t.Fatalf("New() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got.HTTPClient == nil || got.HTTPClient.Timeout != 15*time.Second {
				t.Fatalf("New().HTTPClient = %#v, want 15s timeout", got.HTTPClient)
			}
		})
	}
}

func TestCreateThreadAndPostTurn(t *testing.T) {
	requests := make(chan capturedRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload postMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- capturedRequest{
			Authorization: r.Header.Get("Authorization"),
			ContentType:   r.Header.Get("Content-Type"),
			Payload:       payload,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"123.456"}`)
	}))
	defer server.Close()

	b := testBackend(t, server)
	threadRef, err := b.CreateThread(backend.SessionInfo{
		Repo: "github.com/acme/product",
		User: "alice",
		Host: "workstation",
	})
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if threadRef != "123.456" {
		t.Fatalf("CreateThread() = %q, want %q", threadRef, "123.456")
	}

	turn := backend.Turn{
		UserPrompts: []string{"first line\nsecond line", "next prompt"},
		Texts:       []string{"an answer", "```go\nfmt.Println(\"kept\")\n```"},
		ToolCalls:   2,
	}
	if err := b.PostTurn(threadRef, turn); err != nil {
		t.Fatalf("PostTurn() error = %v", err)
	}

	parent := <-requests
	reply := <-requests
	for _, got := range []capturedRequest{parent, reply} {
		if got.Authorization != "Bearer xoxb-token" {
			t.Errorf("Authorization = %q, want %q", got.Authorization, "Bearer xoxb-token")
		}
		if got.ContentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got.ContentType)
		}
		if got.Payload.Channel != "C1" {
			t.Errorf("channel = %q, want C1", got.Payload.Channel)
		}
	}
	if parent.Payload.Text != ":claude: github.com/acme/product — session by alice@workstation" {
		t.Errorf("parent text = %q", parent.Payload.Text)
	}
	if parent.Payload.ThreadTS != nil {
		t.Errorf("parent thread_ts = %q, want omitted", *parent.Payload.ThreadTS)
	}

	wantReply := "> first line\n> second line\n\n> next prompt\n\nan answer\n\n```go\nfmt.Println(\"kept\")\n```\n\n_(2 tool calls)_"
	if reply.Payload.Text != wantReply {
		t.Errorf("reply text = %q, want %q", reply.Payload.Text, wantReply)
	}
	if reply.Payload.ThreadTS == nil || *reply.Payload.ThreadTS != threadRef {
		t.Errorf("reply thread_ts = %v, want %q", reply.Payload.ThreadTS, threadRef)
	}
}

func TestSlackAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"error":"not_in_channel"}`)
	}))
	defer server.Close()

	b := testBackend(t, server)
	_, err := b.CreateThread(backend.SessionInfo{})
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("CreateThread() error = %v, want containing not_in_channel", err)
	}
}

func TestPostTurnTruncates(t *testing.T) {
	t.Parallel()

	requests := make(chan postMessageRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload postMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"123.456"}`)
	}))
	defer server.Close()

	b := testBackend(t, server)
	if err := b.PostTurn("123.456", backend.Turn{
		Texts: []string{strings.Repeat("x", maxMessageLen+1000)},
	}); err != nil {
		t.Fatalf("PostTurn() error = %v", err)
	}

	got := <-requests
	if len(got.Text) > maxMessageLen {
		t.Fatalf("len(text) = %d, want <= %d", len(got.Text), maxMessageLen)
	}
	if !strings.Contains(got.Text, "…(truncated)") {
		t.Fatalf("text does not contain truncation marker: %q", got.Text)
	}
}

func testBackend(t *testing.T, server *httptest.Server) *Backend {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("xoxb-token", "C1")
	if err != nil {
		t.Fatal(err)
	}
	b.HTTPClient = &http.Client{
		Transport: rewriteTransport{
			target: target,
			base:   server.Client().Transport,
		},
	}
	return b
}

func TestCreateThreadWithTopicAndUpdateThread(t *testing.T) {
	type updatePayload struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Text    string `json:"text"`
	}
	type call struct {
		Path    string
		Payload updatePayload
	}
	calls := make(chan call, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload updatePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		calls <- call{Path: r.URL.Path, Payload: payload}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"123.456"}`)
	}))
	defer server.Close()

	b := testBackend(t, server)
	info := backend.SessionInfo{
		Repo:  "github.com/acme/product",
		User:  "alice",
		Host:  "workstation",
		Topic: "fix the login bug",
	}
	threadRef, err := b.CreateThread(info)
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	created := <-calls
	wantText := ":claude: github.com/acme/product — fix the login bug (by alice@workstation)"
	if created.Path != "/api/chat.postMessage" {
		t.Errorf("create path = %q, want /api/chat.postMessage", created.Path)
	}
	if created.Payload.Text != wantText {
		t.Errorf("parent text = %q, want %q", created.Payload.Text, wantText)
	}

	info.Topic = "login bug: constant-time compare"
	if err := b.UpdateThread(threadRef, info); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}
	updated := <-calls
	if updated.Path != "/api/chat.update" {
		t.Errorf("update path = %q, want /api/chat.update", updated.Path)
	}
	if updated.Payload.TS != threadRef {
		t.Errorf("update ts = %q, want %q", updated.Payload.TS, threadRef)
	}
	wantText = ":claude: github.com/acme/product — login bug: constant-time compare (by alice@workstation)"
	if updated.Payload.Text != wantText {
		t.Errorf("update text = %q, want %q", updated.Payload.Text, wantText)
	}
	if updated.Payload.Channel != "C1" {
		t.Errorf("update channel = %q, want C1", updated.Payload.Channel)
	}
}
