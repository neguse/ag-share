// Package hookio handles the Claude Code hooks I/O contract: JSON on stdin,
// exit codes out, and the error log. Hooks must never interfere with the
// session, so logging is best-effort and never fails.
package hookio

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/neguse/ag-share/internal/config"
)

// Input is the hook payload common to UserPromptSubmit and Stop.
// Verified fields: session_id, transcript_path, cwd, hook_event_name on both;
// prompt on UserPromptSubmit only. turn_id is Codex-only (its turn-scoped
// hooks carry the completing turn's ID; Claude Code sends none).
// last_assistant_message is Claude Code Stop only: the turn's final response
// text, which the hooks reference guarantees even when the transcript file
// does not yet contain it.
type Input struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	CWD                  string `json:"cwd"`
	Prompt               string `json:"prompt"`
	HookEventName        string `json:"hook_event_name"`
	TurnID               string `json:"turn_id"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// Read decodes the hook input from r (stdin).
func Read(r io.Reader) (Input, error) {
	var input Input
	err := json.NewDecoder(r).Decode(&input)
	return input, err
}

// Logf appends one timestamped line to BaseDir()/error.log, creating the
// directory as needed. Error metadata only — never forwarded message content
// (the log must not become a second leak path). Best-effort: all failures are
// swallowed.
func Logf(format string, args ...any) {
	dir := config.BaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}

	f, err := os.OpenFile(filepath.Join(dir, "error.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
}
