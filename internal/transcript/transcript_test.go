package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neguse/ag-share/internal/backend"
)

func TestReadEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		wantLen int
		wantNil bool
	}{
		{
			name:    "fixture skips malformed line",
			path:    filepath.Join("testdata", "session.jsonl"),
			wantLen: 16,
		},
		{
			name:    "missing file",
			path:    filepath.Join(t.TempDir(), "missing.jsonl"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ReadEntries(tt.path)
			if err != nil {
				t.Fatalf("ReadEntries() error = %v", err)
			}
			if len(entries) != tt.wantLen {
				t.Fatalf("len(ReadEntries()) = %d, want %d", len(entries), tt.wantLen)
			}
			if tt.wantNil && entries != nil {
				t.Fatalf("ReadEntries() = %#v, want nil", entries)
			}
		})
	}
}

func TestReadEntriesLargeLine(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("x", 2*1024*1024)
	line, err := json.Marshal(map[string]any{
		"type": "user",
		"uuid": "large",
		"message": map[string]any{
			"content": want,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "large.jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(ReadEntries()) = %d, want 1", len(entries))
	}
	got, ok := entries[0].UserPromptText()
	if !ok || got != want {
		t.Fatalf("UserPromptText() matched = %v, len = %d; want true, %d", ok, len(got), len(want))
	}
}

func TestEntryFilters(t *testing.T) {
	t.Parallel()

	userTests := []struct {
		name  string
		entry Entry
		want  string
		ok    bool
	}{
		{
			name: "plain user prompt",
			entry: Entry{
				Type:    "user",
				Message: json.RawMessage(`{"content":"hello"}`),
			},
			want: "hello",
			ok:   true,
		},
		{
			name: "array tool result",
			entry: Entry{
				Type:    "user",
				Message: json.RawMessage(`{"content":[{"type":"tool_result"}]}`),
			},
		},
		{
			name: "meta",
			entry: Entry{
				Type:    "user",
				IsMeta:  true,
				Message: json.RawMessage(`{"content":"hidden"}`),
			},
		},
		{
			name: "compact summary",
			entry: Entry{
				Type:             "user",
				IsCompactSummary: true,
				Message:          json.RawMessage(`{"content":"hidden"}`),
			},
		},
		{
			name: "null is not a string",
			entry: Entry{
				Type:    "user",
				Message: json.RawMessage(`{"content":null}`),
			},
		},
	}
	for _, tt := range userTests {
		t.Run("user/"+tt.name, func(t *testing.T) {
			got, ok := tt.entry.UserPromptText()
			if got != tt.want || ok != tt.ok {
				t.Fatalf("UserPromptText() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}

	assistantTests := []struct {
		name      string
		entry     Entry
		wantTexts []string
		wantTools int
	}{
		{
			name: "texts tools and thinking",
			entry: Entry{
				Type:    "assistant",
				Message: json.RawMessage(`{"content":[{"type":"text","text":"one"},{"type":"thinking","thinking":"hidden"},{"type":"tool_use"},{"type":"text","text":"two"}]}`),
			},
			wantTexts: []string{"one", "two"},
			wantTools: 1,
		},
		{
			name: "non assistant",
			entry: Entry{
				Type:    "user",
				Message: json.RawMessage(`{"content":[{"type":"text","text":"hidden"}]}`),
			},
		},
	}
	for _, tt := range assistantTests {
		t.Run("assistant/"+tt.name, func(t *testing.T) {
			texts, tools := tt.entry.AssistantContent()
			if !reflect.DeepEqual(texts, tt.wantTexts) || tools != tt.wantTools {
				t.Fatalf("AssistantContent() = (%#v, %d), want (%#v, %d)", texts, tools, tt.wantTexts, tt.wantTools)
			}
		})
	}
}

func TestLatestUUIDAndExtractAfter(t *testing.T) {
	t.Parallel()

	entries, err := ReadEntries(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := LatestUUID(entries); got != "a6" {
		t.Fatalf("LatestUUID() = %q, want %q", got, "a6")
	}

	allTexts := []string{
		"Looking at the auth module first.",
		"Fixed: the token check used `==` on byte slices.\n\n```go\nif subtle.ConstantTimeCompare(a, b) == 1 {\n```",
		"Added TestTokenCompare; go test ./... passes.",
	}
	tests := []struct {
		name      string
		after     string
		wantTurn  backend.Turn
		wantLast  string
		wantFound bool
	}{
		{
			name:  "whole transcript",
			after: "",
			wantTurn: backend.Turn{
				UserPrompts: []string{"fix the login bug", "now add a test for it"},
				Texts:       allTexts,
				ToolCalls:   2,
			},
			wantLast:  "a6",
			wantFound: true,
		},
		{
			name:  "after a4",
			after: "a4",
			wantTurn: backend.Turn{
				UserPrompts: []string{"now add a test for it"},
				Texts:       []string{"Added TestTokenCompare; go test ./... passes."},
				ToolCalls:   1,
			},
			wantLast:  "a6",
			wantFound: true,
		},
		{
			name:      "missing cursor",
			after:     "missing-uuid",
			wantTurn:  backend.Turn{},
			wantLast:  "a6",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTurn, gotLast, gotFound := ExtractAfter(entries, tt.after)
			if !reflect.DeepEqual(gotTurn, tt.wantTurn) || gotLast != tt.wantLast || gotFound != tt.wantFound {
				t.Fatalf(
					"ExtractAfter(%q) = (%#v, %q, %v), want (%#v, %q, %v)",
					tt.after,
					gotTurn,
					gotLast,
					gotFound,
					tt.wantTurn,
					tt.wantLast,
					tt.wantFound,
				)
			}
		})
	}
}

func TestLatestTitle(t *testing.T) {
	t.Parallel()

	entries, err := ReadEntries(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatalf("ReadEntries() error = %v", err)
	}
	if got := LatestTitle(entries); got != "Login bug fix" {
		t.Errorf("LatestTitle(fixture) = %q, want %q", got, "Login bug fix")
	}

	if got := LatestTitle(nil); got != "" {
		t.Errorf("LatestTitle(nil) = %q, want empty", got)
	}
	multi := []Entry{
		{Type: "ai-title", AiTitle: "first"},
		{Type: "user", UUID: "u1"},
		{Type: "ai-title", AiTitle: "second"},
	}
	if got := LatestTitle(multi); got != "second" {
		t.Errorf("LatestTitle(multi) = %q, want %q", got, "second")
	}
}

func TestSplitAfter(t *testing.T) {
	t.Parallel()

	entries, err := ReadEntries(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatalf("ReadEntries() error = %v", err)
	}

	chunks, lastUUID, cursorFound := SplitAfter(entries, "")
	if !cursorFound || lastUUID != "a6" {
		t.Fatalf("SplitAfter() lastUUID=%q cursorFound=%v, want a6/true", lastUUID, cursorFound)
	}
	if len(chunks) != 2 {
		t.Fatalf("SplitAfter() chunks = %d, want 2", len(chunks))
	}
	c1, c2 := chunks[0], chunks[1]
	if len(c1.Turn.UserPrompts) != 1 || c1.Turn.UserPrompts[0] != "fix the login bug" {
		t.Errorf("chunk1 prompts = %q", c1.Turn.UserPrompts)
	}
	if len(c1.Turn.Texts) != 2 || c1.Turn.ToolCalls != 1 {
		t.Errorf("chunk1 texts=%d tools=%d, want 2/1", len(c1.Turn.Texts), c1.Turn.ToolCalls)
	}
	if c1.LastUUID != "cs1" {
		t.Errorf("chunk1 LastUUID = %q, want cs1 (cursor skips non-forwardable entries)", c1.LastUUID)
	}
	if len(c2.Turn.UserPrompts) != 1 || c2.Turn.UserPrompts[0] != "now add a test for it" {
		t.Errorf("chunk2 prompts = %q", c2.Turn.UserPrompts)
	}
	if len(c2.Turn.Texts) != 1 || c2.Turn.ToolCalls != 1 {
		t.Errorf("chunk2 texts=%d tools=%d, want 1/1", len(c2.Turn.Texts), c2.Turn.ToolCalls)
	}
	if c2.LastUUID != "a6" {
		t.Errorf("chunk2 LastUUID = %q, want a6", c2.LastUUID)
	}

	if _, _, found := SplitAfter(entries, "missing"); found {
		t.Error("SplitAfter(missing cursor) cursorFound = true, want false")
	}
}

func TestContainsFinalText(t *testing.T) {
	t.Parallel()

	entries, err := ReadEntries(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	finalText := "Added TestTokenCompare; go test ./... passes."

	tests := []struct {
		name  string
		after string
		text  string
		want  bool
	}{
		{name: "text after cursor", after: "a4", text: finalText, want: true},
		{name: "whole transcript", after: "", text: finalText, want: true},
		{name: "text only before cursor", after: "a6", text: "Looking at the auth module first.", want: false},
		{name: "text absent", after: "", text: "never said this", want: false},
		{name: "missing cursor stops waiting", after: "missing-uuid", text: "never said this", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsFinalText(entries, tt.after, tt.text); got != tt.want {
				t.Fatalf("containsFinalText(after=%q, text=%q) = %v, want %v", tt.after, tt.text, got, tt.want)
			}
		})
	}
}

func TestAwaitFinalText(t *testing.T) {
	t.Parallel()

	line := func(uuid, text string) string {
		return `{"type":"assistant","uuid":"` + uuid + `","message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}

	t.Run("returns once the text is appended", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(line("u1", "working on it")), 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			defer f.Close()
			_, _ = f.WriteString(line("u2", "the final answer"))
		}()

		start := time.Now()
		AwaitFinalText(path, "", "the final answer", 2*time.Second)
		elapsed := time.Since(start)
		if elapsed >= time.Second {
			t.Fatalf("AwaitFinalText waited %v, want well under the 2s timeout", elapsed)
		}
		entries, err := ReadEntries(path)
		if err != nil {
			t.Fatal(err)
		}
		if !containsFinalText(entries, "", "the final answer") {
			t.Fatal("AwaitFinalText returned before the final text was readable")
		}
	})

	t.Run("empty final text returns immediately", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		AwaitFinalText(filepath.Join(t.TempDir(), "missing.jsonl"), "", "", time.Second)
		if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
			t.Fatalf("AwaitFinalText(empty text) waited %v, want immediate return", elapsed)
		}
	})

	t.Run("gives up at the timeout", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(line("u1", "working on it")), 0o600); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		AwaitFinalText(path, "", "never arrives", 100*time.Millisecond)
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Fatalf("AwaitFinalText returned after %v, want at least the 100ms timeout", elapsed)
		}
	})
}
