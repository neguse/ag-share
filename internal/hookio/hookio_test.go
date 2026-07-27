package hookio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Input
		wantErr bool
	}{
		{
			name:  "UserPromptSubmit",
			input: `{"session_id":"s1","transcript_path":"session.jsonl","cwd":"/repo","prompt":"share-on","hook_event_name":"UserPromptSubmit"}`,
			want: Input{
				SessionID:      "s1",
				TranscriptPath: "session.jsonl",
				CWD:            "/repo",
				Prompt:         "share-on",
				HookEventName:  "UserPromptSubmit",
			},
		},
		{
			name:    "malformed",
			input:   `{"session_id":`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Read(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Read() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("Read() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLogf(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T) string
		wantEntry string
	}{
		{
			name: "writes log line",
			prepare: func(t *testing.T) string {
				return t.TempDir()
			},
			wantEntry: "request failed: 42",
		},
		{
			name: "swallows directory creation failure",
			prepare: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "occupied")
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.prepare(t)
			t.Setenv("AG_SHARE_HOME", base)
			Logf("request failed: %d", 42)
			if tt.wantEntry == "" {
				return
			}

			data, err := os.ReadFile(filepath.Join(base, "error.log"))
			if err != nil {
				t.Fatalf("read error.log: %v", err)
			}
			line := string(data)
			if !strings.Contains(line, tt.wantEntry) || !strings.HasSuffix(line, "\n") {
				t.Fatalf("error.log = %q, want timestamped line containing %q", line, tt.wantEntry)
			}
		})
	}
}
